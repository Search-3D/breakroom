package git

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// emptyTree is git's well-known empty tree id, used as the merge base when
// the two sides share no history.
const emptyTree = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

// Conflict is one path both sides touch incompatibly. Ranges are
// "start-end" in that side's current file, or nil when unattributable
// (untracked on that side, or a non-content conflict).
type Conflict struct {
	Path        string
	OursLines   *string
	TheirsLines *string
}

// ConflictResult separates true conflicts from files both sides changed
// that still merge cleanly.
type ConflictResult struct {
	Conflicts   []Conflict
	OverlapOnly []string
}

// Conflicts runs git's own merge algorithm over the two worktrees'
// uncommitted states (materialized via stash-create) and reports conflicting
// paths with each side's changed line ranges.
func Conflicts(ctx context.Context, oursDir, theirsDir string) (ConflictResult, error) {
	empty := ConflictResult{Conflicts: []Conflict{}, OverlapOnly: []string{}}
	if err := CheckVersion(ctx); err != nil {
		return empty, err
	}
	oursDir, err := topLevel(ctx, oursDir)
	if err != nil {
		return empty, err
	}
	theirsDir, err = topLevel(ctx, theirsDir)
	if err != nil {
		return empty, err
	}
	ctx, cleanup, err := quarantine(ctx, oursDir, theirsDir)
	if err != nil {
		return empty, err
	}
	defer cleanup()
	ours, err := commitState(ctx, oursDir)
	if err != nil {
		return empty, err
	}
	theirs, err := commitState(ctx, theirsDir)
	if err != nil {
		return empty, err
	}
	// Runs in oursDir: same-repo worktrees share its object store. Separate
	// clones surface as ErrCannotCompare.
	paths, err := mergeTreePaths(ctx, oursDir, ours, theirs)
	if err != nil {
		return empty, err
	}
	base := mergeBaseOrEmpty(ctx, oursDir, ours, theirs)
	oursChanged, err := changedFiles(ctx, oursDir, base, ours)
	if err != nil {
		return empty, err
	}
	theirsChanged, err := changedFiles(ctx, oursDir, base, theirs)
	if err != nil {
		return empty, err
	}
	inConflict := map[string]bool{}
	for _, p := range paths {
		inConflict[p] = true
	}
	res := ConflictResult{Conflicts: []Conflict{}, OverlapOnly: []string{}}
	for p := range oursChanged {
		if theirsChanged[p] && !inConflict[p] {
			res.OverlapOnly = append(res.OverlapOnly, p)
		}
	}
	sort.Strings(res.OverlapOnly)
	for _, p := range paths {
		res.Conflicts = append(res.Conflicts, Conflict{
			Path:        p,
			OursLines:   changedRange(ctx, oursDir, base, ours, p),
			TheirsLines: changedRange(ctx, oursDir, base, theirs, p),
		})
	}
	if err := untrackedOverlay(ctx, oursDir, theirsDir, ours, theirs, oursChanged, theirsChanged, &res, base); err != nil {
		return empty, err
	}
	sort.Slice(res.Conflicts, func(i, j int) bool { return res.Conflicts[i].Path < res.Conflicts[j].Path })
	return res, nil
}

// commitState materializes a worktree's uncommitted state as a commit:
// stash-create when dirty, HEAD when clean.
func commitState(ctx context.Context, dir string) (string, error) {
	// Fallback identity: stash objects land in the quarantine, never the
	// repo, and users without git identity configured still get conflict checks.
	out, err := run(ctx, dir, "-c", "user.name=breakroom", "-c", "user.email=breakroom@localhost", "stash", "create")
	if err != nil {
		if head, herr := hasHead(ctx, dir); herr != nil {
			return "", herr
		} else if !head {
			return "", ErrNoCommits
		}
		return "", err
	}
	if sha := strings.TrimSpace(out); sha != "" {
		return sha, nil
	}
	head, err := run(ctx, dir, "rev-parse", "--verify", "HEAD")
	if err != nil {
		if clean, herr := hasHead(ctx, dir); herr != nil {
			return "", herr
		} else if !clean {
			return "", ErrNoCommits
		}
		return "", err
	}
	return strings.TrimSpace(head), nil
}

