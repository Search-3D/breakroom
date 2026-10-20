// Package protocol defines shared request/response wire contracts between the
// breakroom CLI, proxy, and daemon.
package protocol

// Message is one delivered message.
type Message struct {
	ID             int64  `json:"id"`
	From           string `json:"from"`
	To             string `json:"to"`
	Type           string `json:"type"`
	Content        string `json:"content"`
	DiffAttachment string `json:"diff_attachment,omitempty"`
	CreatedAt      string `json:"created_at"`
}

// Agent is one registered agent in a workspace.
type Agent struct {
	AgentID       string   `json:"agent_id"`
	Role          string   `json:"role"`
	Status        string   `json:"status"`
	CurrentStep   string   `json:"current_step"`
	WorktreePath  string   `json:"worktree_path"`
	ModifiedFiles []string `json:"modified_files"`
	LastHeartbeat string   `json:"last_heartbeat"`
}

// Peer is one agent shown at registration.
type Peer struct {
	AgentID     string `json:"agent_id"`
	Role        string `json:"role"`
	Status      string `json:"status"`
	CurrentStep string `json:"current_step"`
}

// Warning is one overlap or conflict warning.
type Warning struct {
	Code  string   `json:"code"`
	Files []string `json:"files"`
}

// GateStopRequest asks the stop gate whether the agent in worktree may stop.
type GateStopRequest struct {
	WorkspaceRoot  string  `json:"workspace_root"`
	WorktreePath   string  `json:"worktree_path"`
	Host           string  `json:"host,omitempty"`
	Status         string  `json:"status,omitempty"`
	Ancestors      []int64 `json:"ancestors,omitempty"`
	StopHookActive bool    `json:"stop_hook_active"`
	HoldSec        int     `json:"hold_sec"`
}

// GateDecision is the stop gate's verdict for a host Stop hook.
type GateDecision struct {
	Action      string  `json:"action"`         // allow|block
	Kind        string  `json:"kind,omitempty"` // mail|hold_expired|reminder, "" on allow
	Reason      string  `json:"reason,omitempty"`
	Agent       string  `json:"agent,omitempty"`
	MessageIDs  []int64 `json:"message_ids"`
	SuppressAck bool    `json:"suppress_ack,omitempty"`
	HostPayload any     `json:"host_payload,omitempty"`
}

// PromptRecordRequest reports a prompt the human typed.
type PromptRecordRequest struct {
	WorkspaceRoot string  `json:"workspace_root"`
	WorktreePath  string  `json:"worktree_path"`
	Host          string  `json:"host,omitempty"`
	Text          string  `json:"text"`
	Ancestors     []int64 `json:"ancestors,omitempty"`
}

// PromptRecordResponse is returned by /v1/prompts/record.
type PromptRecordResponse struct {
	Agent    string `json:"agent"`
	Recorded bool   `json:"recorded"`
	Output   string `json:"output,omitempty"`
}

// AckRequest acknowledges receipt of messages.
type AckRequest struct {
	WorkspaceRoot string  `json:"workspace_root"`
	Recipient     string  `json:"recipient"`
	IDs           []int64 `json:"ids"`
}

// RoadmapPhase is one planned phase in a roadmap.
type RoadmapPhase struct {
	Phase    int    `json:"phase" jsonschema:"phase number, 1..N in order"`
	Title    string `json:"title" jsonschema:"phase title; its spec must use the same title"`
	Delivers string `json:"delivers,omitempty" jsonschema:"what the phase delivers"`
}

// Roadmap is one proposed version of a blueprint's phase plan.
type Roadmap struct {
	Version       int            `json:"version"`
	Phases        []RoadmapPhase `json:"phases"`
	OutOfScope    string         `json:"out_of_scope"`
	Base          string         `json:"base,omitempty"`
	ProposedAt    string         `json:"proposed_at"`
	ApprovedAt    *string        `json:"approved_at"`
	ApprovedVia   string         `json:"approved_via"`
	ApprovalQuote string         `json:"approval_quote"`
}

// BlueprintRequirement is one spec item (requirement or acceptance).
type BlueprintRequirement struct {
	ID   string `json:"id" jsonschema:"stable id, e.g. r1"`
	Text string `json:"text" jsonschema:"requirement text"`
}

// BlueprintItem maps one requirement id to what the builder did.
type BlueprintItem struct {
	RequirementID string `json:"requirement_id" jsonschema:"spec requirement id"`
	Summary       string `json:"summary" jsonschema:"what was implemented"`
}

