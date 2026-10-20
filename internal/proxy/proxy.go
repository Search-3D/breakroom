package proxy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/search-3d/breakroom/internal/daemon"
	"github.com/search-3d/breakroom/internal/paths"
	"github.com/search-3d/breakroom/internal/version"
)

// envelope is the tool result shape from docs/tool-contracts.md. Errors ride
// inside it (ok:false) so agents always parse one shape.
type envelope struct {
	OK       bool      `json:"ok"`
	Data     any       `json:"data"`
	Err      string    `json:"error"`
	Unread   []Message `json:"_unread_messages"`
	Warnings []Warning `json:"_warnings"`
}

// ok wraps data in a successful tool response.
func ok(data any) *envelope {
	return &envelope{OK: true, Data: data, Unread: []Message{}, Warnings: []Warning{}}
}

// fail builds a failed tool response with an error code.
func fail(code, detail string) *envelope {
	return &envelope{Data: map[string]any{}, Err: code + ": " + detail, Unread: []Message{}, Warnings: []Warning{}}
}

// failErr turns an error into a failed tool response, keeping daemon error codes.
func failErr(err error) *envelope {
	var derr *Error
	if errors.As(err, &derr) {
		return fail(derr.Code, derr.Detail)
	}
	return fail("internal", err.Error())
}

// session is one MCP connection. The SDK runs tool calls concurrently, so
// agents (one per chat sharing this process, set by register_agent) is
// guarded by mu and blueprint transitions serialize on bpMu.
type session struct {
	client *Client
	// workspace is the default for registration only; each identity keeps
	// the workspace it registered in (see agents), since chats sharing
	// this process may register in different repos.
	workspace string
	mu        sync.Mutex
	bpMu      sync.Mutex
	agents    map[string]string
	keys      map[string]string // id -> worktree+"\x00"+role, to spot a bare re-register
	sawCaller atomic.Bool       // a call has passed caller_id (see piggybackFor)
}

// caller returns which registered agent this tool call acts for: the caller_id,
// or the only agent registered here. With several and no caller_id it fails
// rather than guess. retryable means nothing is registered yet.
func (s *session) caller(ctx context.Context) (id, workspace string, retryable bool, e *envelope) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if want := callerFromCtx(ctx); want != "" {
		ws, ok := s.agents[want]
		if !ok {
			return "", "", false, fail("unknown_caller", "caller_id "+want+" was not registered through this process; call register_agent first")
		}
		s.sawCaller.Store(true)
		return want, ws, false, nil
	}
	switch len(s.agents) {
	case 0:
		return "", "", true, fail("not_registered", "call register_agent first")
	case 1:
		for id, ws := range s.agents {
			return id, ws, false, nil
		}
	}
	ids := make([]string, 0, len(s.agents))
	for id := range s.agents {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return "", "", false, fail("caller_required", "pass caller_id (one of: "+strings.Join(ids, ", ")+")")
}

// bpCall runs blueprint tool calls one at a time per session, retrying briefly
// when a register_agent sent in the same batch hasn't landed yet.
func (s *session) bpCall(ctx context.Context, fn func(id, workspace string) (*envelope, error)) *envelope {
	for attempt := range 3 {
		s.bpMu.Lock()
		id, ws, retryable, e := s.caller(ctx)
		if e != nil {
			s.bpMu.Unlock()
			if retryable && attempt < 2 {
				time.Sleep(15 * time.Millisecond)
				continue
			}
			return e
		}
		env, err := fn(id, ws)
		s.bpMu.Unlock()
		if err == nil {
			return s.piggybackFor(ctx, ws, id, env)
		}
		var derr *Error
		if attempt < 2 && errors.As(err, &derr) &&
			(derr.Code == "unknown_blueprint" || derr.Code == "roadmap_unapproved" || derr.Code == "bad_transition") {
			time.Sleep(15 * time.Millisecond)
			continue
		}
		return failErr(err)
	}
	return fail("internal", "unreachable")
}

// piggybackFor attaches id's unread mail to a tool response (marking it read).
// A call without caller_id, once others have used one, gets none: it may be
// another chat, and its owner would lose that mail.
func (s *session) piggybackFor(ctx context.Context, workspace, id string, e *envelope) *envelope {
	if callerFromCtx(ctx) == "" && s.sawCaller.Load() {
		return e
	}
	msgs, _, err := s.client.Take(ctx, workspace, id, "", nil)
	if err != nil {
		return e
	}
	e.Unread = msgs
	return e
}

type registerArgs struct {
	Role         string `json:"role" jsonschema:"your role from the user's instructions (e.g. builder, architect, or solo); if none was given, ask the user whether you are flying solo or joining an architect on a blueprint instead of inventing one like general"`
	PlanMarkdown string `json:"plan_markdown,omitempty" jsonschema:"initial execution plan"`
	WorktreePath string `json:"worktree_path" jsonschema:"your working directory (absolute, or relative to your cwd), under the workspace root"`
	ProviderHint string `json:"provider_hint,omitempty" jsonschema:"host hint, e.g. claude-code"`
	Name         string `json:"name,omitempty" jsonschema:"resume your earlier offline identity, e.g. otter-417 (keeps inbox and blueprint roles)"`
	Series       string `json:"series,omitempty" jsonschema:"the series (blueprint) the human assigned you, e.g. api-debugging; resumes your identity in it"`
}

