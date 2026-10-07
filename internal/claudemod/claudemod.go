// Package claudemod installs the sessionhub Claude Code mod: the files under mod/,
// embedded in the binary, written to paths.ModDir() with the sessionhub binary's
// path filled in, and listed in env.CLAUDE_CODE_PLUGIN_DIRS of Claude Code's
// settings.json. See docs/cli.md, "sessionhub install-mod".
package claudemod

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/abdallah/session-hub/internal/hooks"
	"github.com/abdallah/session-hub/internal/paths"
)

// The mod's files, by name: the tests, the type declarations Claude Code
// generates, and the development files stay out of the binary. The mod's own
// $.state contract (types/index.d.ts), which plugin.json names, goes in.
//
//go:embed mod/.claude-plugin/plugin.json mod/hooks/config.js mod/hooks/hooks.json mod/hooks/register.js mod/hooks/pane.jsx mod/types/index.d.ts
var embedded embed.FS

// configFile holds the sessionhub binary's path; placeholder is what install
// replaces there, quotes included.
const (
	configFile  = "hooks/config.js"
	placeholder = `"__SESSIONHUB_BIN__"`
)

// Files returns the mod's files as install writes them, by path relative to
// the mod directory, with bin (the absolute sessionhub path) in hooks/config.js.
func Files(bin string) (map[string][]byte, error) {
	root, err := fs.Sub(embedded, "mod")
	if err != nil {
		return nil, err
	}
	out := map[string][]byte{}
	err = fs.WalkDir(root, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(root, p)
		if err != nil {
			return err
		}
		out[p] = b
		return nil
	})
	if err != nil {
		return nil, err
	}
	cfg := out[configFile]
	if n := bytes.Count(cfg, []byte(placeholder)); n != 1 {
		return nil, fmt.Errorf("%s: found the placeholder %d times, want 1", configFile, n)
	}
	var q bytes.Buffer
	enc := json.NewEncoder(&q)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(bin); err != nil {
		return nil, err
	}
	out[configFile] = bytes.Replace(cfg, []byte(placeholder), bytes.TrimRight(q.Bytes(), "\n"), 1)
	return out, nil
}

type opts struct {
	settings string
	binary   string
	dir      string
	out      io.Writer
	now      time.Time
}

func parseOpts(name string, args []string) (opts, error) {
	o := opts{settings: hooks.SettingsPath(), binary: paths.Binary(), dir: paths.ModDir(), out: os.Stdout, now: time.Now()}
	f := flag.NewFlagSet(name, flag.ContinueOnError)
	f.SetOutput(io.Discard)
	f.StringVar(&o.settings, "settings", o.settings, "settings file to edit")
	f.StringVar(&o.binary, "binary", o.binary, "absolute path of the sessionhub binary")
	if err := f.Parse(args); err != nil {
		return o, fmt.Errorf("%s: %w", name, err)
	}
	if f.NArg() > 0 {
		return o, fmt.Errorf("%s: unexpected argument %q", name, f.Arg(0))
	}
	if !filepath.IsAbs(o.binary) {
		return o, fmt.Errorf("%s: --binary must be an absolute path", name)
	}
	return o, nil
}

// RunInstall implements `sessionhub install-mod [--settings FILE] [--binary PATH]`.
func RunInstall(ctx context.Context, args []string) error {
	o, err := parseOpts("install-mod", args)
	if err != nil {
		return err
	}
	if err := install(o); err != nil {
		return fmt.Errorf("install-mod: %w", err)
	}
	return nil
}

// RunUninstall implements `sessionhub uninstall-mod [--settings FILE]`.
func RunUninstall(ctx context.Context, args []string) error {
	o, err := parseOpts("uninstall-mod", args)
	if err != nil {
		return err
	}
	if err := uninstall(o); err != nil {
		return fmt.Errorf("uninstall-mod: %w", err)
	}
	return nil
}