type quarantineKey struct{}

// quarantine routes object writes (stash-create commits, merge-tree trees)
// into a temp dir deleted by cleanup; each dir's real store is a read-only
// alternate. Same mechanism as receive-pack's incoming quarantine.
func quarantine(ctx context.Context, dirs ...string) (context.Context, func(), error) {
	tmp, err := os.MkdirTemp("", "breakroom-objects-")
	if err != nil {
		return ctx, func() {}, err
	}
	cleanup := func() { os.RemoveAll(tmp) }
	env := map[string][]string{}
	for _, d := range dirs {
		out, err := run(ctx, d, "rev-parse", "--path-format=absolute", "--git-path", "objects")
		if err != nil {
			cleanup()
			return ctx, func() {}, err
		}
		env[d] = []string{"GIT_OBJECT_DIRECTORY=" + tmp, "GIT_ALTERNATE_OBJECT_DIRECTORIES=" + strings.TrimSpace(out)}
	}
	return context.WithValue(ctx, quarantineKey{}, env), cleanup, nil
}

// mergeTreePaths returns conflicted paths via merge-tree exit 1 + NUL fields:
// tree, paths..., empty separator, messages.
func mergeTreePaths(ctx context.Context, dir, ours, theirs string) ([]string, error) {
	out, err := run(ctx, dir, "merge-tree", "--write-tree", "--name-only", "-z",
		"--allow-unrelated-histories", ours, theirs)
	if isUsage(err) {
		// Ancient git without the flag; retry plain (unrelated histories
		// then fail below, surfaced raw).
		out, err = run(ctx, dir, "merge-tree", "--write-tree", "--name-only", "-z", ours, theirs)
	}
	if err != nil {
		if isMissingObjects(err) {
			return nil, fmt.Errorf("%w: %w", ErrCannotCompare, err)
		}
		var gerr *Error
		if !errors.As(err, &gerr) || gerr.Code != 1 {
			return nil, err
		}
		if strings.TrimSpace(out) == "" {
			return nil, err // exit 1 with no merge data: unmergeable input
		}
	}
	fields := strings.Split(out, "\x00")
	if len(fields) == 0 || !isHex(fields[0]) {
		return nil, fmt.Errorf("git merge-tree: unparsable output %q", out)
	}
	var paths []string
	for _, f := range fields[1:] {
		if f == "" {
			break // empty separator before messages
		}
		paths = append(paths, f)
	}
	return paths, nil
}

// isUsage reports whether git rejected the command line (exit code 129).
func isUsage(err error) bool {
	var gerr *Error
	return errors.As(err, &gerr) && gerr.Code == 129
}

// isMissingObjects reports whether git failed because a commit or object
// isn't in this repository.
func isMissingObjects(err error) bool {
	var gerr *Error
	if !errors.As(err, &gerr) {
		return false
	}
	for _, s := range []string{"bad object", "unknown revision", "Not a valid object", "not something we can merge"} {
		if strings.Contains(gerr.Stderr, s) {
			return true
		}
	}
	return false
}

// isHex reports whether s looks like a full SHA-1 or SHA-256 object id.
func isHex(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}

// mergeBaseOrEmpty returns the merge base of two commits, or the empty tree
// when they share no history.
func mergeBaseOrEmpty(ctx context.Context, dir, ours, theirs string) string {
	out, err := run(ctx, dir, "merge-base", ours, theirs)
	if err != nil {
		return emptyTree
	}
	return strings.TrimSpace(out)
}

// changedFiles returns the set of paths that differ between base and side.
func changedFiles(ctx context.Context, dir, base, side string) (map[string]bool, error) {
	out, err := run(ctx, dir, "diff", "--name-only", "-z", base, side, "--")
	if err != nil {
		return nil, err
	}
	files := map[string]bool{}
	for _, f := range strings.Split(out, "\x00") {
		if f != "" {
			files[f] = true
		}
	}
	return files, nil
}