// handleRegister is register_agent: it registers this chat's agent and
// remembers it for later calls from this process.
func (s *session) handleRegister(ctx context.Context, _ *mcp.CallToolRequest, args registerArgs) (*mcp.CallToolResult, *envelope, error) {
	// The proxy runs in the agent's cwd; the daemon does not. Resolve "." and
	// friends here, where they mean what the agent meant.
	if args.WorktreePath != "" && !filepath.IsAbs(args.WorktreePath) {
		if abs, err := filepath.Abs(args.WorktreePath); err == nil {
			args.WorktreePath = abs
		}
	}
	s.bpMu.Lock()
	key := args.WorktreePath + "\x00" + args.Role
	s.mu.Lock()
	// A re-register frees its old identity first: the daemon only resumes an
	// offline one. The old identity is named by caller_id or name, or, for a
	// bare register, is the one here with the same worktree and role (how a
	// single-chat host re-registers). Any other register is a new chat
	// sharing this process, and the other identities stay live.
	old := callerFromCtx(ctx)
	if _, held := s.agents[args.Name]; old == "" && held {
		old = args.Name
	}
	if old == "" && args.Name == "" {
		for id, k := range s.keys {
			if k == key {
				old = id
				break
			}
		}
	}
	oldWS, held := s.agents[old]
	if held {
		delete(s.agents, old)
		delete(s.keys, old)
	}
	// The workspace comes from the worktree, resolved as the CLI and hooks
	// resolve their cwd (git common root, else the folder itself), not from
	// the proxy's cwd: hosts like Cursor start it in $HOME, and a series in a
	// $HOME workspace would git-init the home directory. Computed per
	// register, not stored: another chat's repo must not leak into this one.
	ws := s.workspace
	if args.WorktreePath != "" {
		ws = paths.ResolveWorkspace(args.WorktreePath)
	}
	s.mu.Unlock()
	if held {
		_ = s.client.Release(ctx, oldWS, old)
	}
	id, peers, unread, err := s.client.Register(ctx, ws, args.WorktreePath, args.Role, args.PlanMarkdown, args.ProviderHint, args.Name, args.Series, int64(os.Getpid()), int64(os.Getppid()))
	if err != nil {
		s.bpMu.Unlock()
		return nil, failErr(err), nil
	}
	s.mu.Lock()
	if s.agents == nil {
		s.agents, s.keys = map[string]string{}, map[string]string{}
	}
	s.agents[id], s.keys[id] = ws, key
	s.mu.Unlock()
	s.bpMu.Unlock()
	// This call created id, so its mail is ours even without caller_id.
	return nil, s.piggybackFor(withCaller(ctx, id), ws, id, ok(map[string]any{
		"agent_id": id, "peers": peers, "unread_count": unread,
	})), nil
}

type listArgs struct {
	IncludeOffline bool `json:"include_offline,omitempty" jsonschema:"include offline agents"`
}

// handleList is list_agents.
func (s *session) handleList(ctx context.Context, _ *mcp.CallToolRequest, args listArgs) (*mcp.CallToolResult, *envelope, error) {
	id, ws, _, e := s.caller(ctx)
	if e != nil {
		return nil, e, nil
	}
	agents, err := s.client.ListAgents(ctx, ws, id, args.IncludeOffline)
	if err != nil {
		return nil, failErr(err), nil
	}
	return nil, s.piggybackFor(ctx, ws, id, ok(map[string]any{"agents": agents})), nil
}

type updateArgs struct {
	Status       *string `json:"status,omitempty" jsonschema:"idle, active, waiting, or completed"`
	CurrentStep  *string `json:"current_step,omitempty" jsonschema:"active milestone"`
	PlanMarkdown *string `json:"plan_markdown,omitempty" jsonschema:"full plan replacement"`
}

// handleUpdate is update_plan: publish status, current step, and plan.
func (s *session) handleUpdate(ctx context.Context, _ *mcp.CallToolRequest, args updateArgs) (*mcp.CallToolResult, *envelope, error) {
	id, ws, _, e := s.caller(ctx)
	if e != nil {
		return nil, e, nil
	}
	agent, warnings, err := s.client.Update(ctx, ws, id, args.Status, args.CurrentStep, args.PlanMarkdown)
	if err != nil {
		return nil, failErr(err), nil
	}
	e = ok(map[string]any{"agent": agent})
	e.Warnings = warnings
	return nil, s.piggybackFor(ctx, ws, id, e), nil
}

