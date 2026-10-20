// Package proxy bridges MCP stdio tool calls to the daemon.
package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/search-3d/breakroom/internal/paths"
	"github.com/search-3d/breakroom/internal/protocol"
)

// Wire shapes come from internal/protocol; client_test pins them against
// daemon JSON fixtures, so drift fails loudly.

// Message is one delivered message.
type Message = protocol.Message

// Agent is one listed agent.
type Agent = protocol.Agent

// Peer is one agent shown at registration.
type Peer = protocol.Peer

// Warning is one overlap/conflict warning.
type Warning = protocol.Warning

// SendRequest describes an outbound message.
type SendRequest struct {
	Workspace        string
	From             string
	To               string
	Type             string
	Content          string
	IncludeDiffPaths []string
	AwaitReply       bool
	TimeoutSec       int
}

// SendResponse is the result of a send.
type SendResponse struct {
	MessageIDs []int64  `json:"message_ids"`
	Reply      *Message `json:"reply"`
	TimedOut   bool     `json:"timed_out"`
}

// Client talks to the daemon over its Unix socket.
type Client struct {
	http *http.Client
	// restart, when set (the MCP proxy only; CLI hooks must never spawn
	// a daemon), brings a stopped or crashed daemon back before one retry.
	restart func() error
}

// NewClient dials the daemon socket. Calls have no client-side timeout:
// waits legitimately block, and the caller's context governs.
func NewClient() *Client {
	return &Client{http: &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", paths.Socket())
			},
		},
	}}
}

// Error is a daemon-side failure. Code is stable snake_case.
type Error struct {
	StatusCode int
	Code       string
	Detail     string
}

// Error returns "code: detail".
func (e *Error) Error() string { return e.Code + ": " + e.Detail }

