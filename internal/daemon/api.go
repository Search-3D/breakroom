package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/search-3d/breakroom/internal/paths"
	"github.com/search-3d/breakroom/internal/protocol"
	"github.com/search-3d/breakroom/internal/store"
	"golang.org/x/sys/unix"
)

var msgTypes = map[string]bool{
	"chat": true, "handoff": true, "conflict_alert": true, "review_req": true,
}

var agentStatuses = map[string]bool{
	"idle": true, "active": true, "waiting": true, "completed": true, "offline": true,
}

// writeErr sends a JSON error with an HTTP status and a "code: detail" message.
func writeErr(w http.ResponseWriter, status int, code, detail string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code + ": " + detail})
}

// storeErr sends a database failure as a 500.
func storeErr(w http.ResponseWriter, err error) {
	writeErr(w, http.StatusInternalServerError, "store_error", err.Error())
}

// agentErr maps a store error about agent id to 404 unknown_agent or 500.
func agentErr(w http.ResponseWriter, err error, id string) {
	if errors.Is(err, store.ErrNoAgent) {
		writeErr(w, http.StatusNotFound, "unknown_agent", "no "+id+" in this workspace")
		return
	}
	storeErr(w, err)
}

// badReq sends a malformed request as a 400.
func badReq(w http.ResponseWriter, err error) {
	writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
}

// writeJSON sends v as the JSON response body.
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// decodeBody is lenient about unknown fields so a newer client still works
// against an older daemon after a binary upgrade.
func decodeBody(r *http.Request, v any) error {
	return json.NewDecoder(r.Body).Decode(v)
}

// clampTimeout turns a timeout in seconds into a duration: 300s when unset,
// at most an hour.
func clampTimeout(sec int) time.Duration {
	if sec <= 0 {
		sec = 300
	}
	return time.Duration(min(sec, 3600)) * time.Second
}

type messageJSON = protocol.Message

// toMessageJSON converts a stored message to its API shape.
func toMessageJSON(m *store.Message) messageJSON {
	return messageJSON{
		ID: m.ID, From: m.From, To: m.To, Type: m.Type, Content: m.Content,
		DiffAttachment: m.DiffAttachment, CreatedAt: m.CreatedAt.UTC().Format(time.RFC3339),
	}
}

// toMessageJSONs converts a list of stored messages to their API shape.
func toMessageJSONs(msgs []*store.Message) []messageJSON {
	out := make([]messageJSON, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, toMessageJSON(m))
	}
	return out
}

type agentJSON = protocol.Agent

const maxAgentFiles = 50

// toAgentJSON converts a stored agent to its API shape, capping its file list.
func toAgentJSON(a *store.Agent) agentJSON {
	n := min(len(a.ModifiedFiles), maxAgentFiles)
	files := make([]string, 0, n)
	for _, f := range a.ModifiedFiles[:n] {
		files = append(files, f.Path)
	}
	return agentJSON{
		AgentID: a.AgentID, Role: a.Role, Status: a.Status, CurrentStep: a.CurrentStep,
		WorktreePath: a.WorktreePath, ModifiedFiles: files, LastHeartbeat: a.LastHeartbeat.UTC().Format(time.RFC3339),
	}
}

type peerInfo = protocol.Peer

type warningJSON = protocol.Warning

type registerRequest = protocol.RegisterRequest