// BlueprintFinding is one punch-list item.
type BlueprintFinding struct {
	RequirementID string `json:"requirement_id" jsonschema:"spec requirement id"`
	Issue         string `json:"issue" jsonschema:"what is wrong"`
	Fix           string `json:"fix" jsonschema:"required remediation"`
}

// BlueprintDeliverable is a builder's submission for a phase.
type BlueprintDeliverable struct {
	Items []BlueprintItem `json:"items"`
	Proof string          `json:"proof"`
	Notes string          `json:"notes"`
}

// BlueprintInspection is one recorded architect verdict.
type BlueprintInspection struct {
	Verdict       string             `json:"verdict"`
	AcceptanceIDs []string           `json:"acceptance_ids,omitempty"`
	Findings      []BlueprintFinding `json:"findings,omitempty"`
	At            string             `json:"at"`
}

// BlueprintPhase is one phase: slim in the overview, full in phase detail.
type BlueprintPhase struct {
	Phase        int                    `json:"phase"`
	Title        string                 `json:"title"`
	Objective    string                 `json:"objective,omitempty"`
	Requirements []BlueprintRequirement `json:"requirements,omitempty"`
	Acceptance   []BlueprintRequirement `json:"acceptance,omitempty"`
	Constraints  string                 `json:"constraints,omitempty"`
	Status       string                 `json:"status"`
	Rounds       int                    `json:"rounds"`
	Deliverable  *BlueprintDeliverable  `json:"deliverable,omitempty"`
	Inspections  []BlueprintInspection  `json:"inspections,omitempty"`
	UpdatedAt    string                 `json:"updated_at"`
}

// Blueprint is one named Architect/Builder run; Phases is set on detail.
type Blueprint struct {
	Name         string           `json:"name"`
	Architect    string           `json:"architect"`
	Builder      string           `json:"builder"`
	Goal         string           `json:"goal"`
	Status       string           `json:"status"`
	CurrentPhase int              `json:"current_phase"`
	Series       bool             `json:"series"`
	WorktreePath string           `json:"worktree_path,omitempty"`
	Branch       string           `json:"branch,omitempty"`
	BaseRef      string           `json:"base_ref,omitempty"`
	Phases       []BlueprintPhase `json:"phases,omitempty"`
	Roadmaps     []Roadmap        `json:"roadmaps,omitempty"`
}

// BlueprintInitRequest initializes a blueprint.
type BlueprintInitRequest struct {
	WorkspaceRoot string `json:"workspace_root"`
	Name          string `json:"name"`
	Architect     string `json:"architect"`
	Builder       string `json:"builder,omitempty"`
	Goal          string `json:"goal"`
}

// BlueprintInitResponse is the result of initializing a blueprint.
type BlueprintInitResponse struct {
	Name    string `json:"name"`
	Status  string `json:"status"`
	Builder string `json:"builder,omitempty"`
}

// BlueprintRoadmapRequest proposes a new roadmap.
type BlueprintRoadmapRequest struct {
	WorkspaceRoot string         `json:"workspace_root"`
	Name          string         `json:"name"`
	Caller        string         `json:"caller"`
	Phases        []RoadmapPhase `json:"phases"`
	OutOfScope    string         `json:"out_of_scope"`
	Base          string         `json:"base,omitempty"`
}

// BlueprintRoadmapResponse is the result of proposing a roadmap.
type BlueprintRoadmapResponse struct {
	Version int    `json:"version"`
	Handoff string `json:"handoff"`
}

// BlueprintApproveRequest approves a roadmap version.
type BlueprintApproveRequest struct {
	WorkspaceRoot string `json:"workspace_root"`
	Name          string `json:"name"`
	Caller        string `json:"caller"`
	Version       int    `json:"version"`
	Via           string `json:"via"`
	Quote         string `json:"quote,omitempty"`
}

// ApproveResult is a recorded roadmap approval.
type ApproveResult struct {
	Version     int    `json:"version"`
	ApprovedVia string `json:"approved_via"`
	Handoff     string `json:"handoff,omitempty"`
	SeriesError string `json:"series_error,omitempty"`
}

// BlueprintSpecRequest specs the next phase.
type BlueprintSpecRequest struct {
	WorkspaceRoot string                 `json:"workspace_root"`
	Name          string                 `json:"name"`
	Caller        string                 `json:"caller"`
	Phase         int                    `json:"phase"`
	Title         string                 `json:"title"`
	Objective     string                 `json:"objective"`
	Requirements  []BlueprintRequirement `json:"requirements"`
	Acceptance    []BlueprintRequirement `json:"acceptance"`
	Constraints   string                 `json:"constraints"`
}

