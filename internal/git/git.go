// Package git inspects agent worktrees (status, diffs, merges) and manages
// series worktrees: it can init a repo and make its initial commit, add and
// remove worktrees, and add .workspaces/ to .git/info/exclude. Merge checks
// write objects only to a temp quarantine (see quarantine).
package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

var (
	// ErrNotARepo means dir is not inside a git worktree.
	ErrNotARepo = errors.New("not a git repository")
	// ErrGitMissing means no git binary was found.
	ErrGitMissing = errors.New("git executable not found")
	// ErrTooOld means git predates 2.38 (merge-tree --write-tree).
	ErrTooOld = errors.New("git 2.38+ required")
	// ErrNoCommits means the worktree has no commits yet.
	ErrNoCommits = errors.New("worktree has no commits yet")
	// ErrCannotCompare means a side's objects are missing from the repo
	// running the merge (separate clones with divergent history).
	ErrCannotCompare = errors.New("peer objects missing from object store")
)

// Error is a failed git command.
type Error struct {
	Dir    string
	Args   []string
	Code   int // exit code, -1 when git never ran
	Stderr string
}

// Error describes the failed git command and what git printed.
func (e *Error) Error() string {
	msg := "git " + strings.Join(e.Args, " ") + " (" + e.Dir + ")"
	if e.Stderr != "" {
		msg += ": " + e.Stderr
	}
	return msg
}

const cmdTimeout = 30 * time.Second

// run executes git in dir. Stdout is returned even on failure (merge-tree
// exit 1 carries conflict data); err is nil only on exit 0.
func run(ctx context.Context, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, cmdTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "LC_ALL=C", "GIT_OPTIONAL_LOCKS=0")
	if q, ok := ctx.Value(quarantineKey{}).(map[string][]string); ok {
		cmd.Env = append(cmd.Env, q[dir]...)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			trimmed := strings.TrimSpace(stderr.String())
			if strings.Contains(trimmed, "not a git repository") {
				return stdout.String(), fmt.Errorf("%w in %s", ErrNotARepo, dir)
			}
			return stdout.String(), &Error{Dir: dir, Args: args, Code: exitErr.ExitCode(), Stderr: trimmed}
		}
		if errors.Is(err, exec.ErrNotFound) {
			return "", ErrGitMissing
		}
		return stdout.String(), err
	}
	return stdout.String(), nil
}

// CheckVersion requires git 2.38+ (merge-tree --write-tree).
func CheckVersion(ctx context.Context) error {
	out, err := run(ctx, ".", "--version")
	if err != nil {
		return err
	}
	// "git version 2.50.1 (Apple Git-155)"
	fields := strings.Fields(out)
	if len(fields) < 3 {
		return fmt.Errorf("%w: unparsable %q", ErrTooOld, out)
	}
	parts := strings.Split(fields[2], ".")
	if len(parts) < 2 {
		return fmt.Errorf("%w: unparsable %q", ErrTooOld, out)
	}
	major, err1 := strconv.Atoi(parts[0])
	minor, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil || major < 2 || (major == 2 && minor < 38) {
		return fmt.Errorf("%w: have %s", ErrTooOld, fields[2])
	}
	return nil
}

// FileStatus is one porcelain status entry.
type FileStatus struct {
	Path   string
	Status string // two-letter XY code
}

// noisyUntrackedDirs are never source, at any depth.
var noisyUntrackedDirs = map[string]bool{
	".workspaces": true, "node_modules": true, ".next": true,
	".venv": true, "venv": true, "__pycache__": true,
}

// noisyRootDirs are build output at the repo root, but common source
// package names deeper down (internal/build, cmd/dist).
var noisyRootDirs = map[string]bool{"dist": true, "build": true, "target": true, "vendor": true}

// isNoisyUntracked reports whether an untracked path is dependency or build
// output (say node_modules, or dist at the repo root) rather than source.
func isNoisyUntracked(path string) bool {
	segs := strings.Split(path, "/")
	if len(segs) > 1 && noisyRootDirs[segs[0]] {
		return true
	}
	for _, seg := range segs[:len(segs)-1] {
		if noisyUntrackedDirs[seg] {
			return true
		}
	}
	return false
}

// Status lists modified, staged, and untracked files, one path each.
// Untracked dependency and build folders are skipped even without a .gitignore.
func Status(ctx context.Context, dir string) ([]FileStatus, error) {
	out, err := run(ctx, dir, "status", "--porcelain=v1", "-z", "-uall")
	if err != nil {
		return nil, err
	}
	files := []FileStatus{}
	fields := strings.Split(out, "\x00")
	for i := 0; i < len(fields); i++ {
		f := fields[i]
		if f == "" {
			continue
		}
		if len(f) < 4 || f[2] != ' ' {
			return nil, fmt.Errorf("git status: unparsable entry %q", f)
		}
		xy, path := f[:2], f[3:]
		if xy[0] == 'R' || xy[0] == 'C' {
			i++ // skip the orig path; report the new one
		}
		if xy == "??" && isNoisyUntracked(path) {
			continue
		}
		files = append(files, FileStatus{Path: path, Status: xy})
	}
	return files, nil
}

// topLevel returns the root of dir's worktree. Callers run git there so every
// path stays relative to the root, even for an agent registered in a subfolder.
func topLevel(ctx context.Context, dir string) (string, error) {
	out, err := run(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// hasHead reports whether dir's repo has any commit.
func hasHead(ctx context.Context, dir string) (bool, error) {
	_, err := run(ctx, dir, "rev-parse", "--verify", "HEAD")
	if err == nil {
		return true, nil
	}
	var gerr *Error
	if errors.As(err, &gerr) && strings.Contains(gerr.Stderr, "Needed a single revision") {
		return false, nil
	}
	return false, err
}
