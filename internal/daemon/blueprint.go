package daemon

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/search-3d/breakroom/internal/git"
	"github.com/search-3d/breakroom/internal/protocol"
	"github.com/search-3d/breakroom/internal/render"
	"github.com/search-3d/breakroom/internal/store"
)

// This file serves the Blueprint stage-gate loop: role gating, transition
// calls into the store, and peer notifications over existing mail.
// Sequencing and shape are enforced by internal/store.

// mapBlueprintErr maps store blueprint failures to stable wire codes.
func mapBlueprintErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNoBlueprint):
		writeErr(w, http.StatusNotFound, "unknown_blueprint", err.Error())
	case errors.Is(err, store.ErrBadSeriesName):
		writeErr(w, http.StatusBadRequest, "bad_series_name", err.Error())
	case errors.Is(err, store.ErrBlueprintExists):
		badReq(w, err)
	case errors.Is(err, store.ErrBlueprintBlocked):
		writeErr(w, http.StatusConflict, "blueprint_blocked", err.Error())
	case errors.Is(err, store.ErrBadTransition):
		writeErr(w, http.StatusConflict, "bad_transition", err.Error())
	case errors.Is(err, store.ErrRoadmapUnapproved):
		writeErr(w, http.StatusConflict, "roadmap_unapproved", err.Error())
	case errors.Is(err, store.ErrHumanApprovalRequired):
		writeErr(w, http.StatusForbidden, "human_approval_required", err.Error())
	case errors.Is(err, store.ErrInvalidBlueprint):
		badReq(w, err)
	default:
		storeErr(w, err)
	}
}

// blueprintMail mails a blueprint step to its recipient and wakes them.
// The step is already saved, so a failed mail is logged, not returned.
func (s *server) blueprintMail(ctx context.Context, ws, from, to, typ, content string) {
	s.blueprintMailDiff(ctx, ws, from, to, typ, content, "")
}

// blueprintMailDiff is blueprintMail with a diff attached. Long content is cut
// to fit; the ledger keeps the full text.
func (s *server) blueprintMailDiff(ctx context.Context, ws, from, to, typ, content, diff string) {
	if to == "" {
		return // a series without a builder yet; join resends what it needs
	}
	sent, err := s.store.SendMessage(ctx, store.SendParams{
		WorkspaceRoot: ws, From: from, To: to, Type: typ, Content: capMail(content), DiffAttachment: diff,
	})
	if err != nil {
		s.log.Error("blueprint mail failed", "workspace", ws, "to", to, "type", typ, "error", err)
		return
	}
	for _, m := range sent {
		s.hub.signal(hubKey(ws, m.Recipient))
	}
}

// mailMax caps blueprint notifications. They land in a model's context
// unasked, so they carry the gist; blueprint_status phase=N has the rest.
const mailMax = 8 << 10

// capMail is the backstop: cut oversize mail on a rune boundary.
func capMail(content string) string {
	if len(content) <= mailMax {
		return content
	}
	const note = "\n\n[truncated - full text via blueprint_status]"
	return render.CutRunes(content, mailMax-len(note)) + note
}

// handleBlueprintInit starts a blueprint, or a series (with its own worktree)
// when no builder is named.
func (s *server) handleBlueprintInit(w http.ResponseWriter, r *http.Request) {
	var req protocol.BlueprintInitRequest
	if err := decodeBody(r, &req); err != nil {
		badReq(w, err)
		return
	}
	if req.WorkspaceRoot == "" || req.Name == "" || req.Architect == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "workspace_root, name, and architect are required")
		return
	}
	ids := []string{req.Architect}
	if req.Builder != "" {
		ids = append(ids, req.Builder)
	}
	for _, id := range ids {
		if _, err := s.store.GetAgent(r.Context(), req.WorkspaceRoot, id); err != nil {
			agentErr(w, err, id)
			return
		}
	}
	ctx := r.Context()
	if err := s.store.InitBlueprint(ctx, req.WorkspaceRoot, req.Name, req.Architect, req.Builder, req.Goal); err != nil {
		mapBlueprintErr(w, err)
		return
	}
	builder := req.Builder
	if builder == "" {
		// A series: the architect's team builder carries over; otherwise a
		// lone waiting builder joins automatically, or multiple waiters hear
		// it opened. No mail is sent until phase 1 is specced so a waiting
		// builder stays parked through roadmap approval.
		b, err := s.teamBuilder(ctx, req.WorkspaceRoot, req.Architect)
		if err != nil {
			s.log.Error("team builder lookup failed", "error", err)
		}
		if b == "" {
			if b, err = s.loneWaiter(ctx, req.WorkspaceRoot); err != nil {
				s.log.Error("lone waiter lookup failed", "error", err)
			}
		}
		if b != "" {
			if err := s.store.JoinBlueprint(ctx, req.WorkspaceRoot, req.Name, b); err != nil {
				s.log.Error("builder auto-join failed", "builder", b, "error", err)
			} else {
				builder = b
				_ = s.store.RemoveSeriesWaiter(ctx, req.WorkspaceRoot, b)
				s.touchAgent(ctx, req.WorkspaceRoot, b, "waiting", req.Name+": waiting for phase 1 spec")
			}
		}
		if builder == "" {
			s.notifyWaiters(ctx, req.WorkspaceRoot, req.Name, req.Architect)
		}
	}
	s.touchCaller(req.WorkspaceRoot, req.Architect)
	writeJSON(w, map[string]any{"name": req.Name, "status": store.BlueprintDrafting, "builder": builder})
}