// changedRange unions a side's -U0 hunks for path into "start-end" (new-file
// coordinates), or nil when the side has no content hunks there.
func changedRange(ctx context.Context, dir, base, side, path string) *string {
	out, err := run(ctx, dir, "diff", "-U0", "--no-color", "--no-ext-diff", base, side, "--", path)
	if err != nil {
		return nil
	}
	minStart, maxEnd := -1, -1
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "@@ ") {
			continue
		}
		plus := strings.Index(line, "+")
		if plus < 0 {
			continue
		}
		rest := line[plus+1:]
		if i := strings.Index(rest, " @@"); i >= 0 {
			rest = rest[:i]
		}
		start, count := rest, "1"
		if i := strings.Index(rest, ","); i >= 0 {
			start, count = rest[:i], rest[i+1:]
		}
		s, err1 := strconv.Atoi(start)
		n, err2 := strconv.Atoi(count)
		if err1 != nil || err2 != nil {
			continue
		}
		end := s
		if n > 0 {
			end = s + n - 1
		}
		if minStart < 0 || s < minStart {
			minStart = s
		}
		if maxEnd < 0 || end > maxEnd {
			maxEnd = end
		}
	}
	if minStart < 0 {
		return nil
	}
	var r string
	if minStart == maxEnd {
		r = strconv.Itoa(minStart)
	} else {
		r = strconv.Itoa(minStart) + "-" + strconv.Itoa(maxEnd)
	}
	return &r
}

// untrackedOverlay adds conflicts merge-tree can't see, because untracked
// files are in no commit: the same new file on both sides, or one side's new
// file colliding with the other side's changes.
func untrackedOverlay(ctx context.Context, oursDir, theirsDir, ours, theirs string, oursChanged, theirsChanged map[string]bool, res *ConflictResult, base string) error {
	oursUntracked, err := untrackedSet(ctx, oursDir)
	if err != nil {
		return err
	}
	theirsUntracked, err := untrackedSet(ctx, theirsDir)
	if err != nil {
		return err
	}
	known := map[string]bool{}
	for _, c := range res.Conflicts {
		known[c.Path] = true
	}
	addOverlap := func(p string) {
		for _, o := range res.OverlapOnly {
			if o == p {
				return
			}
		}
		res.OverlapOnly = append(res.OverlapOnly, p)
	}
	for p := range oursUntracked {
		if !theirsUntracked[p] || known[p] {
			continue
		}
		if sameBytes(oursDir, theirsDir, p) {
			addOverlap(p)
			continue
		}
		res.Conflicts = append(res.Conflicts, Conflict{Path: p})
		known[p] = true
	}
	for p := range oursUntracked {
		if known[p] || !theirsChanged[p] {
			continue
		}
		res.Conflicts = append(res.Conflicts, Conflict{Path: p, TheirsLines: changedRange(ctx, oursDir, base, theirs, p)})
		known[p] = true
	}
	for p := range theirsUntracked {
		if known[p] || !oursChanged[p] {
			continue
		}
		res.Conflicts = append(res.Conflicts, Conflict{Path: p, OursLines: changedRange(ctx, oursDir, base, ours, p)})
		known[p] = true
	}
	sort.Strings(res.OverlapOnly)
	return nil
}

// untrackedSet returns the untracked paths in dir.
func untrackedSet(ctx context.Context, dir string) (map[string]bool, error) {
	files, err := Status(ctx, dir)
	if err != nil {
		return nil, err
	}
	set := map[string]bool{}
	for _, f := range files {
		if f.Status == "??" {
			set[f.Path] = true
		}
	}
	return set, nil
}

// sameBytes compares two files by streaming hash. Read failures count as
// different (conservative: flagged, not silently merged).
func sameBytes(oursDir, theirsDir, path string) bool {
	a, err1 := hashFile(filepath.Join(oursDir, path))
	b, err2 := hashFile(filepath.Join(theirsDir, path))
	return err1 == nil && err2 == nil && a == b
}

// hashFile returns the raw SHA-256 digest of a file's contents (compared, never shown).
func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return string(h.Sum(nil)), nil
}