// post sends req as JSON to the daemon and decodes the reply into resp.
func (c *Client) post(ctx context.Context, path string, req, resp any) error {
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(req); err != nil {
		return err
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://localhost"+path, &buf)
	if err != nil {
		return err
	}
	return c.do(r, resp)
}

// get calls a daemon endpoint and decodes the reply into resp.
func (c *Client) get(ctx context.Context, path string, resp any) error {
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://localhost"+path, nil)
	if err != nil {
		return err
	}
	return c.do(r, resp)
}

// do sends a request to the daemon, restarting the daemon and retrying once
// if it couldn't connect. Daemon errors come back as *Error.
func (c *Client) do(r *http.Request, resp any) error {
	res, err := c.http.Do(r)
	// Retry only when the dial failed: the request never reached a daemon,
	// so re-sending cannot duplicate a write.
	var op *net.OpError
	if err != nil && c.restart != nil && errors.As(err, &op) && op.Op == "dial" && r.Context().Err() == nil {
		if c.restart() == nil {
			retry := r.Clone(r.Context())
			if r.GetBody != nil {
				if retry.Body, err = r.GetBody(); err != nil {
					return &Error{Code: "daemon_unreachable", Detail: err.Error()}
				}
			}
			res, err = c.http.Do(retry)
		}
	}
	if err != nil {
		return &Error{Code: "daemon_unreachable", Detail: err.Error()}
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		var body struct {
			Err string `json:"error"`
		}
		_ = json.NewDecoder(res.Body).Decode(&body)
		code, detail := body.Err, ""
		if i := strings.Index(code, ": "); i >= 0 {
			code, detail = code[:i], code[i+2:]
		}
		if code == "" {
			code = "daemon_error"
		}
		return &Error{StatusCode: res.StatusCode, Code: code, Detail: detail}
	}
	return json.NewDecoder(res.Body).Decode(resp)
}

// Register claims an agent identity; name (optional) resumes an offline one.
// hostPID is the process that spawned the proxy: hooks find the agent by it.
// series (optional) resumes that series' offline participant with this role.
func (c *Client) Register(ctx context.Context, workspace, worktree, role, plan, provider, name, series string, pid, hostPID int64) (string, []Peer, int, error) {
	var out struct {
		AgentID     string `json:"agent_id"`
		Peers       []Peer `json:"peers"`
		UnreadCount int    `json:"unread_count"`
	}
	var pPID, pHostPID *int64
	if pid != 0 {
		pPID = &pid
	}
	if hostPID != 0 {
		pHostPID = &hostPID
	}
	err := c.post(ctx, "/v1/agents/register", protocol.RegisterRequest{
		WorkspaceRoot: workspace, WorktreePath: worktree, Role: role,
		PlanMarkdown: plan, ProviderHint: provider, Name: name, PID: pPID,
		HostPID: pHostPID, Series: series,
	}, &out)
	return out.AgentID, out.Peers, out.UnreadCount, err
}

// Release frees an agent slot (idempotent).
func (c *Client) Release(ctx context.Context, workspace, id string) error {
	var out map[string]any
	return c.post(ctx, "/v1/agents/release", protocol.ReleaseRequest{
		WorkspaceRoot: workspace, AgentID: id,
	}, &out)
}

// ListAgents lists agents in a workspace; with caller set, only the ones
// visible to caller (its team).
func (c *Client) ListAgents(ctx context.Context, workspace, caller string, includeOffline bool) ([]Agent, error) {
	var out struct {
		Agents []Agent `json:"agents"`
	}
	path := "/v1/agents?workspace_root=" + url.QueryEscape(workspace) +
		"&include_offline=" + strconv.FormatBool(includeOffline)
	if caller != "" {
		path += "&caller=" + url.QueryEscape(caller)
	}
	err := c.get(ctx, path, &out)
	return out.Agents, err
}

// Update publishes progress; nil fields are kept.
func (c *Client) Update(ctx context.Context, workspace, id string, status, step, plan *string) (Agent, []Warning, error) {
	var out struct {
		Agent    Agent     `json:"agent"`
		Warnings []Warning `json:"warnings"`
	}
	err := c.post(ctx, "/v1/agents/update", protocol.UpdateRequest{
		WorkspaceRoot: workspace, AgentID: id,
		Status: status, CurrentStep: step, PlanMarkdown: plan,
	}, &out)
	return out.Agent, out.Warnings, err
}

// Send delivers a message. With AwaitReply the daemon holds for a reply at
// most wait_hold_max_sec; on timed_out, AwaitReply re-waits.
func (c *Client) Send(ctx context.Context, req SendRequest) (SendResponse, error) {
	var out SendResponse
	err := c.post(ctx, "/v1/messages/send", protocol.SendRequest{
		WorkspaceRoot: req.Workspace, From: req.From, To: req.To,
		Type: req.Type, Content: req.Content,
		IncludeDiffPaths: req.IncludeDiffPaths,
		AwaitReply:       req.AwaitReply, TimeoutSec: req.TimeoutSec,
	}, &out)
	return out, err
}

// AwaitReply waits (one server hold) for the first message from `from` to
// recipient with id > afterID: the reply to a send that timed out.
func (c *Client) AwaitReply(ctx context.Context, workspace, recipient, from string, afterID int64, timeoutSec int) (*Message, bool, error) {
	var out struct {
		Reply    *Message `json:"reply"`
		TimedOut bool     `json:"timed_out"`
	}
	err := c.post(ctx, "/v1/messages/await", protocol.AwaitRequest{
		WorkspaceRoot: workspace, Recipient: recipient, From: from,
		AfterID: afterID, TimeoutSec: timeoutSec,
	}, &out)
	return out.Reply, out.TimedOut, err
}

// Wait blocks (one server hold) until mail arrives or the timeout elapses.
func (c *Client) Wait(ctx context.Context, workspace, recipient string, timeoutSec int) ([]Message, bool, error) {
	var out struct {
		Messages []Message `json:"messages"`
		TimedOut bool      `json:"timed_out"`
	}
	err := c.post(ctx, "/v1/messages/wait", protocol.WaitRequest{
		WorkspaceRoot: workspace, Recipient: recipient, TimeoutSec: timeoutSec,
	}, &out)
	return out.Messages, out.TimedOut, err
}

// Take returns unread mail and marks it read. An empty recipient is resolved
// from ancestors, then worktree; the resolved id is returned too.
func (c *Client) Take(ctx context.Context, workspace, recipient, worktree string, ancestors []int64) ([]Message, string, error) {
	return c.take(ctx, workspace, recipient, worktree, ancestors, false)
}

// Peek is Take without marking read: the caller Acks once delivered, so a
// delivery that dies midway redelivers instead of losing mail.
func (c *Client) Peek(ctx context.Context, workspace, recipient, worktree string, ancestors []int64) ([]Message, string, error) {
	return c.take(ctx, workspace, recipient, worktree, ancestors, true)
}

// take fetches unread mail, marking it read unless peek is set.
func (c *Client) take(ctx context.Context, workspace, recipient, worktree string, ancestors []int64, peek bool) ([]Message, string, error) {
	var out struct {
		Messages  []Message `json:"messages"`
		Recipient string    `json:"recipient"`
	}
	err := c.post(ctx, "/v1/messages/take", protocol.TakeRequest{
		WorkspaceRoot: workspace, Recipient: recipient, WorktreePath: worktree,
		Ancestors: ancestors, Peek: peek,
	}, &out)
	return out.Messages, out.Recipient, err
}

// DiffResult is a capped unified diff.
type DiffResult struct {
	Diff      string   `json:"diff"`
	Files     []string `json:"files"`
	Truncated bool     `json:"truncated"`
	Summary   string   `json:"summary"`
	// Fallback means summarize was asked but no endpoint is configured.
	Fallback bool `json:"fallback"`
}

// Conflict is one conflicting path with per-side line ranges (nil when N/A).
type Conflict = protocol.Conflict

// Diff fetches an agent's worktree diff.
func (c *Client) Diff(ctx context.Context, workspace, agent string, paths []string, untracked, summarize bool) (DiffResult, error) {
	var out DiffResult
	err := c.post(ctx, "/v1/git/diff", protocol.DiffRequest{
		WorkspaceRoot: workspace, AgentID: agent, FilePaths: paths,
		IncludeUntracked: untracked, Summarize: summarize,
	}, &out)
	return out, err
}

// CheckConflicts compares two agents' uncommitted states.
func (c *Client) CheckConflicts(ctx context.Context, workspace, me, target string) ([]Conflict, []string, error) {
	var out struct {
		Conflicts   []Conflict `json:"conflicts"`
		OverlapOnly []string   `json:"overlap_only"`
	}
	err := c.post(ctx, "/v1/git/conflicts", protocol.ConflictsRequest{
		WorkspaceRoot: workspace, AgentID: me, TargetAgentID: target,
	}, &out)
	return out.Conflicts, out.OverlapOnly, err
}

// CompactResult is a handoff digest. With Fallback set, Digest is the raw
// capped transcript because no endpoint is configured.
type CompactResult struct {
	Digest       string `json:"digest"`
	MessageCount int    `json:"message_count"`
	Fallback     bool   `json:"fallback"`
}

// Compact condenses recent messages into a handoff digest.
func (c *Client) Compact(ctx context.Context, workspace string, limit int) (CompactResult, error) {
	var out CompactResult
	err := c.post(ctx, "/v1/messages/compact", protocol.CompactRequest{
		WorkspaceRoot: workspace, Limit: limit,
	}, &out)
	return out, err
}

// Blueprint wire shapes mirror the daemon. The jsonschema tags feed the MCP
// tool schemas; args structs reuse these types directly.

// BlueprintRequirement is one spec item (requirement or acceptance).
type BlueprintRequirement = protocol.BlueprintRequirement

// BlueprintItem maps one requirement id to what the builder did.
type BlueprintItem = protocol.BlueprintItem

// BlueprintFinding is one punch-list item.
type BlueprintFinding = protocol.BlueprintFinding

// BlueprintDeliverable is a builder's submission for a phase.
type BlueprintDeliverable = protocol.BlueprintDeliverable

// BlueprintInspection is one recorded architect verdict.
type BlueprintInspection = protocol.BlueprintInspection

// BlueprintPhase is one phase: slim in the overview, full in phase detail.
type BlueprintPhase = protocol.BlueprintPhase

// Blueprint is one named Architect/Builder run; Phases is set on detail.
type Blueprint = protocol.Blueprint

// RoadmapPhase is one planned phase in a roadmap.
type RoadmapPhase = protocol.RoadmapPhase

// Roadmap is one proposed version of a blueprint's phase plan.
type Roadmap = protocol.Roadmap

// GateDecision is the stop gate's verdict for a host Stop hook.
type GateDecision = protocol.GateDecision

// ApproveResult is a recorded roadmap approval; for a series' first
// approval it carries the worktree handoff, or why setting it up failed.
type ApproveResult = protocol.ApproveResult

// SeriesResult is a (re)created series worktree.
type SeriesResult = protocol.SeriesResult

// JoinResult is the outcome of joining a series.
type JoinResult = protocol.JoinResult

// VerdictResult is a recorded human verdict on finished work.
type VerdictResult = protocol.VerdictResult

// ProposeRoadmap records a new pending roadmap version; caller is the architect.
// It returns the new version and the human's handoff.
func (c *Client) ProposeRoadmap(ctx context.Context, workspace, name, caller string, phases []RoadmapPhase, outOfScope, base string) (int, string, error) {
	var out protocol.BlueprintRoadmapResponse
	err := c.post(ctx, "/v1/blueprints/roadmap", protocol.BlueprintRoadmapRequest{
		WorkspaceRoot: workspace, Name: name, Caller: caller,
		Phases: phases, OutOfScope: outOfScope, Base: base,
	}, &out)
	return out.Version, out.Handoff, err
}

// CreateSeries retries setting up a series' worktree, optionally with
// another branch or path.
func (c *Client) CreateSeries(ctx context.Context, workspace, name, caller, branch, path string) (SeriesResult, error) {
	var out SeriesResult
	err := c.post(ctx, "/v1/blueprints/series", protocol.BlueprintSeriesRequest{
		WorkspaceRoot: workspace, Name: name, Caller: caller, Branch: branch, Path: path,
	}, &out)
	return out, err
}

// ApproveRoadmap approves a roadmap version (0 = latest pending). via is
// "chat" (caller quotes the human) or "cli" (the human's own terminal).
func (c *Client) ApproveRoadmap(ctx context.Context, workspace, name, caller string, version int, via, quote string) (ApproveResult, error) {
	var out ApproveResult
	err := c.post(ctx, "/v1/blueprints/approve", protocol.BlueprintApproveRequest{
		WorkspaceRoot: workspace, Name: name, Caller: caller,
		Version: version, Via: via, Quote: quote,
	}, &out)
	return out, err
}

// RecordPrompt reports a prompt the human typed for the agent in worktree.
func (c *Client) RecordPrompt(ctx context.Context, workspace, worktree, host, text string, ancestors []int64) (string, bool, error) {
	var out protocol.PromptRecordResponse
	err := c.post(ctx, "/v1/prompts/record", protocol.PromptRecordRequest{
		WorkspaceRoot: workspace, WorktreePath: worktree, Host: host, Text: text,
		Ancestors: ancestors,
	}, &out)
	return out.Agent, out.Recorded, err
}

// GateStop asks the stop gate whether the agent in worktree may stop. It may
// hold for up to holdSec.
func (c *Client) GateStop(ctx context.Context, workspace, worktree, host, status string, ancestors []int64, stopHookActive bool, holdSec int) (GateDecision, error) {
	var out GateDecision
	err := c.post(ctx, "/v1/gate/stop", protocol.GateStopRequest{
		WorkspaceRoot: workspace, WorktreePath: worktree, Host: host, Status: status,
		Ancestors: ancestors, StopHookActive: stopHookActive, HoldSec: holdSec,
	}, &out)
	return out, err
}

// Ack marks delivered mail read.
func (c *Client) Ack(ctx context.Context, workspace, recipient string, ids []int64) error {
	return c.post(ctx, "/v1/messages/ack", protocol.AckRequest{
		WorkspaceRoot: workspace, Recipient: recipient, IDs: ids,
	}, &struct{}{})
}

// JoinBlueprint makes caller the builder of series name (or the only open one).
func (c *Client) JoinBlueprint(ctx context.Context, workspace, caller, name string) (JoinResult, error) {
	var out JoinResult
	err := c.post(ctx, "/v1/blueprints/join", protocol.BlueprintJoinRequest{
		WorkspaceRoot: workspace, Caller: caller, Name: name,
	}, &out)
	return out, err
}

// InitBlueprint starts a drafting blueprint; caller is the architect. An
// empty builder starts a series the builder joins by name. Returns the initial
// status and the assigned builder (which may be auto-attached from a previous series).
func (c *Client) InitBlueprint(ctx context.Context, workspace, architect, builder, name, goal string) (string, string, error) {
	var out protocol.BlueprintInitResponse
	err := c.post(ctx, "/v1/blueprints/init", protocol.BlueprintInitRequest{
		WorkspaceRoot: workspace, Name: name,
		Architect: architect, Builder: builder, Goal: goal,
	}, &out)
	return out.Status, out.Builder, err
}

// SpecBlueprint issues the next phase spec.
func (c *Client) SpecBlueprint(ctx context.Context, workspace, name, caller string, phase int, title, objective string, reqs, acc []BlueprintRequirement, constraints string) (string, error) {
	var out protocol.BlueprintSpecResponse
	err := c.post(ctx, "/v1/blueprints/spec", protocol.BlueprintSpecRequest{
		WorkspaceRoot: workspace, Name: name, Caller: caller,
		Phase: phase, Title: title, Objective: objective,
		Requirements: reqs, Acceptance: acc, Constraints: constraints,
	}, &out)
	return out.Status, err
}

// SubmitBlueprint delivers the active phase.
func (c *Client) SubmitBlueprint(ctx context.Context, workspace, name, caller string, phase int, items []BlueprintItem, proof, notes string) (string, error) {
	var out protocol.BlueprintSubmitResponse
	err := c.post(ctx, "/v1/blueprints/submit", protocol.BlueprintSubmitRequest{
		WorkspaceRoot: workspace, Name: name, Caller: caller,
		Phase: phase, Items: items, Proof: proof, Notes: notes,
	}, &out)
	return out.Status, err
}

// InspectBlueprint records a verdict; returns the phase status and rounds.
func (c *Client) InspectBlueprint(ctx context.Context, workspace, name, caller string, phase int, verdict string, accIDs []string, findings []BlueprintFinding) (string, int, error) {
	var out protocol.BlueprintInspectResponse
	err := c.post(ctx, "/v1/blueprints/inspect", protocol.BlueprintInspectRequest{
		WorkspaceRoot: workspace, Name: name, Caller: caller,
		Phase: phase, Verdict: verdict,
		AcceptanceIDs: accIDs, Findings: findings,
	}, &out)
	return out.Status, out.Rounds, err
}

// GetBlueprint returns one blueprint: the phase overview (phase 0) or one
// phase in full detail.
func (c *Client) GetBlueprint(ctx context.Context, workspace, name string, phase int) (Blueprint, error) {
	var out struct {
		Blueprint Blueprint `json:"blueprint"`
	}
	err := c.post(ctx, "/v1/blueprints/status", protocol.BlueprintStatusRequest{
		WorkspaceRoot: workspace, Name: name, Phase: phase,
	}, &out)
	return out.Blueprint, err
}

// ListBlueprints returns the workspace's blueprint headers.
func (c *Client) ListBlueprints(ctx context.Context, workspace string) ([]Blueprint, error) {
	var out struct {
		Blueprints []Blueprint `json:"blueprints"`
	}
	err := c.post(ctx, "/v1/blueprints/status", protocol.BlueprintStatusRequest{
		WorkspaceRoot: workspace,
	}, &out)
	return out.Blueprints, err
}

// CloseBlueprint hands over a blueprint with no active phase. For an unlanded
// series, it returns handoff instructions for the worktree and branch.
func (c *Client) CloseBlueprint(ctx context.Context, workspace, name, caller string) (string, string, error) {
	var out protocol.BlueprintCloseResponse
	err := c.post(ctx, "/v1/blueprints/close", protocol.BlueprintCloseRequest{
		WorkspaceRoot: workspace, Name: name, Caller: caller,
	}, &out)
	return out.Status, out.Handoff, err
}

// RequestAcceptance hands a finished blueprint to the human; it returns the
// handoff to relay.
func (c *Client) RequestAcceptance(ctx context.Context, workspace, name, caller string) (string, error) {
	var out protocol.BlueprintAcceptanceResponse
	err := c.post(ctx, "/v1/blueprints/acceptance", protocol.BlueprintAcceptanceRequest{
		WorkspaceRoot: workspace, Name: name, Caller: caller,
	}, &out)
	return out.Handoff, err
}

// RecordVerdict records the human's accept or changes (via chat or cli).
func (c *Client) RecordVerdict(ctx context.Context, workspace, name, caller, verdict, via, quote, notes, title string) (VerdictResult, error) {
	var out VerdictResult
	err := c.post(ctx, "/v1/blueprints/verdict", protocol.BlueprintVerdictRequest{
		WorkspaceRoot: workspace, Name: name, Caller: caller, Verdict: verdict,
		Via: via, Quote: quote, Notes: notes, Title: title,
	}, &out)
	return out, err
}

// Land records how accepted work landed (method merge|branch|pr|manual, ref)
// and returns the final handoff.
func (c *Client) Land(ctx context.Context, workspace, name, caller, method, ref, via, quote string) (string, error) {
	var out protocol.BlueprintLandResponse
	err := c.post(ctx, "/v1/blueprints/land", protocol.BlueprintLandRequest{
		WorkspaceRoot: workspace, Name: name, Caller: caller,
		Method: method, Ref: ref, Via: via, Quote: quote,
	}, &out)
	if out.Warning != "" {
		out.Handoff += "\n\nWarning: " + out.Warning
	}
	return out.Handoff, err
}
