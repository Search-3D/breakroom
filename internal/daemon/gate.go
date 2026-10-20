package daemon

// This file serves the stop gate: a host Stop hook asks whether its agent
// may end the turn. Unread mail blocks the stop (delivered as the reason);
// an agent waiting on its blueprint peer is held here, at zero model
// tokens, until mail arrives or the hold expires. Mail is peeked, not
// taken: the CLI acks it after printing, so a killed hook redelivers.

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/search-3d/breakroom/internal/hosts"
	"github.com/search-3d/breakroom/internal/protocol"
	"github.com/search-3d/breakroom/internal/render"
	"github.com/search-3d/breakroom/internal/store"
)

const defaultGateHold = 540 * time.Second

// guessedWakeReason wakes a chat the gate couldn't identify for sure.
const guessedWakeReason = "Breakroom: new mail may be waiting for you. Call list_agents with your caller_id: " +
	"unread mail arrives in _unread_messages. If none arrives, end your turn to keep waiting."

// maxMailRun caps stops blocked by mail that was already waiting, in one
// hook-continued turn: two agents trading replies would otherwise never
// stop (each reply is a tool call, which resets the host's own cap). Mail
// that wakes a park doesn't count; that's the blueprint loop working.
const maxMailRun = 20

type gateDecision = protocol.GateDecision

// gateWait is why an agent should not stop yet on one blueprint.
type gateWait struct {
	hold     bool   // waiting on the peer: park
	reminder string // forgot a step: block once
	peer     string
	name     string
	phase    int
	note     string // hold-expired text when not about a blueprint phase
}

// handleGateStop answers a host's Stop hook: block with unread mail, hold an
// agent waiting on its blueprint peer, remind once of a missed step, or allow.
func (s *server) handleGateStop(w http.ResponseWriter, r *http.Request) {
	var req protocol.GateStopRequest
	if err := decodeBody(r, &req); err != nil {
		badReq(w, err)
		return
	}
	if req.WorkspaceRoot == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "workspace_root is required")
		return
	}
	target := hosts.Find(req.Host)
	if target != nil {
		if target.NormalizeStopActive {
			req.StopHookActive = true
		}
		if target.RequireCompletedStatus && req.Status != "" && req.Status != "completed" {
			writeJSON(w, &gateDecision{Action: "allow"})
			return
		}
	}
	ctx := r.Context()
	agent, status, code, detail := s.resolveAgent(ctx, req.WorkspaceRoot, "", req.WorktreePath, req.Ancestors, true)
	// guessed: several chats share the hook's host process and folder, so
	// agent is a heuristic pick. A guess may decide whether to hold, never
	// whose mail lands in this chat or which blueprint step to remind of.
	guessed := false
	if status != 0 {
		if code == "ambiguous_agent" {
			agent, status = s.resolveGateAmbiguity(ctx, req.WorkspaceRoot, req.WorktreePath, req.Ancestors)
			guessed = true
		}
		if status != 0 {
			writeErr(w, status, code, detail)
			return
		}
	}
	s.touchGate(req.WorkspaceRoot, agent)
	// Subscribe before the first peek so mail landing in between still wakes us.
	ch, cancel := s.hub.wait(hubKey(req.WorkspaceRoot, agent))
	defer cancel()
	// On a guess, mail already waiting doesn't block: a wrong guess would
	// re-wake this chat on every stop for mail that isn't its own. Only
	// mail arriving during a hold wakes it, once per arrival.
	var d *gateDecision
	if !guessed {
		var err error
		if d, err = s.peekMail(ctx, req.WorkspaceRoot, agent); err != nil {
			storeErr(w, err)
			return
		}
	}
	immediate := d != nil
	if d == nil {
		waits, err := s.gateWaits(ctx, req.WorkspaceRoot, agent)
		if err != nil {
			storeErr(w, err)
			return
		}
		waiters, err := s.store.ListSeriesWaiters(ctx, req.WorkspaceRoot)
		if err != nil {
			storeErr(w, err)
			return
		}
		if slices.Contains(waiters, agent) && s.anyoneElse(ctx, req.WorkspaceRoot, agent) {
			// Alone in the workspace, nobody can open a series: let it stop.
			waits = append(waits, gateWait{hold: true,
				note: "Breakroom: still waiting for a series to join; end your turn to keep waiting."})
		}
		d = s.decide(ctx, req.WorkspaceRoot, agent, waits, req.StopHookActive || guessed, holdFor(req.HoldSec), ch)
	}
	if guessed && d.Kind == "mail" {
		// Wake without content or ack: the chat's next Breakroom call
		// carries its caller_id, so its own mail arrives there.
		d.Reason, d.SuppressAck = guessedWakeReason, true
	}
	if s.mailRun(hubKey(req.WorkspaceRoot, agent), req.StopHookActive, immediate) > maxMailRun {
		d = &gateDecision{Action: "allow"} // mail stays unread for the next turn
	}
	// Remember the text the host is actually sent (Muse gets a static
	// wakeup), so a host re-submitting it isn't taken for the human.
	sent := d.Reason
	if target != nil && target.StaticWakeupReason != "" && d.Kind == "mail" && !guessed {
		sent = target.StaticWakeupReason
	}
	if d.Action == "block" {
		if err := s.store.RecordEmission(ctx, req.WorkspaceRoot, agent, sent); err != nil {
			s.log.Error("gate emission record failed", "agent", agent, "error", err)
		}
	}
	d.Agent = agent
	if d.MessageIDs == nil {
		d.MessageIDs = []int64{}
	}
	if target != nil {
		if d.Action == "block" {
			d.HostPayload = target.FormatStopPayload(sent)
		}
		if target.SuppressGateMailAck {
			d.SuppressAck = true
		}
	} else if d.Action == "block" {
		d.HostPayload = map[string]string{"decision": "block", "reason": d.Reason}
	}
	writeJSON(w, d)
}

