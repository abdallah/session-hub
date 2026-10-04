package move

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// archiveKeep is how long ~/.local/state/sessionhub/moved/<id> folders stay.
const archiveKeep = 30 * 24 * time.Hour

// PushBranch makes sure the branch's commits are on origin, the remote the
// target clones from. With an upstream on origin it pushes HEAD to that
// branch when HEAD is ahead of it. With no upstream it runs
// git push -u origin <branch>. With an upstream on another remote (a fork's
// upstream, or "." for a local branch) it pushes <branch> to origin without
// -u, so the user's tracking stays as it is. It never forces.
func PushBranch(ctx context.Context, r Repo) error {
	remote, _ := git(ctx, r.Root, "config", "--get", "branch."+r.Branch+".remote")
	merge, _ := git(ctx, r.Root, "config", "--get", "branch."+r.Branch+".merge")
	ref := "refs/heads/" + r.Branch
	net := gitOpts{timeout: gitNetTimeout}
	switch {
	case remote == "" || merge == "":
		_, err := gitRaw(ctx, r.Root, net, "push", "-u", "origin", ref+":"+ref)
		return err
	case remote != "origin":
		_, err := gitRaw(ctx, r.Root, net, "push", "origin", ref+":"+ref)
		return err
	}
	if n, err := git(ctx, r.Root, "rev-list", "--count", "@{u}..HEAD"); err == nil && n == "0" {
		return nil
	}
	_, err := gitRaw(ctx, r.Root, net, "push", "origin", "HEAD:"+merge)
	return err
}

// ClaudeVersion is the first field of `claude --version`, for example
// "2.1.285".
func ClaudeVersion(ctx context.Context, bin string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "--version").Output()
	if err != nil {
		return "", fmt.Errorf("claude --version: %w", err)
	}
	f := strings.Fields(string(out))
	if len(f) == 0 {
		return "", errors.New("claude --version printed nothing")
	}
	return f[0], nil
}

// Archive moves session id's transcript, sidecar folder, and file history
// out of claudeDir into <stateDir>/moved/<moveID>/, so `claude --resume` no
// longer finds the session here, and removes archive folders older than
// 30 days. A session with nothing left to move is not an error.
//
// The transcript moves last: the file history first, then each sidecar
// folder, then its .jsonl, and a failure stops before the .jsonl. So after a
// partial failure the transcript is still in place, and a retry finds the
// session and moves what is left.
func Archive(claudeDir, stateDir, sessionID, moveID string, now time.Time) error {
	base := filepath.Join(stateDir, "moved")
	dest := filepath.Join(base, moveID)
	ts, err := FindTranscripts(claudeDir, sessionID)
	if err != nil {
		return err
	}
	fh := filepath.Join(claudeDir, "file-history", sessionID)
	if _, err := os.Stat(fh); err == nil {
		if err := os.MkdirAll(filepath.Join(dest, "file-history"), 0o700); err != nil {
			return err
		}
		if err := os.Rename(fh, filepath.Join(dest, "file-history", sessionID)); err != nil {
			return err
		}
	}
	var errs []error
	for _, t := range ts {
		into := filepath.Join(dest, "transcript", filepath.Base(filepath.Dir(t)))
		if err := os.MkdirAll(into, 0o700); err != nil {
			return err
		}
		side := strings.TrimSuffix(t, ".jsonl")
		if _, err := os.Stat(side); err == nil {
			if err := os.Rename(side, filepath.Join(into, sessionID)); err != nil {
				errs = append(errs, err)
				continue
			}
		}
		errs = append(errs, os.Rename(t, filepath.Join(into, sessionID+".jsonl")))
	}
	errs = append(errs, pruneArchive(base, now))
	return errors.Join(errs...)
}

// pruneArchive removes archive folders older than archiveKeep.
func pruneArchive(base string, now time.Time) error {
	entries, err := os.ReadDir(base)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var errs []error
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || !e.IsDir() || now.Sub(info.ModTime()) <= archiveKeep {
			continue
		}
		errs = append(errs, os.RemoveAll(filepath.Join(base, e.Name())))
	}
	return errors.Join(errs...)
}