type sendArgs struct {
	ToAgent          string   `json:"to_agent" jsonschema:"agent id or * for broadcast"`
	Content          string   `json:"content" jsonschema:"message body"`
	MsgType          string   `json:"msg_type,omitempty" jsonschema:"chat, handoff, conflict_alert, or review_req"`
	IncludeDiffPaths []string `json:"include_diff_paths,omitempty" jsonschema:"attach sender diff for these paths"`
	AwaitReply       bool     `json:"await_reply,omitempty" jsonschema:"block for an answer"`
	TimeoutSec       int      `json:"timeout_sec,omitempty" jsonschema:"max block when awaiting (capped at the broker hold, default 45); on timed_out the reply will arrive via wait_for_message"`
}

// handleSend is send_message, optionally waiting for the reply.
func (s *session) handleSend(ctx context.Context, _ *mcp.CallToolRequest, args sendArgs) (*mcp.CallToolResult, *envelope, error) {
	id, ws, _, e := s.caller(ctx)
	if e != nil {
		return nil, e, nil
	}
	resp, err := s.client.Send(ctx, SendRequest{
		Workspace: ws, From: id, To: args.ToAgent,
		Type: args.MsgType, Content: args.Content,
		IncludeDiffPaths: args.IncludeDiffPaths,
		AwaitReply:       args.AwaitReply, TimeoutSec: args.TimeoutSec,
	})
	if err != nil {
		return nil, failErr(err), nil
	}
	return nil, s.piggybackFor(ctx, ws, id, ok(map[string]any{
		"message_ids": resp.MessageIDs, "reply": resp.Reply, "timed_out": resp.TimedOut,
	})), nil
}

type waitArgs struct {
	TimeoutSec int `json:"timeout_sec,omitempty" jsonschema:"max wait for this call in seconds (capped at the broker hold, default 45); re-issue on timed_out"`
}

// handleWait is one daemon hold (at most wait_hold_max_sec): MCP hosts kill
// long tool calls, so the agent re-issues on timed_out.
func (s *session) handleWait(ctx context.Context, _ *mcp.CallToolRequest, args waitArgs) (*mcp.CallToolResult, *envelope, error) {
	id, ws, _, e := s.caller(ctx)
	if e != nil {
		return nil, e, nil
	}
	msgs, timedOut, err := s.client.Wait(ctx, ws, id, args.TimeoutSec)
	if err != nil {
		return nil, failErr(err), nil
	}
	if msgs == nil {
		msgs = []Message{}
	}
	return nil, ok(map[string]any{"messages": msgs, "timed_out": timedOut}), nil
}

type diffArgs struct {
	AgentID          string   `json:"agent_id" jsonschema:"target agent"`
	FilePaths        []string `json:"file_paths,omitempty" jsonschema:"only these files or directories"`
	IncludeUntracked *bool    `json:"include_untracked,omitempty" jsonschema:"include untracked files (default true)"`
	Summarize        bool     `json:"summarize,omitempty" jsonschema:"condense via the configured LLM; without one, fallback: true and you summarize the diff"`
}

// handleDiff is get_diff: another agent's uncommitted diff, untracked files
// included by default.
func (s *session) handleDiff(ctx context.Context, _ *mcp.CallToolRequest, args diffArgs) (*mcp.CallToolResult, *envelope, error) {
	id, ws, _, e := s.caller(ctx)
	if e != nil {
		return nil, e, nil
	}
	includeUntracked := true
	if args.IncludeUntracked != nil {
		includeUntracked = *args.IncludeUntracked
	}
	res, err := s.client.Diff(ctx, ws, args.AgentID, args.FilePaths, includeUntracked, args.Summarize)
	if err != nil {
		return nil, failErr(err), nil
	}
	data := map[string]any{
		"diff": res.Diff, "truncated": res.Truncated, "files": res.Files, "summary": res.Summary,
		"fallback": res.Fallback,
	}
	// Peer-delegation hint. With no LLM configured, the diff's author is
	// the best summarizer: the change is in its hot context, so its
	// summary is cheaper and better than the caller reading the raw diff.
	// "ask" names that author so the caller can send_message it
	// (await_reply) instead of burning its own context.
	//
	// Deliberately a hint, not a dispatch. A broker-run "pending task" queue
	// (claim, lease, timeout, pick the next idle agent) was considered and
	// rejected: it duplicates mail + await_reply, interrupts the peer, and
	// makes the deterministic broker schedule model work. The caller
	// decides; ignoring "ask" and reading the diff is always fine.
	//
	// Lives in the proxy, not the daemon: only the session knows the
	// caller, and self-diffs get no hint. To remove it, delete this block,
	// authorOnline, plus the "ask" mentions in docs/tool-contracts.md and
	// SKILL.md; nothing else reads it.
	//
	// An offline author gets no hint either, so the caller falls back to
	// reading the diff itself. Another peer is no better than the caller:
	// it would read the same raw diff without the author's context.
	if res.Fallback && args.AgentID != id && s.authorOnline(ctx, ws, id, args.AgentID) {
		data["ask"] = args.AgentID
	}
	return nil, s.piggybackFor(ctx, ws, id, ok(data)), nil
}