// mailRun counts consecutive immediate-mail stops in a hook-continued turn;
// a fresh turn or a park resets it.
func (s *server) mailRun(key string, active, immediate bool) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.mailRuns[key]
	if !active || !immediate {
		n = 0
	}
	if immediate {
		n++
	}
	s.mailRuns[key] = n
	return n
}

// holdFor turns the hook's hold in seconds into a duration: the default when
// unset, at most an hour.
func holdFor(sec int) time.Duration {
	if sec <= 0 {
		return defaultGateHold
	}
	return time.Duration(min(sec, 3600)) * time.Second
}

// decide applies hold > reminder > allow over the agent's blueprints.
func (s *server) decide(ctx context.Context, ws, agent string, waits []gateWait, stopHookActive bool, hold time.Duration, ch <-chan struct{}) *gateDecision {
	for _, wt := range waits {
		if wt.hold {
			return s.park(ctx, ws, agent, wt, hold, ch)
		}
	}
	if !stopHookActive {
		for _, wt := range waits {
			if wt.reminder != "" {
				return &gateDecision{Action: "block", Kind: "reminder", Reason: wt.reminder}
			}
		}
	}
	return &gateDecision{Action: "allow"}
}

// park holds until mail arrives (block with it) or the hold expires (block
// with a keep-waiting note). A cancelled request allows the stop.
func (s *server) park(ctx context.Context, ws, agent string, wt gateWait, hold time.Duration, ch <-chan struct{}) *gateDecision {
	key := hubKey(ws, agent)
	s.mu.Lock()
	s.parked[key]++
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.parked[key]--
		if s.parked[key] <= 0 {
			delete(s.parked, key)
		}
		s.mu.Unlock()
	}()
	timer := time.NewTimer(hold)
	defer timer.Stop()
	for {
		select {
		case <-ch:
			d, err := s.peekMail(ctx, ws, agent)
			if err != nil {
				return &gateDecision{Action: "allow"}
			}
			if d != nil {
				return d
			}
		case <-timer.C:
			if d, err := s.peekMail(ctx, ws, agent); err == nil && d != nil {
				return d
			}
			reason := wt.note
			if reason == "" {
				reason = fmt.Sprintf("Breakroom: still waiting on %s for blueprint %s phase %d; end your turn to keep waiting.",
					wt.peer, wt.name, wt.phase)
			}
			return &gateDecision{Action: "block", Kind: "hold_expired", Reason: reason}
		case <-ctx.Done():
			return &gateDecision{Action: "allow"}
		}
	}
}