// handleRegister registers an agent (or gives a returning one its old name
// back) and returns its id and peers.
func (s *server) handleRegister(w http.ResponseWriter, r *http.Request) {
	var req registerRequest
	if err := decodeBody(r, &req); err != nil {
		badReq(w, err)
		return
	}
	if req.WorkspaceRoot == "" || req.WorktreePath == "" || req.Role == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "workspace_root, worktree_path, and role are required")
		return
	}
	if !filepath.IsAbs(req.WorktreePath) {
		writeErr(w, http.StatusBadRequest, "bad_request", "worktree_path must be absolute (the daemon cannot know whose cwd it is relative to)")
		return
	}
	if !contained(req.WorkspaceRoot, req.WorktreePath) {
		writeErr(w, http.StatusBadRequest, "worktree_escape", "worktree_path must sit under workspace_root")
		return
	}
	a, err := s.store.RegisterAgent(r.Context(), store.RegisterParams{
		WorkspaceRoot: req.WorkspaceRoot, WorktreePath: req.WorktreePath,
		Role: req.Role, PlanMarkdown: req.PlanMarkdown,
		ProviderHint: req.ProviderHint, PID: req.PID, HostPID: req.HostPID, Name: req.Name,
		Series: req.Series,
	})
	switch {
	case errors.Is(err, store.ErrNoAgent):
		writeErr(w, http.StatusNotFound, "unknown_agent", "no "+req.Name+" to resume in this workspace")
		return
	case errors.Is(err, store.ErrNameInUse):
		writeErr(w, http.StatusConflict, "name_in_use", req.Name+" is live; only an offline identity can be resumed")
		return
	case err != nil:
		storeErr(w, err)
		return
	}
	s.touchCaller(req.WorkspaceRoot, a.AgentID)
	agents, err := s.store.ListAgents(r.Context(), req.WorkspaceRoot, false)
	if err != nil {
		storeErr(w, err)
		return
	}
	visible := s.visiblePeers(r.Context(), req.WorkspaceRoot, a.AgentID, agents)
	peers := make([]peerInfo, 0, len(visible))
	for _, p := range visible {
		peers = append(peers, peerInfo{
			AgentID: p.AgentID, Role: p.Role, Status: p.Status, CurrentStep: p.CurrentStep,
		})
	}
	unread, err := s.store.PeekUnread(r.Context(), req.WorkspaceRoot, a.AgentID)
	if err != nil {
		storeErr(w, err)
		return
	}
	writeJSON(w, map[string]any{
		"agent_id": a.AgentID, "peers": peers, "unread_count": len(unread),
	})
}

// agentsContaining returns the agents whose worktree is dir or the nearest
// enclosing ancestor of it.
func agentsContaining(agents []*store.Agent, dir string) []*store.Agent {
	dir = canonical(dir)
	var out []*store.Agent
	bestLen := -1
	for _, a := range agents {
		wt := canonical(a.WorktreePath)
		if dir == wt || strings.HasPrefix(dir, strings.TrimSuffix(wt, string(filepath.Separator))+string(filepath.Separator)) {
			if len(wt) > bestLen {
				bestLen = len(wt)
				out = []*store.Agent{a}
			} else if len(wt) == bestLen {
				out = append(out, a)
			}
		}
	}
	return out
}

// contained reports whether path p is root or inside it.
func contained(root, p string) bool {
	root = canonical(root)
	p = canonical(p)
	if p == root {
		return true
	}
	if !strings.HasSuffix(root, string(filepath.Separator)) {
		root += string(filepath.Separator)
	}
	if strings.HasPrefix(p, root) {
		return true
	}
	// Sibling worktrees of one repo share its common git dir.
	ours, theirs := paths.GitCommonRoot(root), paths.GitCommonRoot(p)
	return ours != "" && ours == theirs
}

// canonical resolves symlinks (macOS /var -> /private/var, /tmp -> ...) so
// logically identical paths compare equal. Unresolvable paths fall back to
// a lexical clean.
func canonical(p string) string {
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved
	}
	return filepath.Clean(p)
}

// handleRelease marks an agent offline when its session ends.
func (s *server) handleRelease(w http.ResponseWriter, r *http.Request) {
	var req protocol.ReleaseRequest
	if err := decodeBody(r, &req); err != nil {
		badReq(w, err)
		return
	}
	if req.WorkspaceRoot == "" || req.AgentID == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "workspace_root and agent_id are required")
		return
	}
	// Idempotent: releasing twice (or a reaped agent) is success.
	if err := s.store.ReleaseAgent(r.Context(), req.WorkspaceRoot, req.AgentID); err != nil &&
		!errors.Is(err, store.ErrNoAgent) {
		storeErr(w, err)
		return
	}
	writeJSON(w, map[string]any{})
}