// touchAgent updates an online caller's status, current_step, and heartbeat on
// blueprint transitions so blueprint agents don't need separate update_plan calls.
func (s *server) touchAgent(ctx context.Context, ws, agentID, status, step string) {
	if agentID == "" {
		return
	}
	a, err := s.store.GetAgent(ctx, ws, agentID)
	if err != nil || a.Status == "offline" {
		return
	}
	if _, err := s.store.UpdateAgent(ctx, ws, agentID, store.AgentUpdate{
		Status: &status, CurrentStep: &step,
	}); err != nil {
		s.log.Error("blueprint touch agent failed", "agent", agentID, "error", err)
	}
}

// releaseAgent marks an agent offline, logging any failure.
func (s *server) releaseAgent(ctx context.Context, ws, agentID string) {
	if agentID == "" {
		return
	}
	if err := s.store.ReleaseAgent(ctx, ws, agentID); err != nil && !errors.Is(err, store.ErrNoAgent) {
		s.log.Error("blueprint release agent failed", "agent", agentID, "error", err)
	}
}

// handleBlueprintSpec records the architect's spec for the next phase and mails
// it to the builder.
func (s *server) handleBlueprintSpec(w http.ResponseWriter, r *http.Request) {
	var req protocol.BlueprintSpecRequest
	if err := decodeBody(r, &req); err != nil {
		badReq(w, err)
		return
	}
	if req.WorkspaceRoot == "" || req.Name == "" || req.Caller == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "workspace_root, name, and caller are required")
		return
	}
	bp, err := s.store.GetBlueprint(r.Context(), req.WorkspaceRoot, req.Name)
	if err != nil {
		mapBlueprintErr(w, err)
		return
	}
	if req.Caller != bp.Architect {
		writeErr(w, http.StatusForbidden, "forbidden_role", "only the architect specs phases")
		return
	}
	if err := s.store.SpecPhase(r.Context(), store.SpecParams{
		WorkspaceRoot: req.WorkspaceRoot, Name: req.Name, Phase: req.Phase,
		Title: req.Title, Objective: req.Objective, Requirements: req.Requirements,
		Acceptance: req.Acceptance, Constraints: req.Constraints,
	}); err != nil {
		mapBlueprintErr(w, err)
		return
	}
	s.touchAgent(r.Context(), req.WorkspaceRoot, bp.Architect, "waiting", fmt.Sprintf("%s: phase %d specced", req.Name, req.Phase))
	s.touchAgent(r.Context(), req.WorkspaceRoot, bp.Builder, "active", fmt.Sprintf("%s: phase %d (%s)", req.Name, req.Phase, req.Title))
	content := render.RenderSpec(req.Name, req.Phase, req.Title, req.Objective, req.Requirements, req.Acceptance, req.Constraints) + specWhere(bp)
	s.blueprintMail(r.Context(), req.WorkspaceRoot, bp.Architect, bp.Builder, "blueprint_spec", content)
	s.touchCaller(req.WorkspaceRoot, req.Caller)
	writeJSON(w, map[string]any{"phase": req.Phase, "status": store.PhaseConstructing})
}

