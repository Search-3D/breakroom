package git

// This file manages series worktrees: Breakroom adds one per series and
// removes it once the work has landed, never discarding uncommitted work.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrWorktreeDirty is returned when removing a worktree that still has
// uncommitted or untracked changes.
var ErrWorktreeDirty = errors.New("worktree has uncommitted changes")

// excludeWorkspaces ensures .workspaces/ is listed in dir's .git/info/exclude
// so series worktrees inside the workspace root never clutter git status.
func excludeWorkspaces(dir string) {
	excl := filepath.Join(dir, ".git", "info", "exclude")
	data, err := os.ReadFile(excl)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == ".workspaces/" || strings.TrimSpace(line) == ".workspaces" {
			return
		}
	}
	prefix := ""
	if len(data) > 0 && data[len(data)-1] != '\n' {
		prefix = "\n"
	}
	_ = os.WriteFile(excl, append(data, []byte(prefix+".workspaces/\n")...), 0o644)
}

// WorktreeAdd creates a worktree at path on a new branch started from base.
func WorktreeAdd(ctx context.Context, repoDir, path, branch, base string) error {
	excludeWorkspaces(repoDir)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	_, err := run(ctx, repoDir, "worktree", "add", "-b", branch, path, base)
	return err
}

// WorktreeRemove removes a clean worktree; its branch stays.
func WorktreeRemove(ctx context.Context, repoDir, path string) error {
	dirty, err := Dirty(ctx, path)
	if err != nil {
		return err
	}
	if dirty {
		return ErrWorktreeDirty
	}
	if _, err := run(ctx, repoDir, "worktree", "remove", path); err != nil {
		return err
	}
	if filepath.Base(filepath.Dir(path)) == ".workspaces" {
		_ = os.Remove(filepath.Dir(path))
	}
	return nil
}

// Head returns the commit dir has checked out.
func Head(ctx context.Context, dir string) (string, error) {
	out, err := run(ctx, dir, "rev-parse", "HEAD")
	return strings.TrimSpace(out), err
}

// EnsureHead returns the commit dir has checked out, initializing a git
// repository with basic config and an initial commit if dir is not yet a git
// repository or has no commits. initial is how many files that commit took
// (0 when dir already had a HEAD).
func EnsureHead(ctx context.Context, dir string) (head string, initial int, err error) {
	if _, statErr := os.Stat(filepath.Join(dir, ".git")); errors.Is(statErr, os.ErrNotExist) {
		if _, err := hasHead(ctx, dir); errors.Is(err, ErrNotARepo) {
			// "add -A" below would commit everything under dir.
			if home, _ := os.UserHomeDir(); filepath.Clean(dir) == filepath.Clean(home) || filepath.Clean(dir) == "/" {
				return "", 0, fmt.Errorf("refusing to create a git repository in %s: register from the project folder", dir)
			}
			if _, err := run(ctx, dir, "init"); err != nil {
				return "", 0, err
			}
			if _, err := run(ctx, dir, "config", "user.name"); err != nil {
				_, _ = run(ctx, dir, "config", "user.name", "Breakroom")
			}
			if _, err := run(ctx, dir, "config", "user.email"); err != nil {
				_, _ = run(ctx, dir, "config", "user.email", "breakroom@localhost")
			}
		}
	}
	excludeWorkspaces(dir)
	ok, err := hasHead(ctx, dir)
	if err != nil {
		return "", 0, err
	}
	if !ok {
		if IsBare(ctx, dir) {
			return "", 0, fmt.Errorf("bare repository %s has no commits: push a first commit, or name a base", dir)
		}
		if _, err := run(ctx, dir, append([]string{"add", "-A", "--", "."}, initialExcludes()...)...); err != nil {
			return "", 0, err
		}
		out, err := run(ctx, dir, "ls-files", "-z")
		if err != nil {
			return "", 0, err
		}
		initial = strings.Count(out, "\x00")
		if _, err := run(ctx, dir,
			"-c", "user.name=Breakroom",
			"-c", "user.email=breakroom@localhost",
			"commit", "--allow-empty", "-m", "Initial commit",
		); err != nil {
			return "", 0, err
		}
	}
	head, err = Head(ctx, dir)
	return head, initial, err
}

// initialSecrets are never swept into an automatic initial commit.
var initialSecrets = []string{".env", ".env.*", "*.pem", "*.key", "id_rsa*", "id_ed25519*", "id_ecdsa*"}

// initialExcludes are pathspecs keeping dependency/build folders (the ones
// isNoisyUntracked skips) and likely secrets out of the initial commit.
func initialExcludes() []string {
	var ps []string
	for d := range noisyUntrackedDirs {
		ps = append(ps, ":(exclude,glob)**/"+d+"/**")
	}
	for d := range noisyRootDirs {
		ps = append(ps, ":(exclude,glob)"+d+"/**")
	}
	for _, f := range initialSecrets {
		ps = append(ps, ":(exclude,glob)**/"+f)
	}
	return ps
}

// IsBare reports whether dir is a bare repository (no work tree).
func IsBare(ctx context.Context, dir string) bool {
	out, err := run(ctx, dir, "rev-parse", "--is-bare-repository")
	return err == nil && strings.TrimSpace(out) == "true"
}

// Resolve returns the commit a ref (branch, tag, sha) names in dir's repo.
func Resolve(ctx context.Context, dir, ref string) (string, error) {
	out, err := run(ctx, dir, "rev-parse", "--verify", ref+"^{commit}")
	return strings.TrimSpace(out), err
}

// IsAncestor reports whether commit a is reachable from b in dir's repo
// (a is merged into b).
func IsAncestor(ctx context.Context, dir, a, b string) (bool, error) {
	_, err := run(ctx, dir, "merge-base", "--is-ancestor", a, b)
	var gerr *Error
	if errors.As(err, &gerr) && gerr.Code == 1 {
		return false, nil
	}
	return err == nil, err
}

// Dirty reports whether dir has uncommitted or untracked changes.
func Dirty(ctx context.Context, dir string) (bool, error) {
	files, err := Status(ctx, dir)
	return len(files) > 0, err
}