// handleListAgents lists the agents in a workspace, scoped to who the caller may see.
func (s *server) handleListAgents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	ws := q.Get("workspace_root")
	if ws == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "workspace_root is required")
		return
	}
	includeOffline, _ := strconv.ParseBool(q.Get("include_offline"))
	agents, err := s.store.ListAgents(r.Context(), ws, includeOffline)
	if err != nil {
		storeErr(w, err)
		return
	}
	if caller := q.Get("caller"); caller != "" {
		// The caller's own row stays: list_agents always showed it.
		visible := s.visiblePeers(r.Context(), ws, caller, agents)
		for _, a := range agents {
			if a.AgentID == caller {
				visible = append([]*store.Agent{a}, visible...)
			}
		}
		agents = visible
	}
	out := make([]agentJSON, 0, len(agents))
	for _, a := range agents {
		out = append(out, toAgentJSON(a))
	}
	writeJSON(w, map[string]any{"agents": out})
}

// handleUpdate records an agent's status, current step, and changed files, and
// warns when those files overlap another agent's.
func (s *server) handleUpdate(w http.ResponseWriter, r *http.Request) {
	var req protocol.UpdateRequest
	if err := decodeBody(r, &req); err != nil {
		badReq(w, err)
		return
	}
	if req.WorkspaceRoot == "" || req.AgentID == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "workspace_root and agent_id are required")
		return
	}
	if req.Status == nil && req.CurrentStep == nil && req.PlanMarkdown == nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "at least one field to update is required")
		return
	}
	if req.Status != nil && !agentStatuses[*req.Status] {
		writeErr(w, http.StatusBadRequest, "bad_request", "bad status "+*req.Status)
		return
	}
	a0, err := s.store.GetAgent(r.Context(), req.WorkspaceRoot, req.AgentID)
	if err != nil {
		agentErr(w, err, req.AgentID)
		return
	}
	// Modified files refresh server-side from git status.
	upd := store.AgentUpdate{
		Status: req.Status, CurrentStep: req.CurrentStep, PlanMarkdown: req.PlanMarkdown,
	}
	var extra []warningJSON
	if files, code := refreshFiles(r.Context(), a0.WorktreePath); code != "" {
		extra = append(extra, warningJSON{Code: code, Files: []string{}})
	} else if files != nil {
		upd.ModifiedFiles = files
	}
	a, err := s.store.UpdateAgent(r.Context(), req.WorkspaceRoot, req.AgentID, upd)
	if err != nil {
		agentErr(w, err, req.AgentID)
		return
	}
	others, err := s.store.ListAgents(r.Context(), req.WorkspaceRoot, false)
	if err != nil {
		storeErr(w, err)
		return
	}
	warnings := append(extra, overlapWarnings(a, others)...)
	if warnings == nil {
		warnings = []warningJSON{}
	}
	s.touchCaller(req.WorkspaceRoot, req.AgentID)
	writeJSON(w, map[string]any{
		"agent": toAgentJSON(a), "warnings": warnings,
	})
}

// overlapWarnings lists files that me and another agent have both changed.
func overlapWarnings(me *store.Agent, others []*store.Agent) []warningJSON {
	mine := map[string]bool{}
	for _, f := range me.ModifiedFiles {
		mine[f.Path] = true
	}
	files := []string{}
	seen := map[string]bool{}
	for _, o := range others {
		if o.AgentID == me.AgentID {
			continue
		}
		for _, f := range o.ModifiedFiles {
			if mine[f.Path] && !seen[f.Path] {
				seen[f.Path] = true
				files = append(files, f.Path)
			}
		}
	}
	if len(files) == 0 {
		return []warningJSON{}
	}
	sort.Strings(files)
	return []warningJSON{{Code: "file_overlap", Files: files}}
}

