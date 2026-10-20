package daemon

// This file serves the human's acceptance of finished blueprints: the
// architect requests it, the human's verdict (relayed from chat or typed
// in a terminal) accepts the work or turns their notes into a follow-up
// phase.

import (
	"fmt"
	"net/http"
	"os"

	"github.com/search-3d/breakroom/internal/git"
	"github.com/search-3d/breakroom/internal/protocol"
	"github.com/search-3d/breakroom/internal/store"
)

// handleBlueprintAcceptance hands finished work to the human for review, after
// the architect signed off the last phase.
func (s *server) handleBlueprintAcceptance(w http.ResponseWriter, r *http.Request) {
	var req protocol.BlueprintAcceptanceRequest
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
		writeErr(w, http.StatusForbidden, "forbidden_role", "only the architect requests acceptance")
		return
	}
	if err := s.store.RequestAcceptance(ctx, req.WorkspaceRoot, req.Name); err != nil {
		mapBlueprintErr(w, err)
		return
	}
	if bp, err = s.store.GetBlueprint(ctx, req.WorkspaceRoot, req.Name); err != nil {
		mapBlueprintErr(w, err)
		return
	}
	s.touchAgent(ctx, req.WorkspaceRoot, bp.Architect, "waiting", bp.Name+": awaiting human acceptance")
	if bp.Builder != "" {
		s.hub.signal(hubKey(req.WorkspaceRoot, bp.Builder))
	}
	writeJSON(w, map[string]any{"status": bp.Status, "handoff": handoffAcceptance(bp, req.WorkspaceRoot)})
}

// handleBlueprintVerdict records the human's review: accept, or changes, which
// add a follow-up phase. A chat verdict must quote the human's own words.
func (s *server) handleBlueprintVerdict(w http.ResponseWriter, r *http.Request) {
	var req protocol.BlueprintVerdictRequest
	if err := decodeBody(r, &req); err != nil {
		badReq(w, err)
		return
	}
	if req.WorkspaceRoot == "" || req.Name == "" || (req.Via != "chat" && req.Via != "cli") {
		writeErr(w, http.StatusBadRequest, "bad_request", "workspace_root, name, and via (chat|cli) are required")
		return
	}
	ctx := r.Context()
	bp, err := s.store.GetBlueprint(ctx, req.WorkspaceRoot, req.Name)
	if err != nil {
		mapBlueprintErr(w, err)
		return
	}
	if req.Via == "chat" && req.Caller != bp.Architect {
		writeErr(w, http.StatusForbidden, "forbidden_role", "only the architect records the human's verdict")
		return
	}
	followUp, err := s.store.RecordVerdict(ctx, store.VerdictParams{
		WorkspaceRoot: req.WorkspaceRoot, Name: req.Name, Verdict: req.Verdict, Via: req.Via,
		Caller: req.Caller, Quote: req.Quote, Notes: req.Notes, Title: req.Title,
	})
	if err != nil {
		mapBlueprintErr(w, err)
		return
	}
	if bp, err = s.store.GetBlueprint(ctx, req.WorkspaceRoot, req.Name); err != nil {
		mapBlueprintErr(w, err)
		return
	}
	if req.Verdict == "accept" {
		handoff := handoffLandingChoices(bp)
		s.touchAgent(ctx, req.WorkspaceRoot, bp.Architect, "active", bp.Name+": accepted, awaiting landing choice")
		if req.Via == "cli" {
			s.blueprintMail(ctx, req.WorkspaceRoot, "human", bp.Architect, "chat",
				fmt.Sprintf("### [BLUEPRINT] %s accepted\n\nThe human accepted the work from the CLI. Ask how to land it:\n\n%s\n", bp.Name, handoff))
		}
		writeJSON(w, map[string]any{"status": bp.Status, "handoff": handoff})
		return
	}
	s.touchAgent(ctx, req.WorkspaceRoot, bp.Architect, "active", fmt.Sprintf("%s: speccing follow-up phase %d", bp.Name, followUp))
	if req.Via == "cli" {
		s.blueprintMail(ctx, req.WorkspaceRoot, "human", bp.Architect, "chat",
			fmt.Sprintf("### [BLUEPRINT] %s - changes requested\n\nThe human asked for changes from the CLI:\n\n%s\n\nSpec phase %d to cover them.\n", bp.Name, req.Notes, followUp))
	}
	writeJSON(w, map[string]any{"status": bp.Status, "follow_up_phase": followUp, "handoff": handoffFollowUp(bp.Name, followUp)})
}