// authorOnline reports whether id is a non-offline agent in this workspace.
// Errors count as offline: the hint is optional and must never fail
// get_diff.
func (s *session) authorOnline(ctx context.Context, workspace, caller, id string) bool {
	agents, err := s.client.ListAgents(ctx, workspace, caller, false)
	if err != nil {
		return false
	}
	for _, a := range agents {
		if a.AgentID == id {
			return true
		}
	}
	return false
}

type conflictsArgs struct {
	TargetAgentID string `json:"target_agent_id" jsonschema:"agent to compare against"`
}

// handleConflicts is check_conflicts between the caller and another agent.
func (s *session) handleConflicts(ctx context.Context, _ *mcp.CallToolRequest, args conflictsArgs) (*mcp.CallToolResult, *envelope, error) {
	id, ws, _, e := s.caller(ctx)
	if e != nil {
		return nil, e, nil
	}
	conflicts, overlap, err := s.client.CheckConflicts(ctx, ws, id, args.TargetAgentID)
	if err != nil {
		return nil, failErr(err), nil
	}
	return nil, s.piggybackFor(ctx, ws, id, ok(map[string]any{
		"conflicts": conflicts, "overlap_only": overlap,
	})), nil
}

type compactArgs struct {
	Limit int `json:"limit,omitempty" jsonschema:"condense at most N recent messages"`
}

// handleCompact is compact_thread: a digest of recent workspace mail.
func (s *session) handleCompact(ctx context.Context, _ *mcp.CallToolRequest, args compactArgs) (*mcp.CallToolResult, *envelope, error) {
	id, ws, _, e := s.caller(ctx)
	if e != nil {
		return nil, e, nil
	}
	res, err := s.client.Compact(ctx, ws, args.Limit)
	if err != nil {
		return nil, failErr(err), nil
	}
	return nil, s.piggybackFor(ctx, ws, id, ok(map[string]any{
		"digest": res.Digest, "message_count": res.MessageCount, "fallback": res.Fallback,
	})), nil
}

type bpInitArgs struct {
	Name    string `json:"name" jsonschema:"blueprint name, unique in this workspace"`
	Builder string `json:"builder,omitempty" jsonschema:"builder agent id; omit to start a series the builder joins by name (Breakroom creates its worktree)"`
	Goal    string `json:"goal,omitempty" jsonschema:"overall goal"`
}

type bpJoinArgs struct {
	Name string `json:"name,omitempty" jsonschema:"the series to join, from the human's instructions; omit to join the only open one"`
}

// handleBlueprintJoin is blueprint_join: a builder joins a series by name.
func (s *session) handleBlueprintJoin(ctx context.Context, _ *mcp.CallToolRequest, args bpJoinArgs) (*mcp.CallToolResult, *envelope, error) {
	return nil, s.bpCall(ctx, func(id, ws string) (*envelope, error) {
		res, err := s.client.JoinBlueprint(ctx, ws, id, args.Name)
		if err != nil {
			return nil, err
		}
		return ok(res), nil
	}), nil
}

// handleBlueprintInit is blueprint_init: the architect starts a blueprint.
func (s *session) handleBlueprintInit(ctx context.Context, _ *mcp.CallToolRequest, args bpInitArgs) (*mcp.CallToolResult, *envelope, error) {
	return nil, s.bpCall(ctx, func(id, ws string) (*envelope, error) {
		status, builder, err := s.client.InitBlueprint(ctx, ws, id, args.Builder, args.Name, args.Goal)
		if err != nil {
			return nil, err
		}
		return ok(map[string]any{
			"name": args.Name, "status": status, "builder": builder,
		}), nil
	}), nil
}

type bpRoadmapArgs struct {
	Name       string         `json:"name" jsonschema:"blueprint name"`
	Phases     []RoadmapPhase `json:"phases" jsonschema:"every planned phase, numbered 1..N"`
	OutOfScope string         `json:"out_of_scope,omitempty" jsonschema:"what this blueprint will not do"`
	Base       string         `json:"base,omitempty" jsonschema:"series only: branch to start from instead of the main checkout, e.g. breakroom/previous-series when this work builds on an unlanded series; say so in the roadmap"`
}

// handleBlueprintRoadmap is blueprint_roadmap: propose a roadmap version.
func (s *session) handleBlueprintRoadmap(ctx context.Context, _ *mcp.CallToolRequest, args bpRoadmapArgs) (*mcp.CallToolResult, *envelope, error) {
	return nil, s.bpCall(ctx, func(id, ws string) (*envelope, error) {
		version, handoff, err := s.client.ProposeRoadmap(ctx, ws, args.Name, id, args.Phases, args.OutOfScope, args.Base)
		if err != nil {
			return nil, err
		}
		return ok(map[string]any{"version": version, "status": "pending", "handoff": handoff}), nil
	}), nil
}