type sendRequest = protocol.SendRequest
type sendResponse = protocol.SendResponse

// handleSend delivers a message to one agent or a broadcast, optionally with
// the sender's diff, and can wait for the reply.
func (s *server) handleSend(w http.ResponseWriter, r *http.Request) {
	var req sendRequest
	if err := decodeBody(r, &req); err != nil {
		badReq(w, err)
		return
	}
	if req.WorkspaceRoot == "" || req.From == "" || req.To == "" || req.Content == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "workspace_root, from, to, and content are required")
		return
	}
	if req.Type != "" && !msgTypes[req.Type] {
		writeErr(w, http.StatusBadRequest, "bad_request", "bad msg_type "+req.Type)
		return
	}
	if len(req.Content) > store.MaxContentBytes {
		writeErr(w, http.StatusBadRequest, "bad_request", "content exceeds 64KB")
		return
	}
	if req.AwaitReply && req.To == "*" {
		writeErr(w, http.StatusBadRequest, "bad_request", "await_reply needs a concrete recipient")
		return
	}
	// Load before storing: an error after the send would make a retry
	// deliver the message twice.
	cfg, err := loadConfigFor(req.WorkspaceRoot)
	if err != nil {
		badReq(w, err)
		return
	}
	sender, err := s.store.GetAgent(r.Context(), req.WorkspaceRoot, req.From)
	if err != nil {
		agentErr(w, err, req.From)
		return
	}
	var attachment string
	if len(req.IncludeDiffPaths) > 0 {
		attachment, err = diffAttachment(r.Context(), sender.WorktreePath, req.IncludeDiffPaths)
		if err != nil {
			writeGitErr(w, err)
			return
		}
	}
	var recipients []string
	if req.To == "*" {
		all, err := s.store.ListAgents(r.Context(), req.WorkspaceRoot, false)
		if err != nil {
			storeErr(w, err)
			return
		}
		recipients = []string{}
		for _, a := range s.visiblePeers(r.Context(), req.WorkspaceRoot, req.From, all) {
			recipients = append(recipients, a.AgentID)
		}
	}
	sent, err := s.store.SendMessage(r.Context(), store.SendParams{
		WorkspaceRoot: req.WorkspaceRoot, From: req.From, To: req.To,
		Type: req.Type, Content: req.Content, DiffAttachment: attachment,
		Recipients: recipients,
	})
	if err != nil {
		agentErr(w, err, req.To)
		return
	}
	for _, m := range sent {
		s.hub.signal(hubKey(req.WorkspaceRoot, m.Recipient))
	}
	s.touchCaller(req.WorkspaceRoot, req.From)
	resp := sendResponse{MessageIDs: []int64{}}
	for _, m := range sent {
		resp.MessageIDs = append(resp.MessageIDs, m.ID)
	}
	if req.AwaitReply {
		// Held at most wait_hold_max_sec (MCP hosts kill long tool calls);
		// clients re-wait via /v1/messages/await until their budget is spent.
		hold := time.Duration(cfg.WaitHoldMaxSec) * time.Second
		// A reply is anything from the peer after the question itself; ids
		// are assigned in commit order, so earlier mail can't pass for one.
		reply, timedOut := s.awaitReply(r.Context(), req.WorkspaceRoot, req.From, req.To, resp.MessageIDs[0], min(clampTimeout(req.TimeoutSec), hold))
		resp.Reply, resp.TimedOut = reply, timedOut
	}
	writeJSON(w, resp)
}

