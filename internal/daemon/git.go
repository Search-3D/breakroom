package daemon

import (
	"context"
	"errors"
	"net/http"
	"os"

	"github.com/search-3d/breakroom/internal/git"
	"github.com/search-3d/breakroom/internal/llm"
	"github.com/search-3d/breakroom/internal/protocol"
	"github.com/search-3d/breakroom/internal/store"
)

// refreshFiles loads git status for an update. Missing or non-repo worktrees
// silently keep stored files (messaging-only use is legitimate); other git
// failures surface a warning code.
func refreshFiles(ctx context.Context, worktree string) ([]store.ModifiedFile, string) {
	if _, err := os.Stat(worktree); err != nil {
		return nil, ""
	}
	statuses, err := git.Status(ctx, worktree)
	if err != nil {
		if errors.Is(err, git.ErrNotARepo) {
			return nil, ""
		}
		return nil, "git_error"
	}
	files := make([]store.ModifiedFile, 0, len(statuses))
	for _, f := range statuses {
		files = append(files, store.ModifiedFile{Path: f.Path, Status: f.Status})
	}
	return files, ""
}

// diffAttachment renders the sender's diff for paths, capped for attachments.
func diffAttachment(ctx context.Context, worktree string, paths []string) (string, error) {
	res, err := git.Diff(ctx, worktree, git.DiffOptions{Paths: paths, MaxBytes: store.MaxDiffBytes})
	if err != nil {
		return "", err
	}
	return res.Diff, nil
}

// resolveLLM loads a fresh config (global + the workspace's project overlay)
// and resolves the API key env var, so config edits apply without a daemon
// restart.
func resolveLLM(workspace string) (llm.Config, error) {
	cfg, err := loadConfigFor(workspace)
	if err != nil {
		return llm.Config{}, err
	}
	c := llm.Config{
		EndpointURL:       llm.NormalizeEndpointURL(cfg.EndpointURL),
		Model:             cfg.Model,
		SummarizeMaxBytes: cfg.SummarizeMaxBytes,
		ReasoningEffort:   cfg.ReasoningEffort,
		ExtraBody:         cfg.ExtraBody,
	}
	if cfg.APIKeyEnv != "" {
		c.APIKey = os.Getenv(cfg.APIKeyEnv)
	}
	if !c.Valid() {
		return llm.Config{}, llm.ErrNotConfigured
	}
	return c, nil
}

// handleCompact returns a digest of recent workspace mail, made by the LLM
// when one is configured, else the raw thread.
func (s *server) handleCompact(w http.ResponseWriter, r *http.Request) {
	var req protocol.CompactRequest
	if err := decodeBody(r, &req); err != nil {
		badReq(w, err)
		return
	}
	if req.WorkspaceRoot == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "workspace_root is required")
		return
	}
	limit := min(max(req.Limit, 1), 200)
	if req.Limit <= 0 {
		limit = 50
	}
	msgs, err := s.store.ListThread(r.Context(), req.WorkspaceRoot, limit)
	if err != nil {
		storeErr(w, err)
		return
	}
	if len(msgs) == 0 {
		writeJSON(w, map[string]any{"digest": "", "message_count": 0})
		return
	}
	thread := make([]llm.ThreadMessage, 0, len(msgs))
	for i := len(msgs) - 1; i >= 0; i-- {
		thread = append(thread, llm.ThreadMessage{
			From: msgs[i].From, To: msgs[i].To, Type: msgs[i].Type, Content: msgs[i].Content,
		})
	}
	cfg, err := resolveLLM(req.WorkspaceRoot)
	if errors.Is(err, llm.ErrNotConfigured) {
		// No sidecar: hand back the capped transcript; the caller's own
		// model writes the brief.
		writeJSON(w, map[string]any{
			"digest": llm.Transcript(thread), "message_count": len(msgs), "fallback": true,
		})
		return
	}
	if err != nil {
		badReq(w, err)
		return
	}
	digest, err := llm.CompactThread(r.Context(), cfg, thread)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "llm_error", err.Error())
		return
	}
	writeJSON(w, map[string]any{"digest": digest, "message_count": len(msgs)})
}