type bpApproveArgs struct {
	Name    string `json:"name" jsonschema:"blueprint name"`
	Version int    `json:"version" jsonschema:"roadmap version the human approved"`
	Quote   string `json:"quote" jsonschema:"the human's approving words, quoted from their chat message"`
}

// handleBlueprintApprove is blueprint_approve: record the human's roadmap
// approval, quoting their words.
func (s *session) handleBlueprintApprove(ctx context.Context, _ *mcp.CallToolRequest, args bpApproveArgs) (*mcp.CallToolResult, *envelope, error) {
	return nil, s.bpCall(ctx, func(id, ws string) (*envelope, error) {
		rm, err := s.client.ApproveRoadmap(ctx, ws, args.Name, id, args.Version, "chat", args.Quote)
		if err != nil {
			return nil, err
		}
		data := map[string]any{"version": rm.Version, "approved_via": rm.ApprovedVia}
		if rm.Handoff != "" {
			data["handoff"] = rm.Handoff
		}
		if rm.SeriesError != "" {
			data["series_error"] = rm.SeriesError
		}
		return ok(data), nil
	}), nil
}

type bpAcceptanceArgs struct {
	Name string `json:"name" jsonschema:"blueprint name"`
}

// handleBlueprintAcceptance is blueprint_request_acceptance: hand finished
// work to the human.
func (s *session) handleBlueprintAcceptance(ctx context.Context, _ *mcp.CallToolRequest, args bpAcceptanceArgs) (*mcp.CallToolResult, *envelope, error) {
	return nil, s.bpCall(ctx, func(id, ws string) (*envelope, error) {
		handoff, err := s.client.RequestAcceptance(ctx, ws, args.Name, id)
		if err != nil {
			return nil, err
		}
		return ok(map[string]any{"status": "acceptance", "handoff": handoff}), nil
	}), nil
}

type bpVerdictArgs struct {
	Name    string `json:"name" jsonschema:"blueprint name"`
	Verdict string `json:"verdict" jsonschema:"accept, or changes when the human asked for fixes"`
	Quote   string `json:"quote" jsonschema:"the human's words, quoted from their chat message"`
	Notes   string `json:"notes,omitempty" jsonschema:"changes: what the human wants fixed, in their words; becomes the follow-up phase"`
	Title   string `json:"title,omitempty" jsonschema:"changes: follow-up phase title (default Review fixes (round N))"`
}

// handleBlueprintVerdict is blueprint_verdict: record the human's accept or
// changes.
func (s *session) handleBlueprintVerdict(ctx context.Context, _ *mcp.CallToolRequest, args bpVerdictArgs) (*mcp.CallToolResult, *envelope, error) {
	return nil, s.bpCall(ctx, func(id, ws string) (*envelope, error) {
		res, err := s.client.RecordVerdict(ctx, ws, args.Name, id, args.Verdict, "chat", args.Quote, args.Notes, args.Title)
		if err != nil {
			return nil, err
		}
		data := map[string]any{"status": res.Status, "handoff": res.Handoff}
		if res.FollowUpPhase > 0 {
			data["follow_up_phase"] = res.FollowUpPhase
		}
		return ok(data), nil
	}), nil
}

type bpLandArgs struct {
	Name   string `json:"name" jsonschema:"blueprint name"`
	Method string `json:"method" jsonschema:"merge, branch, pr, or manual (let the human handle landing, close blueprint, and disconnect agents): the human's choice"`
	Ref    string `json:"ref,omitempty" jsonschema:"the result: merge commit, branch name, or PR URL"`
	Quote  string `json:"quote" jsonschema:"the human's words choosing how to land, quoted from their chat message"`
}

// handleBlueprintLand is blueprint_land: record how accepted work landed.
func (s *session) handleBlueprintLand(ctx context.Context, _ *mcp.CallToolRequest, args bpLandArgs) (*mcp.CallToolResult, *envelope, error) {
	return nil, s.bpCall(ctx, func(id, ws string) (*envelope, error) {
		handoff, err := s.client.Land(ctx, ws, args.Name, id, args.Method, args.Ref, "chat", args.Quote)
		if err != nil {
			return nil, err
		}
		return ok(map[string]any{"status": "handover", "handoff": handoff}), nil
	}), nil
}

type bpSeriesArgs struct {
	Name   string `json:"name" jsonschema:"series (blueprint) name"`
	Branch string `json:"branch,omitempty" jsonschema:"branch to create instead of breakroom/<name>"`
	Path   string `json:"path,omitempty" jsonschema:"worktree folder instead of .workspaces/<name> inside the workspace"`
}