// awaitReply waits for a reply from `to` to `from`, ignoring older mail
// (id <= afterID) and leaving other mail unread. It returns false on timeout.
func (s *server) awaitReply(ctx context.Context, ws, from, to string, afterID int64, timeout time.Duration) (*messageJSON, bool) {
	ch, cancel := s.hub.wait(hubKey(ws, from))
	defer cancel()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for done := false; ; {
		if peek, err := s.store.PeekUnread(ctx, ws, from); err == nil {
			for _, m := range peek {
				if m.ID > afterID && m.From == to {
					_ = s.store.MarkRead(ctx, ws, from, []int64{m.ID})
					j := toMessageJSON(m)
					return &j, false
				}
			}
		}
		if done {
			return nil, true
		}
		select {
		case <-ch:
		case <-timer.C:
			done = true // one last look, then give up
		case <-ctx.Done():
			return nil, true
		}
	}
}

// handleAwait re-waits for a reply after an await_reply send timed out its
// hold: the first message from `from` to `recipient` with id > after_id.
func (s *server) handleAwait(w http.ResponseWriter, r *http.Request) {
	var req protocol.AwaitRequest
	if err := decodeBody(r, &req); err != nil {
		badReq(w, err)
		return
	}
	if req.WorkspaceRoot == "" || req.Recipient == "" || req.From == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "workspace_root, recipient, and from are required")
		return
	}
	if _, err := s.store.GetAgent(r.Context(), req.WorkspaceRoot, req.Recipient); err != nil {
		agentErr(w, err, req.Recipient)
		return
	}
	cfg, err := loadConfigFor(req.WorkspaceRoot)
	if err != nil {
		badReq(w, err)
		return
	}
	hold := time.Duration(cfg.WaitHoldMaxSec) * time.Second
	reply, timedOut := s.awaitReply(r.Context(), req.WorkspaceRoot, req.Recipient, req.From, req.AfterID, min(clampTimeout(req.TimeoutSec), hold))
	writeJSON(w, map[string]any{"reply": reply, "timed_out": timedOut})
}

// handleWait holds until mail arrives for the recipient or the hold ends,
// then returns the unread mail.
func (s *server) handleWait(w http.ResponseWriter, r *http.Request) {
	var req protocol.WaitRequest
	if err := decodeBody(r, &req); err != nil {
		badReq(w, err)
		return
	}
	if req.WorkspaceRoot == "" || req.Recipient == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "workspace_root and recipient are required")
		return
	}
	if _, err := s.store.GetAgent(r.Context(), req.WorkspaceRoot, req.Recipient); err != nil {
		agentErr(w, err, req.Recipient)
		return
	}
	cfg, err := loadConfigFor(req.WorkspaceRoot)
	if err != nil {
		badReq(w, err)
		return
	}
	hold := time.Duration(cfg.WaitHoldMaxSec) * time.Second
	// Subscribe before draining, and stay subscribed: mail landing between a
	// drain and the park still wakes us. Persisted mail drains first, so a
	// restart never loses messages.
	ch, cancel := s.hub.wait(hubKey(req.WorkspaceRoot, req.Recipient))
	defer cancel()
	timer := time.NewTimer(min(clampTimeout(req.TimeoutSec), hold))
	defer timer.Stop()
	for done := false; ; {
		msgs, err := s.store.TakeUnread(r.Context(), req.WorkspaceRoot, req.Recipient)
		if err != nil {
			storeErr(w, err)
			return
		}
		if len(msgs) > 0 || done {
			writeJSON(w, map[string]any{
				"messages": toMessageJSONs(msgs), "timed_out": len(msgs) == 0,
			})
			return
		}
		if note := s.builderAcceptanceNote(r.Context(), req.WorkspaceRoot, req.Recipient); note != "" {
			writeJSON(w, map[string]any{
				"messages": []messageJSON{{
					From:      "broker",
					To:        req.Recipient,
					Type:      "blueprint_status",
					Content:   note,
					CreatedAt: time.Now().UTC().Format(time.RFC3339),
				}},
				"timed_out": false,
			})
			return
		}
		select {
		case <-ch: // may be a stale wake; the next drain decides
		case <-timer.C:
			done = true // one last drain, then report the timeout
		case <-r.Context().Done():
			return // client gone: a drain now would mark mail read unseen
		}
	}
}