// handleBlueprintSubmit records the builder's deliverable and mails it to the
// architect with the builder's diff attached.
func (s *server) handleBlueprintSubmit(w http.ResponseWriter, r *http.Request) {
	var req protocol.BlueprintSubmitRequest
	if err := decodeBody(r, &req); err != nil {
		badReq(w, err)
		return
	}
	if req.WorkspaceRoot == "" || req.Name == "" || req.Caller == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "workspace_root, name, and caller are required")
		return
	}
	bp, err := s.store.GetBlueprint(r.Context(), req.WorkspaceRoot, req.Name)
	if err != nil {
		mapBlueprintErr(w, err)
		return
	}
	if req.Caller != bp.Builder {
		writeErr(w, http.StatusForbidden, "forbidden_role", "only the builder submits deliverables")
		return
	}
	if err := s.store.SubmitPhase(r.Context(), store.SubmitParams{
		WorkspaceRoot: req.WorkspaceRoot, Name: req.Name, Phase: req.Phase,
		Items: req.Items, Proof: req.Proof, Notes: req.Notes,
	}); err != nil {
		mapBlueprintErr(w, err)
		return
	}
	s.touchAgent(r.Context(), req.WorkspaceRoot, bp.Builder, "waiting", fmt.Sprintf("%s: phase %d submitted", req.Name, req.Phase))
	s.touchAgent(r.Context(), req.WorkspaceRoot, bp.Architect, "active", fmt.Sprintf("%s: inspecting phase %d", req.Name, req.Phase))
	var diffAttach string
	if sender, err := s.store.GetAgent(r.Context(), req.WorkspaceRoot, bp.Builder); err == nil && sender.WorktreePath != "" {
		opts := git.DiffOptions{IncludeUntracked: true, Base: s.seriesBase(r.Context(), req.WorkspaceRoot, sender)}
		if res, err := git.Diff(r.Context(), sender.WorktreePath, opts); err == nil && res.Diff != "" {
			const diffMailBudget = 12 << 10
			diffAttach = render.TrimMiddle(res.Diff, diffMailBudget, func(omitted int) string {
				return fmt.Sprintf("[… %d bytes omitted - call get_diff agent_id=%s for the full diff]\n", omitted, bp.Builder)
			})
		}
	}
	s.blueprintMailDiff(r.Context(), req.WorkspaceRoot, bp.Builder, bp.Architect,
		"review_req", render.RenderDeliverable(req.Name, req.Phase, req.Items, req.Proof, req.Notes, mailMax), diffAttach)
	s.touchCaller(req.WorkspaceRoot, req.Caller)
	writeJSON(w, map[string]any{"phase": req.Phase, "status": store.PhaseInspection})
}

// handleBlueprintInspect records the architect's sign-off or punch list and
// mails it to the builder.
func (s *server) handleBlueprintInspect(w http.ResponseWriter, r *http.Request) {
	var req protocol.BlueprintInspectRequest
	if err := decodeBody(r, &req); err != nil {
		badReq(w, err)
		return
	}
	if req.WorkspaceRoot == "" || req.Name == "" || req.Caller == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "workspace_root, name, and caller are required")
		return
	}
	bp, err := s.store.GetBlueprint(r.Context(), req.WorkspaceRoot, req.Name)
	if err != nil {
		mapBlueprintErr(w, err)
		return
	}
	if req.Caller != bp.Architect {
		writeErr(w, http.StatusForbidden, "forbidden_role", "only the architect inspects; builders submit deliverables with blueprint_submit")
		return
	}
	cfg, err := loadConfigFor(req.WorkspaceRoot)
	if err != nil {
		badReq(w, err)
		return
	}
	rounds, blocked, err := s.store.InspectPhase(r.Context(), store.InspectParams{
		WorkspaceRoot: req.WorkspaceRoot, Name: req.Name, Phase: req.Phase,
		Verdict: req.Verdict, AcceptanceIDs: req.AcceptanceIDs,
		Findings: req.Findings, MaxRounds: cfg.BlueprintMaxRounds,
	})
	if err != nil {
		mapBlueprintErr(w, err)
		return
	}
	status := store.PhaseSignoff
	if req.Verdict == "punchlist" {
		status = store.PhasePunchlist
		if blocked {
			status = store.BlueprintBlocked
		}
		s.touchAgent(r.Context(), req.WorkspaceRoot, bp.Architect, "waiting", fmt.Sprintf("%s: phase %d punchlist", req.Name, req.Phase))
		s.touchAgent(r.Context(), req.WorkspaceRoot, bp.Builder, "active", fmt.Sprintf("%s: phase %d punchlist", req.Name, req.Phase))
		s.blueprintMail(r.Context(), req.WorkspaceRoot, bp.Architect, bp.Builder,
			"blueprint_punchlist", render.RenderInspection(req.Name, req.Phase, req.Verdict, req.AcceptanceIDs, req.Findings, blocked))
	} else {
		// Sign-off stays quiet for the builder: the next phase spec (with
		// "Phase N signed off") or the final landing/handover wakes it once
		// there is something to act on.
		s.touchAgent(r.Context(), req.WorkspaceRoot, bp.Architect, "active", fmt.Sprintf("%s: phase %d signed off", req.Name, req.Phase))
	}
	s.touchCaller(req.WorkspaceRoot, req.Caller)
	writeJSON(w, map[string]any{"phase": req.Phase, "status": status, "rounds": rounds})
}