// handleBlueprintLand records how accepted work landed (merge, branch, PR, or
// manual), removes the series worktree unless manual, and keeps its branch.
// A worktree with uncommitted changes is refused.
func (s *server) handleBlueprintLand(w http.ResponseWriter, r *http.Request) {
	var req protocol.BlueprintLandRequest
	if err := decodeBody(r, &req); err != nil {
		badReq(w, err)
		return
	}
	if req.WorkspaceRoot == "" || req.Name == "" || (req.Via != "chat" && req.Via != "cli") {
		writeErr(w, http.StatusBadRequest, "bad_request", "workspace_root, name, and via (chat|cli) are required")
		return
	}
	ctx := r.Context()
	bp, err := s.store.GetBlueprint(ctx, req.WorkspaceRoot, req.Name)
	if err != nil {
		mapBlueprintErr(w, err)
		return
	}
	if req.Via == "chat" && req.Caller != bp.Architect {
		writeErr(w, http.StatusForbidden, "forbidden_role", "only the architect records the landing")
		return
	}
	wt := ""
	if bp.WorktreePath != "" {
		if _, err := os.Stat(bp.WorktreePath); err == nil {
			wt = bp.WorktreePath
		}
	}
	if wt != "" && req.Method != "manual" && bp.Status == store.BlueprintAccepted {
		dirty, err := git.Dirty(ctx, wt)
		if err != nil {
			writeGitErr(w, err)
			return
		}
		if dirty {
			writeErr(w, http.StatusConflict, "worktree_dirty",
				wt+" has uncommitted changes; commit them (they're part of the work) or discard them, then land again")
			return
		}
	}
	if req.Method == "merge" && bp.Branch != "" && bp.Status == store.BlueprintAccepted {
		// "Merged" is checked, not taken on faith: the worktree goes next.
		merged, err := git.IsAncestor(ctx, req.WorkspaceRoot, bp.Branch, "HEAD")
		if err != nil {
			writeGitErr(w, err)
			return
		}
		if !merged {
			writeErr(w, http.StatusConflict, "not_merged",
				bp.Branch+" is not merged into the main checkout's HEAD; merge it there (or pick another landing), then land again")
			return
		}
	}
	if err := s.store.RecordLanding(ctx, store.LandingParams{
		WorkspaceRoot: req.WorkspaceRoot, Name: req.Name, Method: req.Method, Ref: req.Ref,
		Via: req.Via, Caller: req.Caller, Quote: req.Quote,
	}); err != nil {
		mapBlueprintErr(w, err)
		return
	}
	resp := map[string]any{"status": store.BlueprintHandover}
	var removeErr error
	if wt != "" && req.Method != "manual" {
		if removeErr = git.WorktreeRemove(ctx, req.WorkspaceRoot, wt); removeErr != nil {
			s.log.Error("removing series worktree failed", "worktree", wt, "error", removeErr)
			resp["warning"] = "landed, but removing the worktree failed: " + removeErr.Error()
		}
	}
	if bp.Builder != "" && bp.WorktreePath != "" {
		if err := s.store.SetAgentWorktree(ctx, req.WorkspaceRoot, bp.Builder, req.WorkspaceRoot); err != nil {
			s.log.Error("moving builder back failed", "builder", bp.Builder, "error", err)
		}
	}
	if bp, err = s.store.GetBlueprint(ctx, req.WorkspaceRoot, req.Name); err != nil {
		mapBlueprintErr(w, err)
		return
	}
	s.touchAgent(ctx, req.WorkspaceRoot, bp.Architect, "completed", bp.Name+": landed")
	s.touchAgent(ctx, req.WorkspaceRoot, bp.Builder, "completed", bp.Name+": landed")
	handoff := handoffLanded(bp, req.WorkspaceRoot, removeErr)
	if bp.Builder != "" {
		note := fmt.Sprintf("You're back in the main checkout (%s). Nothing further needed.", req.WorkspaceRoot)
		if bp.LandingMethod == "manual" {
			note = "You have been disconnected from Breakroom. Nothing further needed."
		}
		s.blueprintMail(ctx, req.WorkspaceRoot, bp.Architect, bp.Builder, "chat",
			fmt.Sprintf("### [BLUEPRINT] %s - LANDED\n\n%s\n%s\n",
				bp.Name, landedFacts(bp, req.WorkspaceRoot, removeErr), note))
	}
	if req.Via == "cli" {
		s.blueprintMail(ctx, req.WorkspaceRoot, "human", bp.Architect, "chat",
			fmt.Sprintf("### [BLUEPRINT] %s - LANDED\n\nRecorded from the human's terminal.\n\n%s\n", bp.Name, handoff))
	}
	if bp.LandingMethod == "manual" {
		s.releaseAgent(ctx, req.WorkspaceRoot, bp.Architect)
		s.releaseAgent(ctx, req.WorkspaceRoot, bp.Builder)
	}
	resp["handoff"] = handoff
	writeJSON(w, resp)
}