// builderAcceptanceNote tells a builder whose blueprint awaits human review to
// stop waiting and end its turn. "" when that doesn't apply.
func (s *server) builderAcceptanceNote(ctx context.Context, ws, agent string) string {
	list, err := s.store.ListBlueprints(ctx, ws)
	if err != nil {
		return ""
	}
	for _, hdr := range list {
		if hdr.Builder == agent && (hdr.Status == store.BlueprintAcceptance || hdr.Status == store.BlueprintAccepted) {
			return "### [BLUEPRINT] " + hdr.Name + " - All phases signed off\n\nAll phases are signed off and awaiting human acceptance review. Do not call wait_for_message again; end your turn and stand by."
		}
	}
	return ""
}

// handleTake returns the caller's unread mail and marks it read.
func (s *server) handleTake(w http.ResponseWriter, r *http.Request) {
	var req protocol.TakeRequest
	if err := decodeBody(r, &req); err != nil {
		badReq(w, err)
		return
	}
	if req.WorkspaceRoot == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "workspace_root is required")
		return
	}
	recipient, status, code, detail := s.resolveAgent(r.Context(), req.WorkspaceRoot, req.Recipient, req.WorktreePath, req.Ancestors, false)
	if status != 0 {
		writeErr(w, status, code, detail)
		return
	}
	take := s.store.TakeUnread
	if req.Peek {
		take = s.store.PeekUnread
	}
	msgs, err := take(r.Context(), req.WorkspaceRoot, recipient)
	if err != nil {
		storeErr(w, err)
		return
	}
	writeJSON(w, map[string]any{
		"messages": toMessageJSONs(msgs), "recipient": recipient,
	})
}

// resolveAgent works out which agent a hook or CLI call is for: the named
// recipient, else the agent whose host process is an ancestor, else the agent
// whose worktree holds the folder (strict stops there). status 0 means success.
func (s *server) resolveAgent(ctx context.Context, ws, recipient, worktree string, ancestors []int64, strict bool) (string, int, string, string) {
	if recipient != "" {
		if _, err := s.store.GetAgent(ctx, ws, recipient); err != nil {
			if errors.Is(err, store.ErrNoAgent) {
				return "", http.StatusNotFound, "unknown_agent", "no " + recipient + " in this workspace"
			}
			return "", http.StatusInternalServerError, "store_error", err.Error()
		}
		return recipient, 0, "", ""
	}
	agents, err := s.store.ListAgents(ctx, ws, false)
	if err != nil {
		return "", http.StatusInternalServerError, "store_error", err.Error()
	}
	if id := byHost(agents, ancestors); id != "" {
		return id, 0, "", ""
	}
	if len(ancestors) > 0 {
		agents = notForeign(agents, ancestors)
	}
	// Hooks cannot name their agent but run in its project directory:
	// prefer the one live agent whose worktree contains the hint.
	if strict {
		// At the workspace root with several agents, the folder names none of them.
		if worktree == "" || canonical(worktree) != canonical(ws) || len(agents) <= 1 {
			agents = agentsContaining(agents, worktree)
		}
	} else if len(agents) > 1 && worktree != "" {
		if canonical(worktree) != canonical(ws) {
			if mine := agentsContaining(agents, worktree); len(mine) == 1 {
				agents = mine
			}
		}
	}
	switch len(agents) {
	case 0:
		return "", http.StatusNotFound, "no_agents", "no active agents in this workspace"
	case 1:
		return agents[0].AgentID, 0, "", ""
	default:
		return "", http.StatusConflict, "ambiguous_agent", "multiple agents active; pass recipient"
	}
}