// toRoadmapJSON converts a stored roadmap to its API shape.
func toRoadmapJSON(rm store.Roadmap) protocol.Roadmap {
	out := protocol.Roadmap{
		Version: rm.Version, Phases: rm.Phases, OutOfScope: rm.OutOfScope,
		Base:        rm.Base,
		ProposedAt:  rm.ProposedAt.UTC().Format(time.RFC3339),
		ApprovedVia: rm.ApprovedVia, ApprovalQuote: rm.ApprovalQuote,
	}
	if rm.ApprovedAt != nil {
		at := rm.ApprovedAt.UTC().Format(time.RFC3339)
		out.ApprovedAt = &at
	}
	return out
}

// toBlueprintJSON renders the header only (phase < 0), the ledger overview
// with slim phases (phase 0), or one phase in full detail (phase N).
func toBlueprintJSON(bp *store.Blueprint, phase int) protocol.Blueprint {
	out := protocol.Blueprint{
		Name: bp.Name, Architect: bp.Architect, Builder: bp.Builder,
		Goal: bp.Goal, Status: bp.Status, CurrentPhase: bp.CurrentPhase,
		Series: bp.Series, WorktreePath: bp.WorktreePath, Branch: bp.Branch, BaseRef: bp.BaseRef,
	}
	if phase < 0 {
		return out
	}
	out.Phases = []protocol.BlueprintPhase{}
	for _, p := range bp.Phases {
		j := protocol.BlueprintPhase{
			Phase: p.Phase, Title: p.Title, Status: p.Status, Rounds: p.Rounds,
			UpdatedAt: p.UpdatedAt.UTC().Format(time.RFC3339),
		}
		if phase == 0 {
			out.Phases = append(out.Phases, j)
			continue
		}
		if p.Phase != phase {
			continue
		}
		j.Objective, j.Requirements, j.Acceptance = p.Objective, p.Requirements, p.Acceptance
		j.Constraints, j.Deliverable = p.Constraints, p.Deliverable
		for _, in := range p.Inspections {
			j.Inspections = append(j.Inspections, protocol.BlueprintInspection{
				Verdict: in.Verdict, AcceptanceIDs: in.AcceptanceIDs, Findings: in.Findings,
				At: in.At.Format(time.RFC3339Nano),
			})
		}
		out.Phases = append(out.Phases, j)
	}
	return out
}

// handleBlueprintStatus returns the blueprint list, one blueprint's phases, or
// one phase in full.
func (s *server) handleBlueprintStatus(w http.ResponseWriter, r *http.Request) {
	var req protocol.BlueprintStatusRequest
	if err := decodeBody(r, &req); err != nil {
		badReq(w, err)
		return
	}
	if req.WorkspaceRoot == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "workspace_root is required")
		return
	}
	if req.Phase < 0 || (req.Phase > 0 && req.Name == "") {
		writeErr(w, http.StatusBadRequest, "bad_request", "phase needs a blueprint name and must be >= 1")
		return
	}
	if req.Name == "" {
		list, err := s.store.ListBlueprints(r.Context(), req.WorkspaceRoot)
		if err != nil {
			storeErr(w, err)
			return
		}
		out := []protocol.Blueprint{}
		for _, b := range list {
			out = append(out, toBlueprintJSON(b, -1))
		}
		writeJSON(w, map[string]any{"blueprints": out})
		return
	}
	bp, err := s.store.GetBlueprint(r.Context(), req.WorkspaceRoot, req.Name)
	if err != nil {
		mapBlueprintErr(w, err)
		return
	}
	if req.Phase > len(bp.Phases) {
		writeErr(w, http.StatusBadRequest, "bad_request", fmt.Sprintf("no phase %d (blueprint has %d)", req.Phase, len(bp.Phases)))
		return
	}
	rms, err := s.store.ListRoadmaps(r.Context(), req.WorkspaceRoot, req.Name)
	if err != nil {
		storeErr(w, err)
		return
	}
	out := toBlueprintJSON(bp, req.Phase)
	for _, rm := range rms {
		out.Roadmaps = append(out.Roadmaps, toRoadmapJSON(rm))
	}
	writeJSON(w, map[string]any{"blueprint": out})
}