// peekMail returns a mail decision for unread mail, or nil when there is none.
func (s *server) peekMail(ctx context.Context, ws, agent string) (*gateDecision, error) {
	msgs, err := s.store.PeekUnread(ctx, ws, agent)
	if err != nil || len(msgs) == 0 {
		return nil, err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Breakroom: %d new message(s) for %s.\n\n", len(msgs), agent)
	d := &gateDecision{Action: "block", Kind: "mail"}
	for _, m := range msgs {
		j := toMessageJSON(m)
		b.WriteString(render.FormatMessage(j))
		d.MessageIDs = append(d.MessageIDs, m.ID)
	}
	d.Reason = b.String()
	return d, nil
}

// gateWaits classifies each open blueprint the agent is part of, in name
// order. Blueprints waiting on the human (pending roadmap, blocked, closed)
// contribute nothing, so the agent may stop and the human sees it.
func (s *server) gateWaits(ctx context.Context, ws, agent string) ([]gateWait, error) {
	list, err := s.store.ListBlueprints(ctx, ws)
	if err != nil {
		return nil, err
	}
	var out []gateWait
	for _, hdr := range list {
		switch hdr.Status {
		case store.BlueprintDrafting, store.BlueprintActive:
		case store.BlueprintAcceptance, store.BlueprintAccepted:
			if agent != hdr.Builder {
				continue // the architect relays to the human, who isn't gated
			}
		default:
			continue
		}
		if agent != hdr.Architect && agent != hdr.Builder {
			continue
		}
		bp, err := s.store.GetBlueprint(ctx, ws, hdr.Name)
		if err != nil {
			return nil, err
		}
		rms, err := s.store.ListRoadmaps(ctx, ws, hdr.Name)
		if err != nil {
			return nil, err
		}
		var latest *store.Roadmap
		if len(rms) > 0 {
			latest = &rms[len(rms)-1]
		}
		// Roadmap pending or nothing specced: the architect gets the human's
		// approval in chat; the builder parks so what follows wakes it.
		pending := (latest != nil && latest.ApprovedAt == nil) || bp.CurrentPhase == 0
		if pending && agent != bp.Builder {
			continue
		}
		n := bp.CurrentPhase
		var ph store.Phase
		if n > 0 {
			ph = bp.Phases[n-1]
		}
		wt := gateWait{name: bp.Name, phase: n}
		switch {
		case agent == bp.Builder && ph.Status == store.PhaseInspection:
			wt.hold, wt.peer = true, bp.Architect
		case agent == bp.Architect && (ph.Status == store.PhaseConstructing || ph.Status == store.PhasePunchlist):
			wt.hold, wt.peer = true, bp.Builder
		case agent == bp.Builder && (ph.Status == store.PhaseConstructing || ph.Status == store.PhasePunchlist):
			wt.reminder = fmt.Sprintf("Breakroom: blueprint %s phase %d is still open. Call blueprint_submit, or send_message the architect, before stopping.", bp.Name, n)
		case pending: // after the open-phase reminder: a revision mid-phase doesn't pause the build
			wt.hold, wt.peer = true, bp.Architect
			wt.note = fmt.Sprintf("Breakroom: waiting on %s for the blueprint %s roadmap or next spec. End your turn to keep waiting.",
				bp.Architect, bp.Name)
		case agent == bp.Builder && ph.Status == store.PhaseSignoff:
			// Next spec, acceptance, landing or close comes as mail: park
			// until handover, or it lands in an idle session.
			wt.hold, wt.peer = true, bp.Architect
			wt.note = fmt.Sprintf("Breakroom: blueprint %s phase %d is signed off; waiting on %s for what's next. End your turn to keep waiting.",
				bp.Name, n, bp.Architect)
		case agent == bp.Architect && ph.Status == store.PhaseInspection:
			wt.reminder = fmt.Sprintf("Breakroom: blueprint %s phase %d awaits your inspection. Call blueprint_inspect before stopping.", bp.Name, n)
		case agent == bp.Architect && ph.Status == store.PhaseSignoff && bp.Status == store.BlueprintActive:
			if latest == nil || len(latest.Phases) > n {
				wt.reminder = fmt.Sprintf("Breakroom: blueprint %s phase %d is signed off. Spec phase %d or call blueprint_close.", bp.Name, n, n+1)
			} else if bp.Series {
				wt.reminder = fmt.Sprintf("Breakroom: blueprint %s is complete. Call blueprint_request_acceptance and relay its handoff to the human.", bp.Name)
			} else {
				wt.reminder = fmt.Sprintf("Breakroom: blueprint %s phase %d is signed off. Call blueprint_close.", bp.Name, n)
			}
		default:
			continue
		}
		if wt.hold && !s.online(ctx, ws, wt.peer) {
			// Waiting on a peer that's gone would park forever: say so once,
			// then let the agent stop so the human sees it.
			wt.hold = false
			wt.reminder = fmt.Sprintf("Breakroom: %s (your peer on blueprint %s) is offline, so it can't move. "+
				"Tell the human, then end your turn.", wt.peer, bp.Name)
		}
		out = append(out, wt)
	}
	return out, nil
}

// online reports whether agent is connected; lookup failures count as
// online so the gate keeps its usual hold.
func (s *server) online(ctx context.Context, ws, agent string) bool {
	a, err := s.store.GetAgent(ctx, ws, agent)
	return err != nil || a.Status != "offline"
}

// anyoneElse reports whether another agent is connected in ws.
func (s *server) anyoneElse(ctx context.Context, ws, agent string) bool {
	agents, err := s.store.ListAgents(ctx, ws, false)
	if err != nil {
		return true
	}
	return slices.ContainsFunc(agents, func(a *store.Agent) bool { return a.AgentID != agent })
}

// resolveGateAmbiguity guesses which of several agents sharing a host process
// is stopping, using recent stop hooks, recent tool calls, parked agents,
// blueprint waits, unread mail, and status, in that order.
func (s *server) resolveGateAmbiguity(ctx context.Context, ws, worktree string, ancestors []int64) (string, int) {
	agents, err := s.store.ListAgents(ctx, ws, false)
	if err != nil {
		return "", http.StatusInternalServerError
	}
	if len(ancestors) > 0 {
		agents = notForeign(agents, ancestors)
	}
	// Keep resolveAgent's folder narrowing: a guess never crosses worktrees.
	if worktree != "" && canonical(worktree) != canonical(ws) {
		if mine := agentsContaining(agents, worktree); len(mine) > 0 {
			agents = mine
		}
	}
	if len(agents) == 0 {
		return "", http.StatusNotFound
	}
	if len(agents) == 1 {
		return agents[0].AgentID, 0
	}

	// 1. Concurrent / duplicate stop hooks: if a stop hook resolved for an agent
	// in this workspace very recently (within 3s), and no peer has called an MCP tool
	// since, this request is part of the same stop event. Both hooks must resolve to
	// the same agent to prevent cross-agent contamination and dropped holds.
	s.mu.Lock()
	recentGateAgent := s.lastGateAgent[ws]
	recentGateAt := s.lastGateAt[ws]
	s.mu.Unlock()

	if recentGateAgent != "" && time.Since(recentGateAt) < 3*time.Second {
		hasNewerCall := false
		s.mu.Lock()
		for _, a := range agents {
			if a.AgentID != recentGateAgent {
				if t, ok := s.lastCallAt[hubKey(ws, a.AgentID)]; ok && t.After(recentGateAt) {
					hasNewerCall = true
					break
				}
			}
		}
		s.mu.Unlock()
		if !hasNewerCall {
			for _, a := range agents {
				if a.AgentID == recentGateAgent {
					return a.AgentID, 0
				}
			}
		}
	}

	// 2. Check if one of the candidates was the recent MCP caller (within 2 minutes).
	// An agent actively generating tokens and concluding its turn calls MCP tools
	// (blueprint_submit, blueprint_spec, send_message, etc.).
	var (
		bestCaller string
		bestCallAt time.Time
	)
	s.mu.Lock()
	for _, a := range agents {
		if t, ok := s.lastCallAt[hubKey(ws, a.AgentID)]; ok {
			if bestCallAt.IsZero() || t.After(bestCallAt) {
				bestCallAt = t
				bestCaller = a.AgentID
			}
		}
	}
	s.mu.Unlock()

	if bestCaller != "" && time.Since(bestCallAt) < 2*time.Minute {
		return bestCaller, 0
	}

	// 3. Exclude agents already holding in s.park (parked from an earlier turn).
	var unparked []*store.Agent
	for _, a := range agents {
		if !s.isParked(ws, a.AgentID) {
			unparked = append(unparked, a)
		}
	}
	if len(unparked) == 1 {
		return unparked[0].AgentID, 0
	}
	if len(unparked) > 1 {
		agents = unparked
	}

	// 4. Blueprint wait state: the agent whose turn just finished has hold: true
	// (waiting on their peer), while the peer has an action reminder or mail.
	seriesWaiters, _ := s.store.ListSeriesWaiters(ctx, ws)
	var holders []string
	for _, a := range agents {
		waits, err := s.gateWaits(ctx, ws, a.AgentID)
		if err == nil {
			isHolder := false
			for _, wt := range waits {
				if wt.hold {
					isHolder = true
					break
				}
			}
			if !isHolder && slices.Contains(seriesWaiters, a.AgentID) && s.anyoneElse(ctx, ws, a.AgentID) {
				isHolder = true
			}
			if isHolder {
				holders = append(holders, a.AgentID)
			}
		}
	}
	if len(holders) == 1 {
		return holders[0], 0
	}

	// 5. Prefer the agent without unread mail (the peer with mail is the one being woken up).
	var withoutMail []string
	for _, a := range agents {
		d, err := s.peekMail(ctx, ws, a.AgentID)
		if err == nil && d == nil {
			withoutMail = append(withoutMail, a.AgentID)
		}
	}
	if len(withoutMail) == 1 {
		return withoutMail[0], 0
	}

	// 6. Prefer an agent in "waiting" status over "active".
	var waiting []string
	for _, a := range agents {
		if a.Status == "waiting" {
			waiting = append(waiting, a.AgentID)
		}
	}
	if len(waiting) == 1 {
		return waiting[0], 0
	}

	return agents[0].AgentID, 0
}
