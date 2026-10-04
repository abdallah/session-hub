package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/abdallah/session-hub/internal/store"
)

// Run is `sessionhub server`: it serves the API on every listen address until
// SIGINT or SIGTERM.
func Run(ctx context.Context, args []string) error {
	return run(ctx, args, os.Stderr)
}

func run(ctx context.Context, args []string, stderr io.Writer) error {
	return runWithListeners(ctx, args, stderr, nil)
}

// runWithListeners is run with an optional seam for tests: if preopened is
// non-empty, run serves on those already-bound listeners instead of binding
// the configured listen addresses. Tests use it to avoid the race between
// releasing a free port and rebinding it. Production code passes nil. The caller owns closing
// preopened listeners if run fails before serving.
func runWithListeners(ctx context.Context, args []string, stderr io.Writer, preopened []net.Listener) error {
	if len(args) > 0 {
		return fmt.Errorf("server: unexpected arguments %q; configure with server.toml or SESSIONHUB_* env", args)
	}
	cfg, err := LoadConfig()
	if err != nil {
		return fmt.Errorf("server: %w", err)
	}
	st, err := store.Open(cfg.DB, store.Options{StaleAfter: cfg.StaleAfter})
	if err != nil {
		return fmt.Errorf("server: %w", err)
	}
	defer st.Close()

	logger := log.New(stderr, "", log.LstdFlags)
	if cfg.LegacyReadToken {
		logger.Printf("warning: read_token (server.toml) and SESSIONHUB_READ_TOKEN are ignored: browsers sign in with `sessionhub login`. Delete the setting once you no longer need to roll back.")
	}
	for _, w := range cfg.Warnings {
		logger.Printf("warning: %s", w)
	}
	sessionhub := New(st, cfg.PublicURL, logger)
	sessionhub.SetMoveDir(filepath.Join(filepath.Dir(cfg.DB), "moves"))
	srv := &http.Server{
		Handler:           sessionhub.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second, // the long poll extends its own deadline
		IdleTimeout:       2 * time.Minute,
		ErrorLog:          logger,
	}
	// Shutdown waits for active requests; an open long poll would hold it
	// for up to 30 s. StopPolls answers them with 204 first.
	srv.RegisterOnShutdown(sessionhub.StopPolls)

	// Bind every address before serving any, so a bad address fails fast.
	lns := preopened
	for _, addr := range cfg.Listen {
		if len(preopened) > 0 {
			break // the caller already bound the listeners
		}
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			for _, l := range lns {
				l.Close()
			}
			return fmt.Errorf("server: listen %s: %w", addr, err)
		}
		lns = append(lns, ln)
	}

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	// Deferred after st.Close, so it runs first: the notifier stops before
	// the database closes, on every way out.
	stopAlerts, onRule := startAlerts(ctx, st, cfg, logger)
	sessionhub.ruleAdded = onRule
	defer stopAlerts()
	sweepCtx, stopSweep := context.WithCancel(ctx)
	sweepDone := make(chan struct{})
	go func() {
		defer close(sweepDone)
		sessionhub.runMoveSweeper(sweepCtx)
	}()
	// Runs before st.Close: the sweeper stops before the database closes.
	defer func() {
		stopSweep()
		<-sweepDone
	}()
	errc := make(chan error, len(lns))
	for _, ln := range lns {
		logger.Printf("sessionhub server listening on %s (db %s, stale_after %s)", ln.Addr(), cfg.DB, cfg.StaleAfter)
		go func(ln net.Listener) { errc <- srv.Serve(ln) }(ln)
	}

	var serveErr error
	select {
	case <-ctx.Done():
		logger.Printf("sessionhub server shutting down")
	case serveErr = <-errc:
	}
	shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutCtx); err != nil {
		return fmt.Errorf("server: shutdown: %w", err)
	}
	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		return fmt.Errorf("server: %w", serveErr)
	}
	return nil
}