// handleBlueprintSeries is blueprint_series: retry creating the series worktree.
func (s *session) handleBlueprintSeries(ctx context.Context, _ *mcp.CallToolRequest, args bpSeriesArgs) (*mcp.CallToolResult, *envelope, error) {
	return nil, s.bpCall(ctx, func(id, ws string) (*envelope, error) {
		res, err := s.client.CreateSeries(ctx, ws, args.Name, id, args.Branch, args.Path)
		if err != nil {
			return nil, err
		}
		return ok(res), nil
	}), nil
}

type bpSpecArgs struct {
	Name         string                 `json:"name" jsonschema:"blueprint name"`
	Phase        int                    `json:"phase" jsonschema:"phase number (current+1)"`
	Title        string                 `json:"title" jsonschema:"phase title"`
	Objective    string                 `json:"objective" jsonschema:"what this phase accomplishes"`
	Requirements []BlueprintRequirement `json:"requirements" jsonschema:"numbered requirements"`
	Acceptance   []BlueprintRequirement `json:"acceptance" jsonschema:"acceptance criteria"`
	Constraints  string                 `json:"constraints,omitempty" jsonschema:"dependencies and restrictions"`
}

// handleBlueprintSpec is blueprint_spec: the architect specs the next phase.
func (s *session) handleBlueprintSpec(ctx context.Context, _ *mcp.CallToolRequest, args bpSpecArgs) (*mcp.CallToolResult, *envelope, error) {
	return nil, s.bpCall(ctx, func(id, ws string) (*envelope, error) {
		status, err := s.client.SpecBlueprint(ctx, ws, args.Name, id, args.Phase, args.Title, args.Objective, args.Requirements, args.Acceptance, args.Constraints)
		if err != nil {
			return nil, err
		}
		return ok(map[string]any{
			"phase": args.Phase, "status": status,
			"next": "end your turn (with hooks, the stop gate holds you at zero tokens until the builder submits the deliverable; without hooks, call wait_for_message)",
		}), nil
	}), nil
}

type bpSubmitArgs struct {
	Name  string          `json:"name" jsonschema:"blueprint name"`
	Phase int             `json:"phase" jsonschema:"phase number"`
	Items []BlueprintItem `json:"items" jsonschema:"one summary per requirement id"`
	Proof string          `json:"proof" jsonschema:"verification evidence (test output, logs)"`
	Notes string          `json:"notes,omitempty" jsonschema:"design decisions and caveats"`
}

// handleBlueprintSubmit is blueprint_submit: the builder delivers a phase.
func (s *session) handleBlueprintSubmit(ctx context.Context, _ *mcp.CallToolRequest, args bpSubmitArgs) (*mcp.CallToolResult, *envelope, error) {
	return nil, s.bpCall(ctx, func(id, ws string) (*envelope, error) {
		status, err := s.client.SubmitBlueprint(ctx, ws, args.Name, id, args.Phase, args.Items, args.Proof, args.Notes)
		if err != nil {
			return nil, err
		}
		return ok(map[string]any{
			"phase": args.Phase, "status": status,
			"next": "end your turn (with hooks, the stop gate holds you at zero tokens until the next phase spec, punch list, or handover; without hooks, call wait_for_message)",
		}), nil
	}), nil
}

type bpInspectArgs struct {
	Name          string             `json:"name" jsonschema:"blueprint name"`
	Phase         int                `json:"phase" jsonschema:"phase number"`
	Verdict       string             `json:"verdict" jsonschema:"signoff or punchlist"`
	AcceptanceIDs []string           `json:"acceptance_ids,omitempty" jsonschema:"cited acceptance ids (signoff)"`
	Findings      []BlueprintFinding `json:"findings,omitempty" jsonschema:"punch list items (punchlist)"`
}

// handleBlueprintInspect is blueprint_inspect: the architect signs off or
// issues a punch list.
func (s *session) handleBlueprintInspect(ctx context.Context, _ *mcp.CallToolRequest, args bpInspectArgs) (*mcp.CallToolResult, *envelope, error) {
	return nil, s.bpCall(ctx, func(id, ws string) (*envelope, error) {
		status, rounds, err := s.client.InspectBlueprint(ctx, ws, args.Name, id, args.Phase, args.Verdict, args.AcceptanceIDs, args.Findings)
		if err != nil {
			return nil, err
		}
		return ok(map[string]any{
			"phase": args.Phase, "status": status, "rounds": rounds,
		}), nil
	}), nil
}

type bpStatusArgs struct {
	Name  string `json:"name,omitempty" jsonschema:"blueprint name (omit to list)"`
	Phase int    `json:"phase,omitempty" jsonschema:"one phase in full detail: spec, full proof, inspections (omit for the overview)"`
}

// handleBlueprintStatus is blueprint_status: the blueprint list, one
// blueprint, or one phase in full.
func (s *session) handleBlueprintStatus(ctx context.Context, _ *mcp.CallToolRequest, args bpStatusArgs) (*mcp.CallToolResult, *envelope, error) {
	return nil, s.bpCall(ctx, func(_, ws string) (*envelope, error) {
		if args.Name == "" {
			list, err := s.client.ListBlueprints(ctx, ws)
			if err != nil {
				return nil, err
			}
			return ok(map[string]any{"blueprints": list}), nil
		}
		bp, err := s.client.GetBlueprint(ctx, ws, args.Name, args.Phase)
		if err != nil {
			return nil, err
		}
		return ok(map[string]any{"blueprint": bp}), nil
	}), nil
}

