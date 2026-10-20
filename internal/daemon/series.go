package daemon

// This file creates series worktrees: one per series, in .workspaces/
// under the main checkout (beside the repo for a bare clone), on its own
// branch, made when the human first approves the roadmap.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/search-3d/breakroom/internal/git"
	"github.com/search-3d/breakroom/internal/protocol"
	"github.com/search-3d/breakroom/internal/store"
)

// ErrWorktreeEscape reports a series worktree path outside the workspace or its parent.
var ErrWorktreeEscape = errors.New("series worktree path must sit under the workspace directory or its parent")

// seriesPathAllowed reports whether a series worktree path is under the main
// checkout's parent folder (and isn't the main checkout itself).
func seriesPathAllowed(main, path string) bool {
	parent := canonical(filepath.Dir(main))
	p := canonical(path)
	if p == canonical(main) {
		return false
	}
	if !strings.HasSuffix(parent, string(filepath.Separator)) {
		parent += string(filepath.Separator)
	}
	return strings.HasPrefix(p, parent)
}

// seriesPath is where a series' worktree goes: inside .workspaces/ under the
// main checkout, or beside a bare repo (whose folder holds no work tree).
func seriesPath(mainCheckout, name string, bare bool) string {
	if bare {
		return filepath.Join(filepath.Dir(mainCheckout), ".workspaces", name)
	}
	return filepath.Join(mainCheckout, ".workspaces", name)
}

// createSeries adds the series worktree and branch (from base, else the main
// checkout's HEAD), records them, moves the builder in, and returns the
// human's handoff. The workspace root is the main checkout, or the repo
// itself for a bare clone.
func (s *server) createSeries(ctx context.Context, bp *store.Blueprint, base, branchOverride, pathOverride string) (string, error) {
	main := bp.WorkspaceRoot
	bare := git.IsBare(ctx, main)
	path := seriesPath(main, bp.Name, bare)
	if pathOverride != "" {
		path = pathOverride
		if !filepath.IsAbs(path) {
			path = filepath.Join(main, path)
		}
	}
	path = filepath.Clean(path)
	if !seriesPathAllowed(main, path) {
		return "", fmt.Errorf("%w: %s must sit under %s", ErrWorktreeEscape, path, filepath.Dir(main))
	}
	branch := "breakroom/" + bp.Name
	if branchOverride != "" {
		branch = branchOverride
	}
	var commit string
	var initial int
	var err error
	if base != "" {
		commit, err = git.Resolve(ctx, main, base)
	} else {
		commit, initial, err = git.EnsureHead(ctx, main)
	}
	if err != nil {
		return "", fmt.Errorf("resolving the series base: %w", err)
	}
	// A bare repo has no work tree of its own to be dirty.
	dirty := false
	if !bare {
		if dirty, err = git.Dirty(ctx, main); err != nil {
			return "", err
		}
	}
	if err := git.WorktreeAdd(ctx, main, path, branch, commit); err != nil {
		return "", err
	}
	if err := s.store.SetSeries(ctx, bp.WorkspaceRoot, bp.Name, path, branch, commit); err != nil {
		return "", err
	}
	bp.WorktreePath, bp.Branch, bp.BaseRef = path, branch, commit
	if bp.Builder != "" {
		s.moveBuilderIn(ctx, bp)
	}
	return handoffSeries(bp, main, dirty, initial), nil
}

// moveBuilderIn re-points the builder at the series worktree. The path
// rides in the join reply and in every phase spec (specWhere), so no
// separate wakeup mail is sent before phase 1 is specced.
func (s *server) moveBuilderIn(ctx context.Context, bp *store.Blueprint) {
	if err := s.store.SetAgentWorktree(ctx, bp.WorkspaceRoot, bp.Builder, bp.WorktreePath); err != nil {
		s.log.Error("re-pointing builder failed", "builder", bp.Builder, "error", err)
	}
}

// handleBlueprintSeries retries series creation (e.g. after the branch or
// folder already existed), optionally with another branch or folder.
func (s *server) handleBlueprintSeries(w http.ResponseWriter, r *http.Request) {
	var req protocol.BlueprintSeriesRequest
	if err := decodeBody(r, &req); err != nil {
		badReq(w, err)
		return
	}
	if req.WorkspaceRoot == "" || req.Name == "" || req.Caller == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "workspace_root, name, and caller are required")
		return
	}
	ctx := r.Context()
	bp, err := s.store.GetBlueprint(ctx, req.WorkspaceRoot, req.Name)
	if err != nil {
		mapBlueprintErr(w, err)
		return
	}
	if req.Caller != bp.Architect {
		writeErr(w, http.StatusForbidden, "forbidden_role", "only the architect sets up the series")
		return
	}
	if !bp.Series {
		writeErr(w, http.StatusConflict, "bad_transition", req.Name+" is not a series (it was started with a builder)")
		return
	}
	if bp.WorktreePath != "" {
		writeErr(w, http.StatusConflict, "bad_transition", req.Name+" already has its worktree at "+bp.WorktreePath)
		return
	}
	base, err := s.approvedBase(ctx, bp)
	if err != nil {
		mapBlueprintErr(w, err)
		return
	}
	handoff, err := s.createSeries(ctx, bp, base, req.Branch, req.Path)
	if err != nil {
		if errors.Is(err, ErrWorktreeEscape) {
			writeErr(w, http.StatusBadRequest, "worktree_escape", err.Error())
			return
		}
		writeGitErr(w, err)
		return
	}
	writeJSON(w, map[string]any{"worktree": bp.WorktreePath, "branch": bp.Branch, "handoff": handoff})
}

// approvedBase is the base of the latest approved roadmap; specs can't start
// without one, so neither can a series.
func (s *server) approvedBase(ctx context.Context, bp *store.Blueprint) (string, error) {
	rms, err := s.store.ListRoadmaps(ctx, bp.WorkspaceRoot, bp.Name)
	if err != nil {
		return "", err
	}
	for i := len(rms) - 1; i >= 0; i-- {
		if rms[i].ApprovedAt != nil {
			return rms[i].Base, nil
		}
	}
	return "", fmt.Errorf("%w: approve the roadmap first", store.ErrRoadmapUnapproved)
}