// BlueprintSpecResponse is the result of speccing a phase.
type BlueprintSpecResponse struct {
	Phase  int    `json:"phase"`
	Status string `json:"status"`
}

// BlueprintSubmitRequest is a builder's submission for a phase.
type BlueprintSubmitRequest struct {
	WorkspaceRoot string          `json:"workspace_root"`
	Name          string          `json:"name"`
	Caller        string          `json:"caller"`
	Phase         int             `json:"phase"`
	Items         []BlueprintItem `json:"items"`
	Proof         string          `json:"proof"`
	Notes         string          `json:"notes"`
}

// BlueprintSubmitResponse is the result of submitting a phase deliverable.
type BlueprintSubmitResponse struct {
	Phase  int    `json:"phase"`
	Status string `json:"status"`
	Round  int    `json:"round"`
}

// BlueprintInspectRequest records an architect inspection verdict.
type BlueprintInspectRequest struct {
	WorkspaceRoot string             `json:"workspace_root"`
	Name          string             `json:"name"`
	Caller        string             `json:"caller"`
	Phase         int                `json:"phase"`
	Verdict       string             `json:"verdict"`
	AcceptanceIDs []string           `json:"acceptance_ids"`
	Findings      []BlueprintFinding `json:"findings"`
}

// BlueprintInspectResponse is the result of an inspection.
type BlueprintInspectResponse struct {
	Phase  int    `json:"phase"`
	Status string `json:"status"`
	Rounds int    `json:"rounds"`
}

// BlueprintJoinRequest joins a series.
type BlueprintJoinRequest struct {
	WorkspaceRoot string `json:"workspace_root"`
	Name          string `json:"name"`
	Caller        string `json:"caller"`
}

// JoinResult is the outcome of joining a series.
type JoinResult struct {
	Joined    bool     `json:"joined"`
	Name      string   `json:"name,omitempty"`
	Architect string   `json:"architect,omitempty"`
	Open      []string `json:"open,omitempty"`
	Waiting   bool     `json:"waiting,omitempty"`
	Worktree  string   `json:"worktree,omitempty"`
}

// BlueprintCloseRequest closes a blueprint.
type BlueprintCloseRequest struct {
	WorkspaceRoot string `json:"workspace_root"`
	Name          string `json:"name"`
	Caller        string `json:"caller"`
}

// BlueprintCloseResponse is the result of closing a blueprint.
type BlueprintCloseResponse struct {
	Name    string `json:"name"`
	Status  string `json:"status"`
	Handoff string `json:"handoff,omitempty"`
}

// BlueprintAcceptanceRequest requests acceptance for a completed blueprint.
type BlueprintAcceptanceRequest struct {
	WorkspaceRoot string `json:"workspace_root"`
	Caller        string `json:"caller"`
	Name          string `json:"name"`
}

// BlueprintAcceptanceResponse is the result of requesting acceptance.
type BlueprintAcceptanceResponse struct {
	Status  string `json:"status"`
	Handoff string `json:"handoff"`
}

// BlueprintVerdictRequest records a human acceptance or changes verdict.
type BlueprintVerdictRequest struct {
	WorkspaceRoot string `json:"workspace_root"`
	Name          string `json:"name"`
	Caller        string `json:"caller"`
	Verdict       string `json:"verdict"`
	Via           string `json:"via"`
	Quote         string `json:"quote,omitempty"`
	Notes         string `json:"notes,omitempty"`
	Title         string `json:"title,omitempty"`
}

// VerdictResult is a recorded human verdict on finished work.
type VerdictResult struct {
	Status        string `json:"status"`
	FollowUpPhase int    `json:"follow_up_phase,omitempty"`
	Handoff       string `json:"handoff"`
}

// BlueprintLandRequest records how accepted work landed.
type BlueprintLandRequest struct {
	WorkspaceRoot string `json:"workspace_root"`
	Name          string `json:"name"`
	Method        string `json:"method"`
	Ref           string `json:"ref"`
	Via           string `json:"via"`
	Caller        string `json:"caller"`
	Quote         string `json:"quote"`
}

// BlueprintLandResponse is the result of landing a blueprint.
type BlueprintLandResponse struct {
	Handoff string `json:"handoff"`
	Warning string `json:"warning,omitempty"`
}