type bpCloseArgs struct {
	Name string `json:"name" jsonschema:"blueprint name"`
}

// handleBlueprintClose is blueprint_close.
func (s *session) handleBlueprintClose(ctx context.Context, _ *mcp.CallToolRequest, args bpCloseArgs) (*mcp.CallToolResult, *envelope, error) {
	return nil, s.bpCall(ctx, func(id, ws string) (*envelope, error) {
		status, handoff, err := s.client.CloseBlueprint(ctx, ws, args.Name, id)
		if err != nil {
			return nil, err
		}
		data := map[string]any{
			"name": args.Name, "status": status,
		}
		if handoff != "" {
			data["handoff"] = handoff
		}
		return ok(data), nil
	}), nil
}

// newMCPServer builds the MCP server with every Breakroom tool registered.
func newMCPServer(s *session) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "breakroom", Version: version.Version}, nil)
	sp := structuredParams{}
	server.AddReceivingMiddleware(extractCallerID(), decodeStringArgs(sp))
	addTool(server, sp, &mcp.Tool{Name: "register_agent", Description: "Register with the broker and get your agent name (e.g. otter-417). Call once at session start, on its own turn; pass name to resume an earlier identity. Pass the returned agent_id as caller_id on every later Breakroom call."}, s.handleRegister)
	addTool(server, sp, &mcp.Tool{Name: "list_agents", Description: "List agents in this workspace: status, current step, touched files."}, s.handleList)
	addTool(server, sp, &mcp.Tool{Name: "update_plan", Description: "Publish progress when working solo: status, current step, plan. Not needed during a blueprint (blueprint_* calls update your status and heartbeat)."}, s.handleUpdate)
	addTool(server, sp, &mcp.Tool{Name: "send_message", Description: "Send a message to an agent (* broadcasts). Set await_reply to block for an answer (up to the broker hold); on timed_out, call wait_for_message."}, s.handleSend)
	addTool(server, sp, &mcp.Tool{Name: "wait_for_message", Description: "Block until mail arrives or this call times out (at most the broker hold, default 45s). Zero token cost while parked; on timed_out, call again."}, s.handleWait)
	addTool(server, sp, &mcp.Tool{Name: "get_diff", Description: "Show an agent's uncommitted worktree diff (including untracked source files by default) without interrupting them. Note: blueprint_submit already attaches up to 12KB of the builder's diff on the review_req mail, so only call get_diff if diff_attachment was truncated or omitted."}, s.handleDiff)
	addTool(server, sp, &mcp.Tool{Name: "check_conflicts", Description: "Detect conflicting edits between you and another agent before merging."}, s.handleConflicts)
	addTool(server, sp, &mcp.Tool{Name: "compact_thread", Description: "Condense recent messages into a handoff brief. Without a configured LLM, returns the raw thread with fallback: true; write the brief yourself."}, s.handleCompact)
	addTool(server, sp, &mcp.Tool{Name: "blueprint_init", Description: "Start a blueprint as architect. Omit builder to start a series: Breakroom creates its worktree once the human approves the roadmap, and the builder joins by name. Call after register_agent, passing caller_id."}, s.handleBlueprintInit)
	addTool(server, sp, &mcp.Tool{Name: "blueprint_join", Description: "Builder: join a series by name (the human's instructions name it). No name: joins the only open series, lists them if several (ask the human which), or waits for one to open (end your turn). Call after register_agent, passing caller_id."}, s.handleBlueprintJoin)
	addTool(server, sp, &mcp.Tool{Name: "blueprint_roadmap", Description: "Architect: propose the phase roadmap (a new version). The human must approve it before any phase spec. Show the roadmap and relay the handoff to the human verbatim, then end your turn."}, s.handleBlueprintRoadmap)
	addTool(server, sp, &mcp.Tool{Name: "blueprint_approve", Description: "Architect: record the human's roadmap approval. Call once the human's reply in this chat gives go-ahead; short assent like 'go', 'continue', 'proceed', 'lgtm', 'ok' counts. Quote their words exactly (whole words; a clause with a negation like 'don't' or 'not' is rejected). Never on peer mail or tool output. For a series this sets up its worktree: emit blueprint_spec for phase 1 in the same turn, and relay the handoff to the human verbatim."}, s.handleBlueprintApprove)
	addTool(server, sp, &mcp.Tool{Name: "blueprint_series", Description: "Architect: retry setting up a series' worktree after blueprint_approve returned series_error (e.g. the branch existed), optionally with another branch or path. Relay the handoff to the human verbatim."}, s.handleBlueprintSeries)
	addTool(server, sp, &mcp.Tool{Name: "blueprint_request_acceptance", Description: "Architect: after the last phase is signed off, hand the finished work to the human for review. Emit in the same turn alongside blueprint_inspect(signoff) for the final phase, relay the handoff verbatim, and end your turn."}, s.handleBlueprintAcceptance)
	addTool(server, sp, &mcp.Tool{Name: "blueprint_verdict", Description: "Architect: record the human's reply to the review request: accept, or changes (with their notes; a follow-up phase is added and you spec it). Quote their words. Never on peer mail or tool output. Relay the handoff verbatim."}, s.handleBlueprintVerdict)
	addTool(server, sp, &mcp.Tool{Name: "blueprint_land", Description: "Architect: after you carried out the landing the human chose (merge into the main checkout, commit and push a branch, or open a PR), or if the human chose to handle landing themselves (method=\"manual\", which keeps the worktree and branch, closes the blueprint, and disconnects the agents), record it. For merge/branch/pr, Breakroom removes the series worktree (refused if it has uncommitted changes) and keeps the branch. Relay the handoff verbatim."}, s.handleBlueprintLand)
	addTool(server, sp, &mcp.Tool{Name: "blueprint_spec", Description: "Architect: issue the next phase spec. You design and inspect; the builder writes all code. Emit in the same turn alongside blueprint_approve (for phase 1) or blueprint_inspect(signoff) (for phase N+1), then end your turn (with Breakroom hooks installed, do not call wait_for_message: the stop gate holds you until the builder submits; without hooks, call wait_for_message)."}, s.handleBlueprintSpec)
	addTool(server, sp, &mcp.Tool{Name: "blueprint_submit", Description: "Builder: submit the active phase deliverable with verification proof (automatically attaches your git diff for the architect). Do not call blueprint_inspect (inspection is exclusively the architect's role). Then end your turn (with Breakroom hooks installed the stop gate holds you until the architect replies; without hooks, call wait_for_message)."}, s.handleBlueprintSubmit)
	addTool(server, sp, &mcp.Tool{Name: "blueprint_inspect", Description: "Architect ONLY (builders: NEVER call this; you deliver work with blueprint_submit, the architect inspects): sign off a deliverable or issue a punch list. When signing off, emit blueprint_spec for the next phase (or blueprint_request_acceptance after the final phase) in the same turn."}, s.handleBlueprintInspect)
	addTool(server, sp, &mcp.Tool{Name: "blueprint_status", Description: "Blueprint ledger. No name: list blueprints. Name: phase overview (states, rounds). Name + phase: that phase in full (only needed if the deliverable mail truncated the proof)."}, s.handleBlueprintStatus)
	addTool(server, sp, &mcp.Tool{Name: "blueprint_close", Description: "Architect: hand over a blueprint with no active phase."}, s.handleBlueprintClose)
	return server
}