// byHost returns the agent whose host process is the nearest of ancestors,
// or "" when none is, or when two agents share that host (one host process
// running two registrations: the caller falls back to folders).
func byHost(agents []*store.Agent, ancestors []int64) string {
	for _, pid := range ancestors {
		var match []string
		for _, a := range agents {
			if a.HostPID != nil && *a.HostPID == pid {
				match = append(match, a.AgentID)
			}
		}
		switch len(match) {
		case 0:
			continue
		case 1:
			return match[0]
		default:
			return ""
		}
	}
	return ""
}

// processAlive reports whether pid is a live process (tests stub it).
var processAlive = func(pid int64) bool {
	err := unix.Kill(int(pid), 0)
	return err == nil || errors.Is(err, unix.EPERM)
}

// notForeign drops agents whose host process is alive but not an ancestor of
// the caller: the caller is provably not them, so it must not take their mail.
func notForeign(agents []*store.Agent, ancestors []int64) []*store.Agent {
	var out []*store.Agent
	for _, a := range agents {
		if a.HostPID == nil || slices.Contains(ancestors, *a.HostPID) || !processAlive(*a.HostPID) {
			out = append(out, a)
		}
	}
	return out
}

// handlePromptRecord stores a prompt the human typed, as reported by the
// host's prompt hook, for the agent working in worktree_path.
func (s *server) handlePromptRecord(w http.ResponseWriter, r *http.Request) {
	var req protocol.PromptRecordRequest
	if err := decodeBody(r, &req); err != nil {
		badReq(w, err)
		return
	}
	if req.WorkspaceRoot == "" || req.WorktreePath == "" || req.Text == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "workspace_root, worktree_path, and text are required")
		return
	}
	agent, status, code, detail := s.resolveAgent(r.Context(), req.WorkspaceRoot, "", req.WorktreePath, req.Ancestors, true)
	guessed := false
	if status != 0 {
		if code == "ambiguous_agent" {
			if a := s.soleArchitect(r.Context(), req.WorkspaceRoot, req.WorktreePath, req.Ancestors); a != "" {
				agent, status, guessed = a, 0, true
			}
		}
		if status != 0 {
			writeErr(w, status, code, detail)
			return
		}
	}
	recorded, err := s.store.RecordPrompt(r.Context(), req.WorkspaceRoot, agent, req.Text)
	if err != nil {
		storeErr(w, err)
		return
	}
	if guessed {
		// Recorded for approvals, but not named: the caller must not
		// deliver that agent's mail into a chat that may not be its own.
		agent = ""
	}
	writeJSON(w, map[string]any{"agent": agent, "recorded": recorded})
}

// soleArchitect picks who a prompt from a shared host process belongs to:
// the one architect among the candidates, or nobody if there are several,
// so one architect's chat never backs another's approval.
func (s *server) soleArchitect(ctx context.Context, ws, worktree string, ancestors []int64) string {
	agents, err := s.store.ListAgents(ctx, ws, false)
	if err != nil {
		return ""
	}
	if len(ancestors) > 0 {
		agents = notForeign(agents, ancestors)
	}
	if canonical(worktree) != canonical(ws) {
		if mine := agentsContaining(agents, worktree); len(mine) > 0 {
			agents = mine
		}
	}
	found := ""
	for _, a := range agents {
		if a.Role == "architect" {
			if found != "" {
				return ""
			}
			found = a.AgentID
		}
	}
	return found
}

// handleAck marks mail read once a hook has delivered it (at-least-once).
func (s *server) handleAck(w http.ResponseWriter, r *http.Request) {
	var req protocol.AckRequest
	if err := decodeBody(r, &req); err != nil {
		badReq(w, err)
		return
	}
	if req.WorkspaceRoot == "" || req.Recipient == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "workspace_root and recipient are required")
		return
	}
	if err := s.store.MarkRead(r.Context(), req.WorkspaceRoot, req.Recipient, req.IDs); err != nil {
		storeErr(w, err)
		return
	}
	writeJSON(w, map[string]any{})
}
