package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/abdallah/session-hub/internal/paths"
)

// herdrRunner runs the herdr CLI and returns combined output. Tests replace it.
var herdrRunner = defaultHerdrRunner

func defaultHerdrRunner(ctx context.Context, args ...string) ([]byte, error) {
	bin := os.Getenv("HERDR_BIN_PATH")
	if bin == "" {
		bin = "herdr"
	}
	out, err := exec.CommandContext(ctx, bin, args...).CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("herdr %s: %w: %s", strings.Join(args, " "), err, bytes.TrimSpace(out))
	}
	return out, nil
}

// executable is os.Executable; tests replace it. It picks the binary the
// watcher is started from, never the one the manifest names.
var executable = os.Executable

// installedBinary returns paths.Binary() (~/.local/bin/sessionhub), or an error
// when nothing is installed there. The manifest names the installed binary,
// like the hook entries and the MCP registration, not the running one: a
// `./bin/sessionhub install-plugin` from a checkout must not link herdr to a build
// directory that `make clean` removes.
func installedBinary() (string, error) {
	bin := paths.Binary()
	st, err := os.Stat(bin)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "", fmt.Errorf("sessionhub is not installed at %s; run `make install` first", bin)
	case err != nil:
		return "", err
	case st.IsDir():
		return "", fmt.Errorf("%s is a directory, not the sessionhub binary; run `make install` first", bin)
	}
	return bin, nil
}

// linkedPlugin is the part of `herdr plugin list --json` that install reads.
type linkedPlugin struct {
	PluginID     string `json:"plugin_id"`
	ManifestPath string `json:"manifest_path"`
	PluginRoot   string `json:"plugin_root"`
}

func listHub(ctx context.Context) (*linkedPlugin, error) {
	out, err := herdrRunner(ctx, "plugin", "list", "--json", "--plugin", PluginID)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Result struct {
			Plugins []linkedPlugin `json:"plugins"`
		} `json:"result"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out), &resp); err != nil {
		return nil, fmt.Errorf("herdr plugin list: %w", err)
	}
	for _, p := range resp.Result.Plugins {
		if p.PluginID == PluginID {
			return &p, nil
		}
	}
	return nil, nil
}

// installManifest installs the manifest for the installed binary. It writes
// nothing and calls no herdr command when that binary is missing.
func installManifest(ctx context.Context, w io.Writer) (linked bool, err error) {
	bin, err := installedBinary()
	if err != nil {
		return false, err
	}
	return install(ctx, bin, w)
}

// install writes the manifest and links it. It relinks only when the plugin
// is not linked yet, is linked from another directory, or the manifest
// changed (herdr reads the manifest at link time). It reports whether it
// linked.
//
// When it wrote a new manifest and then fails (listing, unlinking, or
// linking), it puts the previous manifest back, or removes the file when
// there was none. Otherwise the next run would find the new manifest on disk,
// see nothing changed, and report "already linked" while herdr still runs the
// manifest it read at the last successful link.
func install(ctx context.Context, bin string, w io.Writer) (linked bool, err error) {
	dir := paths.PluginDir()
	manifest := []byte(Manifest(bin))
	path := filepath.Join(dir, ManifestFile)
	old, rerr := os.ReadFile(path)
	hadOld := rerr == nil
	changed := !hadOld || !bytes.Equal(old, manifest)
	if changed {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return false, err
		}
		if err := writeFileAtomic(path, manifest, 0o644); err != nil {
			return false, err
		}
		fmt.Fprintf(w, "wrote %s\n", path)
		defer func() {
			if err == nil {
				return
			}
			var rerr error
			if hadOld {
				rerr = writeFileAtomic(path, old, 0o644)
			} else {
				rerr = os.Remove(path)
			}
			if rerr != nil {
				err = fmt.Errorf("%w (and restoring the previous manifest failed: %v)", err, rerr)
			} else {
				fmt.Fprintf(w, "restored the previous %s\n", path)
			}
		}()
	}
	cur, err := listHub(ctx)
	if err != nil {
		return false, err
	}
	if cur != nil && !changed && filepath.Clean(cur.PluginRoot) == filepath.Clean(dir) {
		fmt.Fprintf(w, "herdr plugin %q already linked from %s\n", PluginID, dir)
		return false, nil
	}
	if cur != nil {
		if _, err := herdrRunner(ctx, "plugin", "unlink", PluginID); err != nil {
			return false, err
		}
	}
	if _, err := herdrRunner(ctx, "plugin", "link", dir); err != nil {
		return false, err
	}
	fmt.Fprintf(w, "linked herdr plugin %q from %s\n", PluginID, dir)
	return true, nil
}

// uninstall unlinks the plugin (when linked) and removes the plugin dir.
func uninstall(ctx context.Context, w io.Writer) error {
	cur, err := listHub(ctx)
	if err != nil {
		return err
	}
	if cur != nil {
		if _, err := herdrRunner(ctx, "plugin", "unlink", PluginID); err != nil {
			return err
		}
		fmt.Fprintf(w, "unlinked herdr plugin %q\n", PluginID)
	}
	dir := paths.PluginDir()
	if _, err := os.Stat(dir); err == nil {
		if err := os.RemoveAll(dir); err != nil {
			return err
		}
		fmt.Fprintf(w, "removed %s\n", dir)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
