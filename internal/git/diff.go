package git

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// DefaultMaxDiffBytes caps get_diff output when the caller passes no limit.
const DefaultMaxDiffBytes = 65536

// DiffOptions controls Diff.
type DiffOptions struct {
	Paths            []string
	IncludeUntracked bool
	MaxBytes         int
	// Base diffs against this commit instead of HEAD, so work committed
	// since (say, on a series branch) shows too.
	Base string
}

// DiffResult is a capped unified diff. Files lists every file in the diff
// even when the text itself was truncated.
type DiffResult struct {
	Diff      string
	Files     []string
	Truncated bool
}

// Diff returns the worktree's uncommitted changes against HEAD (or opts.Base).
// dir may be any folder inside the worktree; paths are relative to its root.
func Diff(ctx context.Context, dir string, opts DiffOptions) (DiffResult, error) {
	dir, err := topLevel(ctx, dir)
	if err != nil {
		return DiffResult{}, err
	}
	maxBytes := opts.MaxBytes
	if maxBytes <= 0 {
		maxBytes = DefaultMaxDiffBytes
	}
	head, err := hasHead(ctx, dir)
	if err != nil {
		return DiffResult{}, err
	}
	if !head {
		return diffNoHead(ctx, dir, opts, maxBytes)
	}
	rev := "HEAD"
	if opts.Base != "" {
		rev = opts.Base
	}
	args := []string{"diff", "--no-color", "--no-ext-diff", "--no-textconv", rev, "--"}
	args = append(args, opts.Paths...)
	text, err := run(ctx, dir, args...)
	if err != nil {
		return DiffResult{}, err
	}
	names, err := run(ctx, dir, append([]string{"diff", "--name-only", rev, "--"}, opts.Paths...)...)
	if err != nil {
		return DiffResult{}, err
	}
	res := DiffResult{Files: []string{}}
	for _, n := range strings.Split(names, "\n") {
		if n = strings.TrimSpace(n); n != "" {
			res.Files = append(res.Files, n)
		}
	}
	var b strings.Builder
	b.WriteString(text)
	if opts.IncludeUntracked {
		untracked, err := untrackedFiles(ctx, dir, opts.Paths)
		if err != nil {
			return DiffResult{}, err
		}
		for _, p := range untracked {
			res.Files = append(res.Files, p)
			if res.Truncated {
				continue
			}
			// Per-file reads are budget-aware: never load more than the
			// remaining cap plus one probe byte.
			budget := maxBytes - b.Len() + 1
			if budget <= 1 {
				res.Truncated = true
				continue
			}
			entry, capped, err := synthesizeNew(dir, p, budget)
			if err != nil {
				continue // raced with a delete; names still list it
			}
			if capped || b.Len()+len(entry) > maxBytes {
				res.Truncated = true
				continue
			}
			b.WriteString(entry)
		}
	}
	res.Diff = capText(b.String(), maxBytes, &res.Truncated)
	return res, nil
}

// diffNoHead synthesizes every file as new: there is no HEAD to diff against.
func diffNoHead(ctx context.Context, dir string, opts DiffOptions, maxBytes int) (DiffResult, error) {
	res := DiffResult{Files: []string{}}
	tracked, err := run(ctx, dir, "ls-files", "-z")
	if err != nil {
		return DiffResult{}, err
	}
	seen := map[string]bool{}
	var b strings.Builder
	emit := func(p string) {
		if seen[p] || !matchFilter(p, opts.Paths) {
			return
		}
		seen[p] = true
		res.Files = append(res.Files, p)
		if res.Truncated {
			return
		}
		budget := maxBytes - b.Len() + 1
		if budget <= 1 {
			res.Truncated = true
			return
		}
		entry, capped, err := synthesizeNew(dir, p, budget)
		if err != nil {
			return
		}
		if capped || b.Len()+len(entry) > maxBytes {
			res.Truncated = true
			return
		}
		b.WriteString(entry)
	}
	for _, p := range strings.Split(tracked, "\x00") {
		if p != "" {
			emit(p)
		}
	}
	if opts.IncludeUntracked {
		untracked, err := untrackedFiles(ctx, dir, nil)
		if err != nil {
			return DiffResult{}, err
		}
		for _, p := range untracked {
			emit(p)
		}
	}
	res.Diff = capText(b.String(), maxBytes, &res.Truncated)
	return res, nil
}

// untrackedFiles lists untracked files in dir that match filter.
func untrackedFiles(ctx context.Context, dir string, filter []string) ([]string, error) {
	files, err := Status(ctx, dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, f := range files {
		if f.Status == "??" && matchFilter(f.Path, filter) {
			out = append(out, f.Path)
		}
	}
	return out, nil
}

// matchFilter reports whether path is one of the filter paths or inside one.
// An empty filter matches everything.
func matchFilter(path string, filter []string) bool {
	if len(filter) == 0 {
		return true
	}
	for _, f := range filter {
		f = strings.TrimSuffix(f, "/")
		if path == f || strings.HasPrefix(path, f+"/") {
			return true
		}
	}
	return false
}

// synthesizeNew renders an untracked file as a new-file unified diff,
// reading at most limit bytes. capped reports the file exceeded the limit.
func synthesizeNew(dir, path string, limit int) (string, bool, error) {
	// Never follow symlinks out of the worktree; skip non-regular files.
	st, err := os.Lstat(filepath.Join(dir, path))
	if err != nil {
		return "", false, err
	}
	if !st.Mode().IsRegular() {
		return "diff --git a/" + path + " b/" + path + "\n# not a regular file; content skipped\n", false, nil
	}
	f, err := os.Open(filepath.Join(dir, path))
	if err != nil {
		return "", false, err
	}
	defer f.Close()
	content, err := io.ReadAll(io.LimitReader(f, int64(limit)))
	if err != nil {
		return "", false, err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "diff --git a/%s b/%s\nnew file mode 100644\n", path, path)
	if bytes.IndexByte(content[:min(len(content), 8000)], 0) >= 0 {
		fmt.Fprintf(&b, "Binary files /dev/null and b/%s differ\n", path)
		return b.String(), false, nil
	}
	capped := len(content) == limit
	lines := strings.Split(string(content), "\n")
	newline := false
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
		newline = true
	}
	fmt.Fprintf(&b, "--- /dev/null\n+++ b/%s\n@@ -0,0 +1,%d @@\n", path, len(lines))
	for _, l := range lines {
		b.WriteString("+")
		b.WriteString(l)
		b.WriteString("\n")
	}
	if !newline && len(lines) > 0 {
		b.WriteString("\\ No newline at end of file\n")
	}
	return b.String(), capped, nil
}

// capText cuts text at the last newline so that it, plus the truncation
// marker, fits in maxBytes.
func capText(text string, maxBytes int, truncated *bool) string {
	if len(text) <= maxBytes {
		return text
	}
	*truncated = true
	// The marker's count is at most len(text), so this length bounds it.
	budget := max(maxBytes-len(fmt.Sprintf("\n[truncated %d bytes]", len(text))), 0)
	cut := strings.LastIndex(text[:budget], "\n")
	if cut < 0 {
		cut = budget
		for cut > 0 && !utf8.RuneStart(text[cut]) {
			cut--
		}
	}
	return text[:cut] + fmt.Sprintf("\n[truncated %d bytes]", len(text)-cut)
}