// Run ensures the daemon, serves MCP over stdio, and releases the agent lease
// on exit. Logs go to stderr; stdout is the protocol.
func Run() error {
	if _, err := daemon.EnsureStarted(30 * time.Minute); err != nil {
		return err
	}
	// A daemon left running from before an upgrade speaks an older API.
	// Swap it while nobody is attached; otherwise doctor/status flag it.
	if info, err := daemon.QueryStatus(); err == nil && info.Version != version.Version && info.AgentsPresent == 0 {
		slog.Info("restarting stale daemon", "daemon", info.Version, "binary", version.Version)
		if _, err := daemon.StopIf(info.PID); err != nil {
			return err
		}
		if _, err := daemon.EnsureStarted(30 * time.Minute); err != nil {
			return err
		}
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	client := NewClient()
	client.restart = func() error {
		_, err := daemon.EnsureStarted(30 * time.Minute)
		return err
	}
	s := &session{client: client, workspace: paths.ResolveWorkspace(cwd)}
	defer s.release()
	var transport mcp.Transport = &mcp.StdioTransport{}
	// BREAKROOM_TRACE=1 logs raw JSON-RPC, exactly as the client sent it, to
	// trace-<pid>.log for debugging client-side argument encoding.
	if os.Getenv("BREAKROOM_TRACE") != "" {
		f, err := os.OpenFile(filepath.Join(paths.Dir(), fmt.Sprintf("trace-%d.log", os.Getpid())), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return err
		}
		defer f.Close()
		transport = &mcp.LoggingTransport{Transport: transport, Writer: f}
	}
	return newMCPServer(s).Run(context.Background(), transport)
}

// release marks every agent this process registered as offline, when the
// host closes the session.
func (s *session) release() {
	s.mu.Lock()
	agents := s.agents
	s.agents, s.keys = nil, nil
	s.mu.Unlock()
	if len(agents) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for id, ws := range agents {
		if err := s.client.Release(ctx, ws, id); err != nil {
			slog.Error("release failed", "agent", id, "error", err)
		}
	}
}
