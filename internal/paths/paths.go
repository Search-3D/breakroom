// Package paths resolves Breakroom home-directory locations.
package paths

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Dir returns the Breakroom home directory (~/.breakroom).
// BREAKROOM_HOME overrides it (used by tests).
func Dir() string {
	if h := os.Getenv("BREAKROOM_HOME"); h != "" {
		return h
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ".breakroom"
	}
	return filepath.Join(home, ".breakroom")
}

// ResolveWorkspace returns the workspace for a folder: $BREAKROOM_WORKSPACE if
// set, else the repo root shared by all its worktrees, else the folder itself.
// The result is canonical, so different spellings of a path match.
func ResolveWorkspace(cwd string) string {
	if w := os.Getenv("BREAKROOM_WORKSPACE"); w != "" {
		return canonicalKey(w)
	}
	if root := GitCommonRoot(cwd); root != "" {
		return root
	}
	return canonicalKey(cwd)
}

// CurrentWorkspace resolves the workspace for the current working directory,
// or returns an error if Getwd fails.
func CurrentWorkspace() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return ResolveWorkspace(cwd), nil
}

// canonicalKey makes p absolute with symlinks resolved, so different spellings
// of one folder compare equal.
func canonicalKey(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	return abs
}

// GitCommonRoot returns the key shared by all of a repo's worktrees (via
// git's common dir), or "" when outside a repo or git is unavailable.
func GitCommonRoot(cwd string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--git-common-dir")
	cmd.Dir = cwd
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	common := strings.TrimSpace(string(out))
	if !filepath.IsAbs(common) {
		common = filepath.Join(cwd, common)
	}
	if filepath.Base(common) != ".git" {
		// Bare repo (or separate git dir): no checkout root to name, so the
		// common dir itself is the key every linked worktree shares.
		return canonicalKey(common)
	}
	return canonicalKey(filepath.Dir(common))
}

// Socket is the daemon's Unix socket path.
func Socket() string { return filepath.Join(Dir(), "breakroom.sock") }

// Pid is the daemon's pidfile path (also the instance lock).
func Pid() string { return filepath.Join(Dir(), "breakroom.pid") }

// DB is the SQLite state database path.
func DB() string { return filepath.Join(Dir(), "state.db") }

// Log is the daemon log path.
func Log() string { return filepath.Join(Dir(), "daemon.log") }

// Config is the user config path.
func Config() string { return filepath.Join(Dir(), "config.json") }

// Token is the daemon's TCP auth token path.
func Token() string { return filepath.Join(Dir(), "token") }