// BlueprintSeriesRequest retries setting up a series worktree.
type BlueprintSeriesRequest struct {
	WorkspaceRoot string `json:"workspace_root"`
	Name          string `json:"name"`
	Caller        string `json:"caller"`
	Branch        string `json:"branch,omitempty"`
	Path          string `json:"path,omitempty"`
}

// SeriesResult is a (re)created series worktree.
type SeriesResult struct {
	Worktree string `json:"worktree"`
	Branch   string `json:"branch"`
	Handoff  string `json:"handoff"`
}

// RegisterRequest registers an agent in a workspace.
type RegisterRequest struct {
	WorkspaceRoot string `json:"workspace_root"`
	WorktreePath  string `json:"worktree_path"`
	Role          string `json:"role"`
	PlanMarkdown  string `json:"plan_markdown,omitempty"`
	ProviderHint  string `json:"provider_hint,omitempty"`
	PID           *int64 `json:"pid,omitempty"`
	HostPID       *int64 `json:"host_pid,omitempty"`
	Name          string `json:"name,omitempty"`
	Series        string `json:"series,omitempty"`
}

// ReleaseRequest releases an agent.
type ReleaseRequest struct {
	WorkspaceRoot string `json:"workspace_root"`
	AgentID       string `json:"agent_id"`
}

// UpdateRequest updates agent state.
type UpdateRequest struct {
	WorkspaceRoot string  `json:"workspace_root"`
	AgentID       string  `json:"agent_id"`
	Status        *string `json:"status,omitempty"`
	CurrentStep   *string `json:"current_step,omitempty"`
	PlanMarkdown  *string `json:"plan_markdown,omitempty"`
}

// SendRequest sends a message.
type SendRequest struct {
	WorkspaceRoot    string   `json:"workspace_root"`
	From             string   `json:"from"`
	To               string   `json:"to"`
	Type             string   `json:"msg_type"`
	Content          string   `json:"content"`
	IncludeDiffPaths []string `json:"include_diff_paths,omitempty"`
	AwaitReply       bool     `json:"await_reply,omitempty"`
	TimeoutSec       int      `json:"timeout_sec,omitempty"`
}

// SendResponse is returned after sending a message.
type SendResponse struct {
	MessageIDs []int64  `json:"message_ids"`
	Reply      *Message `json:"reply"`
	TimedOut   bool     `json:"timed_out"`
}

// AwaitRequest waits for a specific reply.
type AwaitRequest struct {
	WorkspaceRoot string `json:"workspace_root"`
	Recipient     string `json:"recipient"`
	From          string `json:"from"`
	AfterID       int64  `json:"after_id,omitempty"`
	TimeoutSec    int    `json:"timeout_sec,omitempty"`
}

// WaitRequest waits for any new messages.
type WaitRequest struct {
	WorkspaceRoot string `json:"workspace_root"`
	Recipient     string `json:"recipient"`
	TimeoutSec    int    `json:"timeout_sec,omitempty"`
}

// TakeRequest takes or peeks unread messages.
type TakeRequest struct {
	WorkspaceRoot string  `json:"workspace_root"`
	Recipient     string  `json:"recipient,omitempty"`
	WorktreePath  string  `json:"worktree_path,omitempty"`
	Ancestors     []int64 `json:"ancestors,omitempty"`
	Peek          bool    `json:"peek,omitempty"`
}

// CompactRequest summarizes thread history.
type CompactRequest struct {
	WorkspaceRoot string `json:"workspace_root"`
	Limit         int    `json:"limit,omitempty"`
}

// DiffRequest inspects diffs.
type DiffRequest struct {
	WorkspaceRoot    string   `json:"workspace_root"`
	AgentID          string   `json:"agent_id"`
	FilePaths        []string `json:"file_paths,omitempty"`
	IncludeUntracked bool     `json:"include_untracked,omitempty"`
	Summarize        bool     `json:"summarize,omitempty"`
}

// ConflictsRequest checks conflicts between agents.
type ConflictsRequest struct {
	WorkspaceRoot string `json:"workspace_root"`
	AgentID       string `json:"agent_id"`
	TargetAgentID string `json:"target_agent_id"`
}

// Conflict is one conflicting path with per-side line ranges (nil when N/A).
type Conflict struct {
	Path        string  `json:"path"`
	OursLines   *string `json:"ours_lines"`
	TheirsLines *string `json:"theirs_lines"`
}

// BlueprintStatusRequest requests status for a blueprint.
type BlueprintStatusRequest struct {
	WorkspaceRoot string `json:"workspace_root"`
	Name          string `json:"name,omitempty"`
	Phase         int    `json:"phase,omitempty"`
}