// handleBlueprintRoadmap records a new roadmap version for the human to approve.
func (s *server) handleBlueprintRoadmap(w http.ResponseWriter, r *http.Request) {
	var req protocol.BlueprintRoadmapRequest
	if err := decodeBody(r, &req); err != nil {
		badReq(w, err)
		return
	}
	if req.WorkspaceRoot == "" || req.Name == "" || req.Caller == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "workspace_root, name, and caller are required")
		return
	}
	bp, err := s.store.GetBlueprint(r.Context(), req.WorkspaceRoot, req.Name)
	if err != nil {
		mapBlueprintErr(w, err)
		return
	}
	if req.Caller != bp.Architect {
		writeErr(w, http.StatusForbidden, "forbidden_role", "only the architect proposes the roadmap")
		return
	}
	version, err := s.store.ProposeRoadmap(r.Context(), req.WorkspaceRoot, req.Name, req.Phases, req.OutOfScope, req.Base)
	if err != nil {
		mapBlueprintErr(w, err)
		return
	}
	s.touchAgent(r.Context(), req.WorkspaceRoot, bp.Architect, "waiting", fmt.Sprintf("%s: roadmap v%d proposed", req.Name, version))
	s.touchCaller(req.WorkspaceRoot, req.Caller)
	writeJSON(w, map[string]any{"version": version, "status": "pending", "handoff": handoffRoadmap(req.Name, version)})
}

// handleBlueprintApprove records the human's roadmap approval, from chat (the
// quote must match what they typed) or the CLI. The first one sets up the series.
func (s *server) handleBlueprintApprove(w http.ResponseWriter, r *http.Request) {
	var req protocol.BlueprintApproveRequest
	if err := decodeBody(r, &req); err != nil {
		badReq(w, err)
		return
	}
	if req.WorkspaceRoot == "" || req.Name == "" || (req.Via != "chat" && req.Via != "cli") {
		writeErr(w, http.StatusBadRequest, "bad_request", "workspace_root, name, and via (chat|cli) are required")
		return
	}
	bp, err := s.store.GetBlueprint(r.Context(), req.WorkspaceRoot, req.Name)
	if err != nil {
		mapBlueprintErr(w, err)
		return
	}
	if req.Via == "chat" && req.Caller != bp.Architect {
		writeErr(w, http.StatusForbidden, "forbidden_role", "only the architect records the human's approval")
		return
	}
	rm, err := s.store.ApproveRoadmap(r.Context(), store.ApproveParams{
		WorkspaceRoot: req.WorkspaceRoot, Name: req.Name, Version: req.Version,
		Via: req.Via, Caller: req.Caller, Quote: req.Quote,
	})
	if err != nil {
		mapBlueprintErr(w, err)
		return
	}
	s.touchAgent(r.Context(), req.WorkspaceRoot, bp.Architect, "active", fmt.Sprintf("%s: roadmap v%d approved", req.Name, rm.Version))
	if req.Via == "cli" {
		// Approved from the human's terminal: the architect didn't make this call.
		s.blueprintMail(r.Context(), req.WorkspaceRoot, "human", bp.Architect, "blueprint_roadmap",
			fmt.Sprintf("### [BLUEPRINT] %s Roadmap v%d approved\n\nThe human approved the roadmap from the CLI. Spec phase %d.\n", req.Name, rm.Version, bp.CurrentPhase+1))
	}
	s.touchCaller(req.WorkspaceRoot, bp.Architect)
	resp := map[string]any{"version": rm.Version, "approved_via": rm.ApprovedVia}
	if bp.Series && bp.WorktreePath == "" {
		// First approval of a series: set up its worktree. A git failure
		// keeps the approval; the architect retries with blueprint_series.
		if handoff, err := s.createSeries(r.Context(), bp, rm.Base, "", ""); err != nil {
			resp["series_error"] = err.Error() + " (retry with blueprint_series, optionally with another branch or path)"
		} else {
			resp["handoff"] = handoff
		}
	}
	writeJSON(w, resp)
}