// install writes the mod to o.dir and lists that directory in the settings.
// The directory is replaced as a whole: the files go to a new directory next
// to it, which takes its place with a rename. When the settings edit fails
// after that, the previous directory is put back (or the new one removed),
// so a failed install leaves things as they were.
func install(o opts) (err error) {
	st, err := os.Stat(o.binary)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("sessionhub is not installed at %s; install it there first (see the README's Install)", o.binary)
	case err != nil:
		return err
	case st.IsDir():
		return fmt.Errorf("%s is a directory, not the sessionhub binary; install it there first (see the README's Install)", o.binary)
	}
	files, err := Files(o.binary)
	if err != nil {
		return err
	}
	if !sameFiles(o.dir, files) {
		restore, rerr := replaceDir(o.dir, files)
		if rerr != nil {
			return rerr
		}
		defer func() {
			if rerr := restore(err != nil); rerr != nil {
				if err == nil {
					// The new mod is in place; only the old copy is left over.
					fmt.Fprintf(o.out, "warning: %v\n", rerr)
					return
				}
				err = fmt.Errorf("%w (and restoring the previous mod failed: %v)", err, rerr)
			} else if err != nil {
				fmt.Fprintf(o.out, "restored the previous %s\n", o.dir)
			}
		}()
		fmt.Fprintf(o.out, "wrote the sessionhub Claude Code mod to %s\n", o.dir)
	} else {
		fmt.Fprintf(o.out, "the sessionhub Claude Code mod in %s is up to date\n", o.dir)
	}
	changed, backup, err := hooks.EditSettings(o.settings, o.now, func(src []byte) ([]byte, error) {
		return hooks.AddPluginDir(src, o.dir)
	})
	if err != nil {
		return err
	}
	switch {
	case !changed:
		fmt.Fprintf(o.out, "%s already lists it in env.%s\n", o.settings, hooks.PluginDirsKey)
	case backup != "":
		fmt.Fprintf(o.out, "added it to env.%s in %s (backup: %s)\n", hooks.PluginDirsKey, o.settings, backup)
	default:
		fmt.Fprintf(o.out, "added it to env.%s in %s\n", hooks.PluginDirsKey, o.settings)
	}
	fmt.Fprintln(o.out, "Claude Code sessions started from now on load the mod.")
	return nil
}

// uninstall removes the mod's directory from the settings, then the
// directory.
func uninstall(o opts) error {
	changed, backup, err := hooks.EditSettings(o.settings, o.now, func(src []byte) ([]byte, error) {
		return hooks.RemovePluginDir(src, o.dir)
	})
	if err != nil {
		return err
	}
	if changed {
		fmt.Fprintf(o.out, "removed %s from env.%s in %s (backup: %s)\n", o.dir, hooks.PluginDirsKey, o.settings, backup)
	} else {
		fmt.Fprintf(o.out, "%s does not list %s\n", o.settings, o.dir)
	}
	if _, err := os.Stat(o.dir); err == nil {
		if err := os.RemoveAll(o.dir); err != nil {
			return err
		}
		fmt.Fprintf(o.out, "removed %s\n", o.dir)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// sameFiles reports whether dir holds exactly files, byte for byte.
func sameFiles(dir string, files map[string][]byte) bool {
	seen := 0
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		want, ok := files[filepath.ToSlash(rel)]
		if !ok {
			return errors.New("extra file")
		}
		got, err := os.ReadFile(p)
		if err != nil || !bytes.Equal(got, want) {
			return errors.New("changed")
		}
		seen++
		return nil
	})
	return err == nil && seen == len(files)
}

// replaceDir writes files to a new directory next to dir and renames it into
// dir's place, keeping the previous dir aside. The returned restore puts the
// previous dir back when undo is true, else deletes the kept copy.
func replaceDir(dir string, files map[string][]byte) (restore func(undo bool) error, err error) {
	parent := filepath.Dir(dir)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return nil, err
	}
	stage, err := os.MkdirTemp(parent, ".claude-mod-new-*")
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			os.RemoveAll(stage)
		}
	}()
	if err := os.Chmod(stage, 0o755); err != nil {
		return nil, err
	}
	for rel, b := range files {
		p := filepath.Join(stage, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(p, b, 0o644); err != nil {
			return nil, err
		}
		if err := os.Chmod(p, 0o644); err != nil { // whatever the umask
			return nil, err
		}
	}
	// Keep the previous dir inside a fresh temp dir, so its name never
	// collides.
	aside, err := os.MkdirTemp(parent, ".claude-mod-old-*")
	if err != nil {
		return nil, err
	}
	old := filepath.Join(aside, "mod")
	hadOld := true
	if err := os.Rename(dir, old); errors.Is(err, fs.ErrNotExist) {
		hadOld = false
	} else if err != nil {
		os.RemoveAll(aside)
		return nil, err
	}
	if err := os.Rename(stage, dir); err != nil {
		if hadOld {
			if rerr := os.Rename(old, dir); rerr != nil {
				return nil, fmt.Errorf("%w (and putting back %s failed: %v; it is in %s)", err, dir, rerr, old)
			}
		}
		os.RemoveAll(aside)
		return nil, err
	}
	return func(undo bool) error {
		if !undo {
			return os.RemoveAll(aside)
		}
		if err := os.RemoveAll(dir); err != nil {
			return err
		}
		if hadOld {
			if err := os.Rename(old, dir); err != nil {
				return fmt.Errorf("%w; the previous mod is in %s", err, old)
			}
		}
		return os.RemoveAll(aside)
	}, nil
}