// handleDiff returns an agent's uncommitted diff, with an LLM summary when
// asked and configured.
func (s *server) handleDiff(w http.ResponseWriter, r *http.Request) {
	var req protocol.DiffRequest
	if err := decodeBody(r, &req); err != nil {
		badReq(w, err)
		return
	}
	if req.WorkspaceRoot == "" || req.AgentID == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "workspace_root and agent_id are required")
		return
	}
	a, err := s.store.GetAgent(r.Context(), req.WorkspaceRoot, req.AgentID)
	if err != nil {
		agentErr(w, err, req.AgentID)
		return
	}
	diffCfg, err := loadConfigFor(req.WorkspaceRoot)
	if err != nil {
		badReq(w, err)
		return
	}
	res, err := git.Diff(r.Context(), a.WorktreePath, git.DiffOptions{
		Paths: req.FilePaths, IncludeUntracked: req.IncludeUntracked, MaxBytes: diffCfg.DiffMaxBytes,
		Base: s.seriesBase(r.Context(), req.WorkspaceRoot, a),
	})
	if err != nil {
		writeGitErr(w, err)
		return
	}
	summary, fallback := "", false
	if req.Summarize && res.Diff != "" {
		cfg, err := resolveLLM(req.WorkspaceRoot)
		switch {
		case errors.Is(err, llm.ErrNotConfigured):
			// No sidecar: the raw diff is already in the reply; the
			// caller summarizes it.
			fallback = true
		case err != nil:
			badReq(w, err)
			return
		default:
			summary, err = llm.SummarizeDiff(r.Context(), cfg, res.Files, res.Diff)
			if err != nil {
				writeErr(w, http.StatusBadGateway, "llm_error", err.Error())
				return
			}
		}
	}
	writeJSON(w, map[string]any{
		"diff": res.Diff, "truncated": res.Truncated, "files": res.Files, "summary": summary,
		"fallback": fallback,
	})
}

// seriesBase is the commit an open series' builder branched from, when a
// is that builder working in the series worktree; "" otherwise. Diffing
// from it shows phase work the builder already committed.
func (s *server) seriesBase(ctx context.Context, ws string, a *store.Agent) string {
	list, err := s.store.ListBlueprints(ctx, ws)
	if err != nil {
		return ""
	}
	for _, bp := range list {
		if bp.Series && bp.Status != store.BlueprintHandover && bp.Builder == a.AgentID &&
			bp.BaseRef != "" && canonical(bp.WorktreePath) == canonical(a.WorktreePath) {
			return bp.BaseRef
		}
	}
	return ""
}

// handleConflicts checks whether two agents' work would conflict when merged.
func (s *server) handleConflicts(w http.ResponseWriter, r *http.Request) {
	var req protocol.ConflictsRequest
	if err := decodeBody(r, &req); err != nil {
		badReq(w, err)
		return
	}
	if req.WorkspaceRoot == "" || req.AgentID == "" || req.TargetAgentID == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "workspace_root, agent_id, and target_agent_id are required")
		return
	}
	me, err := s.store.GetAgent(r.Context(), req.WorkspaceRoot, req.AgentID)
	if err != nil {
		agentErr(w, err, req.AgentID)
		return
	}
	them, err := s.store.GetAgent(r.Context(), req.WorkspaceRoot, req.TargetAgentID)
	if err != nil {
		agentErr(w, err, req.TargetAgentID)
		return
	}
	res, err := git.Conflicts(r.Context(), me.WorktreePath, them.WorktreePath)
	if err != nil {
		writeGitErr(w, err)
		return
	}
	conflicts := []protocol.Conflict{}
	for _, c := range res.Conflicts {
		conflicts = append(conflicts, protocol.Conflict{Path: c.Path, OursLines: c.OursLines, TheirsLines: c.TheirsLines})
	}
	writeJSON(w, map[string]any{"conflicts": conflicts, "overlap_only": res.OverlapOnly})
}

// writeGitErr maps git failures to stable codes.
func writeGitErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, git.ErrNotARepo):
		writeErr(w, http.StatusBadRequest, "not_a_repo", err.Error())
	case errors.Is(err, git.ErrNoCommits):
		writeErr(w, http.StatusBadRequest, "no_commits", err.Error())
	case errors.Is(err, git.ErrTooOld):
		writeErr(w, http.StatusBadRequest, "git_too_old", err.Error())
	case errors.Is(err, git.ErrCannotCompare):
		writeErr(w, http.StatusBadRequest, "cannot_compare", err.Error())
	case errors.Is(err, git.ErrGitMissing):
		writeErr(w, http.StatusInternalServerError, "git_missing", err.Error())
	default:
		writeErr(w, http.StatusInternalServerError, "git_error", err.Error())
	}
}