// handleBlueprintClose ends a blueprint that has no phase in progress.
func (s *server) handleBlueprintClose(w http.ResponseWriter, r *http.Request) {
	var req protocol.BlueprintCloseRequest
	if err := decodeBody(r, &req); err != nil {
		badReq(w, err)
		return
	}
	if req.WorkspaceRoot == "" || req.Name == "" || req.Caller == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "workspace_root, name, and caller are required")
		return
	}
	bp, err := s.store.GetBlueprint(r.Context(), req.WorkspaceRoot, req.Name)
	if err != nil {
		mapBlueprintErr(w, err)
		return
	}
	if req.Caller != bp.Architect {
		writeErr(w, http.StatusForbidden, "forbidden_role", "only the architect closes")
		return
	}
	if err := s.store.CloseBlueprint(r.Context(), req.WorkspaceRoot, req.Name); err != nil {
		mapBlueprintErr(w, err)
		return
	}
	// Say how it actually ended; a repeat close (idempotent) stays quiet.
	outcome := ""
	switch bp.Status {
	case store.BlueprintDrafting:
		outcome = "Closed before any phase was specced. No work was delivered."
	case store.BlueprintBlocked:
		outcome = fmt.Sprintf("Closed while BLOCKED at phase %d (punch-list round cap exhausted). Needs human triage; work is incomplete.", bp.CurrentPhase)
	case store.BlueprintActive:
		outcome = fmt.Sprintf("All %d phase(s) verified and signed off. Solution delivered.", bp.CurrentPhase)
		if rms, err := s.store.ListRoadmaps(r.Context(), req.WorkspaceRoot, req.Name); err == nil && len(rms) > 0 {
			if total := len(rms[len(rms)-1].Phases); bp.CurrentPhase < total {
				outcome = fmt.Sprintf("Closed after %d of %d roadmap phase(s) signed off; the rest were not built.", bp.CurrentPhase, total)
			}
		}
	case store.BlueprintAcceptance, store.BlueprintAccepted:
		outcome = fmt.Sprintf("All %d phase(s) verified and signed off. Closed without landing.", bp.CurrentPhase)
	}
	if outcome != "" {
		s.touchAgent(r.Context(), req.WorkspaceRoot, bp.Architect, "completed", req.Name+": closed")
		s.touchAgent(r.Context(), req.WorkspaceRoot, bp.Builder, "completed", req.Name+": closed")
		s.blueprintMail(r.Context(), req.WorkspaceRoot, bp.Architect, bp.Builder,
			"chat", "### [BLUEPRINT] "+req.Name+" - HANDOVER\n\n"+outcome)
	}
	s.touchCaller(req.WorkspaceRoot, req.Caller)
	if bp.Builder != "" && bp.WorktreePath != "" {
		if err := s.store.SetAgentWorktree(r.Context(), req.WorkspaceRoot, bp.Builder, req.WorkspaceRoot); err != nil {
			s.log.Error("moving builder back failed", "builder", bp.Builder, "error", err)
		}
	}
	resp := map[string]any{"name": req.Name, "status": store.BlueprintHandover}
	if bp.WorktreePath != "" && bp.Status != store.BlueprintHandover {
		// Closed without landing: the worktree and branch stay; say where.
		if bp.Status == store.BlueprintBlocked {
			resp["handoff"] = handoffBlocked(bp, req.WorkspaceRoot)
		} else {
			resp["handoff"] = handoffAbandoned(bp, req.WorkspaceRoot)
		}
	}
	writeJSON(w, resp)
}

// Renderers below follow the docs/blueprint-spec.md templates, headed with
// the blueprint name and phase so clients can filter.

// specWhere tells the builder where a series' work goes ("" without one).
func specWhere(bp *store.Blueprint) string {
	if bp.WorktreePath == "" {
		return ""
	}
	return fmt.Sprintf("\n#### Where\nWork only in %s (branch %s).\n", bp.WorktreePath, bp.Branch)
}
