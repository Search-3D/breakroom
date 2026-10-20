# breakroom tool contracts

Authoritative request/response shapes for every breakroom surface.
This file pins the wire. Verified against
`internal/proxy`, `internal/daemon`, and `internal/store`.

## Conventions

- All MCP tools return one envelope on success:
  `{ "ok": true, "data": {...}, "_unread_messages": [...], "_warnings": [] }`.
- Failures ride inside the same envelope:
  `{ "ok": false, "data": {}, "error": "<code>: <detail>",
  "_unread_messages": [], "_warnings": [] }`.
  `error` is a single `"code: detail"` string, not an object.
- `_unread_messages` is attached to every tool response for a registered
  agent (delivery marks read). `_warnings` carries overlap codes from
  `update_plan` (see below).
- Daemon HTTP errors are `{"error": "code: detail"}` with the status below.
- The daemon ignores unknown JSON fields (a newer client keeps working
  against an older daemon after a binary upgrade).
- Empty results are `ok: true` with an empty value, never an error.
- `caller_id`: every MCP tool except `register_agent` takes the agent id
  registration returned. With one identity in the process it is optional;
  with several it is required (`caller_required`), and an id this process
  never registered is rejected (`unknown_caller`). A process never guesses
  which chat a call belongs to. A `register_agent` with no `name` or
  `caller_id` whose worktree and role match an identity already in the
  process is treated as that identity re-registering and replaces it, so
  two chats sharing one process in the same worktree and role must pass
  `name` (or `caller_id`) to stay distinct.

## Error codes

| code | http | when |
| --- | --- | --- |
| unauthorized | 401 | TCP request without the bearer token |
| bad_request | 400 | missing/invalid fields, bad `msg_type`/`status`, oversize content, `await_reply` to `*` |
| unknown_agent | 404 | agent id not registered in this workspace |
| not_a_repo | 400 | worktree is not a git repo |
| no_commits | 400 | worktree has no commits yet |
| git_too_old | 400 | git predates merge-tree support |
| cannot_compare | 400 | worktrees have no merge base (shallow/unrelated) |
| worktree_escape | 400 | `worktree_path` outside the workspace root |
| store_error | 500 | SQLite failure (detail in the error string) |
| git_missing | 500 | git binary not found |
| git_error | 500 | git subprocess failed unexpectedly |
| llm_error | 502 | sidecar call failed (timeout, 5xx, bad payload, empty reply) |
| no_agents | 404 | `take` without `recipient` and no live agent to default to |
| ambiguous_agent | 409 | `take` without `recipient`, several live agents, no single one whose host process is in `ancestors`, and no single one whose worktree contains `worktree_path` |
| daemon_unreachable | - | proxy could not dial the daemon socket |
| daemon_error | - | daemon replied non-200 without a parsable error |
| internal | - | client-side failure (e.g. no cwd) in the proxy or CLI |
| not_running | - | CLI `status --format json` with no daemon |
| not_registered | - | MCP tool called before `register_agent` |
| caller_required | - | MCP tool without `caller_id` while several identities share the process |
| unknown_caller | - | MCP `caller_id` this process never registered |
| name_in_use | 409 | `register_agent` `name` is a live identity (only offline ones resume) |
| unknown_blueprint | 404 | blueprint name not found in this workspace |
| bad_transition | 409 | call does not match the phase's current state |
| forbidden_role | 403 | caller is not the blueprint's architect/builder for this call |
| blueprint_blocked | 409 | phase exhausted its punch-list rounds (close only) |
| roadmap_unapproved | 409 | `spec` before the human approved the latest roadmap |
| human_approval_required | 403 | chat approval, verdict, or landing not backed by a prompt the human typed in the architect's session since the roadmap was proposed / acceptance was requested / the work was accepted |
| bad_series_name | 400 | `blueprint_init` without a builder and a name that isn't `^[a-z0-9][a-z0-9-]{0,47}$` (it names a branch and a folder) |
| not_merged | 409 | `land` with `method: merge` while the series branch isn't in the main checkout's HEAD (nothing is recorded) |
| worktree_dirty | 409 | `land` while the series worktree has uncommitted or untracked changes (nothing is recorded) |

MCP tools surface these via the envelope `error` string.

## Daemon HTTP (UDS + optional TCP)

The daemon is the only process that opens the SQLite database.
All state changes go through these routes.

Transport: the daemon serves these routes on a Unix socket
(`~/.breakroom/breakroom.sock`) with no auth, since filesystem ownership
is the auth. When `tcp_listen` is set (loopback only), the same routes are
also served over TCP behind a bearer token: `Authorization: Bearer
<token>`, where the token is the content of `~/.breakroom/token` (0600,
regenerated each start, removed on stop). Missing/wrong token → 401.
Built-in clients (CLI, MCP proxy) always use the Unix socket.

```text
GET  /v1/status
GET  /v1/agents?workspace_root=...&include_offline=...&caller=... # caller: only agents visible to it (plus itself)
POST /v1/agents/register   {workspace_root, worktree_path, role, plan_markdown?, provider_hint?, pid?, host_pid?, name?, series?}
POST /v1/agents/release    {workspace_root, agent_id}              # disconnect, keep inbox
POST /v1/agents/update     {workspace_root, agent_id, status?, current_step?, plan_markdown?}
POST /v1/messages/send     {workspace_root, from, to, msg_type?, content, include_diff_paths?, await_reply?, timeout_sec?}
POST /v1/messages/wait     {workspace_root, recipient, timeout_sec?} # held ≤ wait_hold_max_sec
POST /v1/messages/await    {workspace_root, recipient, from, after_id, timeout_sec?} # reply from `from` with id > after_id; held ≤ wait_hold_max_sec
POST /v1/messages/take     {workspace_root, recipient?, worktree_path?, ancestors?, peek?} # drain inbox, no block; peek: leave unread (ack after delivery)
POST /v1/messages/compact  {workspace_root, limit?}                 # digest of recent mail
POST /v1/git/diff          {workspace_root, agent_id, file_paths?, include_untracked?, summarize?}
POST /v1/git/conflicts     {workspace_root, agent_id, target_agent_id}
POST /v1/blueprints/init     {workspace_root, name, architect, builder?, goal?} # no builder: a series → {name, status, builder}
POST /v1/blueprints/spec     {workspace_root, name, caller, phase, title, objective, requirements[{id,text}], acceptance[{id,text}], constraints?}
POST /v1/blueprints/submit   {workspace_root, name, caller, phase, items[{requirement_id, summary}], proof, notes?}
POST /v1/blueprints/inspect  {workspace_root, name, caller, phase, verdict, acceptance_ids?, findings?[{requirement_id, issue, fix}]}
POST /v1/blueprints/status   {workspace_root, name?, phase?}
POST /v1/blueprints/close    {workspace_root, name, caller}
POST /v1/blueprints/roadmap  {workspace_root, name, caller, phases[{phase, title, delivers?}], out_of_scope?, base?} # → {version, status, handoff}
POST /v1/blueprints/approve  {workspace_root, name, version?, via, caller?, quote?} # via chat|cli; version 0 = latest pending → {version, approved_via, handoff?, series_error?}
POST /v1/blueprints/join     {workspace_root, caller, name?}        # → {joined, name?, architect?, worktree?, open?, waiting?}
POST /v1/blueprints/series   {workspace_root, caller, name, branch?, path?} # retry series worktree setup → {worktree, branch, handoff}
POST /v1/blueprints/acceptance {workspace_root, caller, name}       # architect hands finished work to the human → {status, handoff}
POST /v1/blueprints/verdict  {workspace_root, name, verdict, via, caller?, quote?, notes?, title?} # accept|changes → {status, follow_up_phase?, handoff}
POST /v1/blueprints/land     {workspace_root, name, method, ref?, via, caller?, quote?} # merge|branch|pr|manual (pr needs ref, the PR URL) → {status, handoff, warning?}
POST /v1/prompts/record      {workspace_root, worktree_path, text, ancestors?}   # prompt hook: what the human typed → {agent, recorded}
POST /v1/gate/stop           {workspace_root, worktree_path, ancestors?, stop_hook_active?, hold_sec?} # Stop hook; may hold ≤ hold_sec (default 540, max 3600)
POST /v1/messages/ack        {workspace_root, recipient, ids}       # mark gate-delivered mail read
```

Hook agent resolution (`take` without `recipient`, `prompts/record`,
`gate/stop`): first the agent whose recorded `host_pid` (the process that
spawned its proxy, sent at register) is the nearest of the caller's
`ancestors` (its process chain, nearest first; hooks and the proxy share
the host process); two live agents under one host PID skip this step.
With `ancestors` given, agents whose host is alive but not among them are
dropped next: the caller is provably another session (say, an
unregistered one sharing the folder) and must not take their mail.
Then the one agent whose worktree contains `worktree_path`; then, for
`take` only, the only live agent. `prompts/record` and `gate/stop` never
fall back to "the only live agent".

When several agents are still left (chats sharing one host process and
folder, e.g. two chats in one Cursor window), neither route acts on a
guess: `prompts/record` records for the one architect among them (nobody,
`ambiguous_agent`, if there are several) and returns `agent: ""`, so the
hook delivers no mail; `gate/stop` picks the likeliest stopping agent only
to decide whether to hold. It never blocks on mail already waiting and
skips reminders; mail arriving during a hold wakes the chat with a generic
reason and `suppress_ack: true`, and the chat's next call with its
`caller_id` brings its own mail.

`/v1/gate/stop` returns `{action: "allow"|"block", kind:
"mail"|"hold_expired"|"reminder"|"", reason, agent, message_ids}`. Mail is
peeked, not taken: the caller acks `message_ids` after delivering
`reason`, so a killed hook redelivers instead of losing mail. Order:
unread mail blocks at once; an agent waiting on its blueprint peer
(builder with a phase in `inspection`, architect with one
`constructing`/`punchlist`) is held until mail arrives (block with it)
or the hold ends (block, `hold_expired`); otherwise, unless
`stop_hook_active`, a reminder blocks once (a peer wait whose peer is
offline turns into this reminder, telling the agent to tell the human;
builder with an open phase,
architect with a phase in `inspection` or signed off on an active
blueprint, for a series whose roadmap is done, the reminder is to call
`blueprint_request_acceptance`); else allow. A builder waiting for a
series to open (a `join` that returned `waiting`) is held like a peer
wait while any other agent is connected. Mail already waiting blocks at
most 20 stops in a row within one hook-continued turn
(`stop_hook_active`); past that the stop is allowed and the mail stays
unread, so two agents trading replies can't keep each other running.
Mail that wakes a hold doesn't count. Blueprints waiting on the human (pending roadmap, `acceptance`,
`accepted`, `blocked`, closed) never hold. Every block reason is hashed so
`prompts/record` ignores it when a host re-submits it as a user prompt.

`to` accepts an agent id or `"*"` (broadcast fans out per recipient
visible to the sender, see Teams below, but
`await_reply` needs a concrete recipient). `msg_type` is `chat` (default),
`handoff`, `conflict_alert`, `review_req`, `blueprint_spec`,
`blueprint_punchlist`, `blueprint_signoff`, `blueprint_roadmap`, or `blueprint_series`. The `blueprint_*` types are
written by the daemon on blueprint transitions (submit notifies the
architect via `review_req`). `status` is `idle`,
`active`, `waiting`, `completed`, or `offline`.

Config lives at `~/.breakroom/config.json` (flat object, all fields
optional, unknown fields ignored):

```json
{
  "wait_hold_max_sec": 45,
  "diff_max_bytes": 65536,
  "summarize_max_bytes": 16384,
  "endpoint_url": "http://127.0.0.1:11434/v1/chat/completions",
  "model": "gemma4:e2b",
  "api_key_env": "OLLAMA_API_KEY",
  "reasoning_effort": "none",
  "extra_body": { "chat_template_kwargs": { "enable_thinking": false } },
  "blueprint_max_rounds": 5,
  "tcp_listen": "127.0.0.1:7420",
  "retention_days": 14
}
```

Defaults: hold 45s (clamped to ≤300s), diff cap 64KB (1KB–16MB),
summarize cap 16KB (1KB–1MB), retention 14 days (`< 0` keeps history forever;
global only). Omit the LLM fields (default) to disable
server-side summarization (callers get `fallback: true` instead); omit `tcp_listen` (default) to stay on the
Unix socket only (loopback hosts only). An invalid or non-loopback
`tcp_listen` disables TCP with a daemon log warning; the Unix socket still serves. Idle shutdown is a CLI flag, not
config: `breakroom start --idle-timeout 30m` (default 30m).

Per-project cascade: `<workspace>/.breakroom/config.json` (same shape)
overlays the global file per request, so each repo can point at its own
endpoint, model, and caps. A project `endpoint_url` must be loopback
(`localhost`, `127.0.0.0/8`, `::1`); any other is ignored with a daemon
log warning. `tcp_listen` never overlays, a project file
cannot move the daemon's listener. A malformed or unreadable project file
is ignored with a daemon log warning (global stands), as is a missing one.
`blueprint_max_rounds` (default 5, 1–20) caps punch-list rounds per phase
and participates in the cascade like any workspace-scoped field.

`reasoning_effort` (default unset) is sent as the OpenAI-standard
`reasoning_effort` request field; `"none"` turns thinking off on Ollama
and OpenAI reasoning models, and non-thinking models ignore it.
`extra_body` (default unset) is a JSON object merged into every sidecar
request for server-specific fields, e.g. vLLM/SGLang
`chat_template_kwargs`. It never overrides `model`, `messages`,
`temperature`, `max_completion_tokens`, or `max_tokens`. A project's `extra_body` replaces the
global one wholesale, and is dropped whenever the global API key would
be sent (keyless project endpoints keep it).

## Blueprints

A blueprint is a broker-enforced Architect/Builder stage-gate loop over a
named piece of work (see `docs/blueprint-spec.md` for the process). The
broker owns sequencing, roles, and shape; judgment stays with the models.

Lifecycle: blueprint `drafting` (no phases) → `active` (first spec) →
`acceptance` (the architect handed finished work to the human) →
`active` again (the human asked for changes: a follow-up phase) or
`accepted` → `handover` (landed, or closed), or `blocked` (round cap
hit, close only). While `acceptance`/`accepted`, nothing but a verdict,
landing, status, or close is accepted (`bad_transition`). Per phase:
`constructing` (spec issued) → `inspection` (submitted) → `punchlist` →
`constructing` … → `signoff`. Drafting/scoping are pre-creation
conditions, not stored states: the Architect's scoping work happens before
the `spec` call, which creates the phase already `constructing`.

Roadmaps: blueprints created with `blueprint_init` require an approved
roadmap before any `spec`. The architect proposes versions with
`blueprint_roadmap`; only the latest version can be approved, once. The
human approves either by replying in the architect's chat (the
architect calls `blueprint_approve` quoting them; accepted only if the
quote's words appear, as whole words and case-insensitively, in a prompt
the prompt hook recorded for the architect after the proposal, in clauses
(split at sentence punctuation and dashes) with no negation such as
"no", "not" or "don't"; a one-character quote like "1" must be its whole
clause. Verdicts and landings check quotes the same way) or with `breakroom
blueprint approve <name>`. A spec for phase N needs N within the
approved roadmap and the same title. A new version must keep every
already-specced phase's number and title; a new pending version blocks
the next spec until approved. This guards against invented or
injected approvals: it checks that the words came from the human, not
what they meant (a quoted question passes), and the negation list is
English. It does not guard against an agent forging hook calls from its
own shell.

Rules, all enforced by the broker:

- One phase at a time: `spec` for phase N requires N=1 on a drafting
  blueprint, or phase N−1 signed off with N = current+1.
- Strict roles: only the architect specs, inspects, closes; only the
  builder submits. Either participant (or any workspace agent) may read
  status.
- Submit must map every requirement id and carry non-empty `proof`.
- Sign-off (`verdict: "signoff"`) must cite every acceptance id.
- Punch-list (`verdict: "punchlist"`) needs non-empty `findings`, each
  citing a valid requirement id. Each punch list consumes one round; past
  `blueprint_max_rounds`, the blueprint goes `blocked` (terminal; the
  judged phase keeps status `inspection` as the record of where it
  stopped).
- Requirement/acceptance ids are architect-chosen non-empty strings,
  unique within their list.
- Transitions notify the peer over existing mail: spec → builder
  (`blueprint_spec`), submit → architect (`review_req`), punch-list →
  builder (`blueprint_punchlist`), sign-off → builder (`blueprint_signoff`).
  Content is rendered markdown per the spec templates, headed with the
  blueprint name and phase so clients can filter, and capped at 8KB:
  a deliverable keeps every item summary and its notes, trimming only
  the proof to its head and tail with a pointer to `blueprint_status`
  `phase=N`. The ledger always stores the full text.

Series: `init` without a builder makes a series. Its name must match
`^[a-z0-9][a-z0-9-]{0,47}$`. The architect's builder from its most
recent blueprint is attached automatically when still online and not
building another open blueprint; otherwise builders that were told to
wait (a `join` with nothing open) are mailed (`blueprint_series`). The
first roadmap approval creates the worktree with `git worktree add`:
`<main checkout>/.workspaces/<name>` (listed in `.git/info/exclude`), branch
`breakroom/<name>`, from the approved roadmap's `base` (resolved to a
commit) or else the main checkout's `HEAD`. A main checkout with no git
repo or no commit yet is initialized first. The main checkout is the
workspace root. The builder's registration is re-pointed at the worktree
(now or when it joins) and mailed the path; every spec mail repeats it. A
git failure keeps the approval and returns `series_error`; `series`
retries, optionally with another branch or path.

Acceptance: `acceptance` requires an active blueprint whose current
phase is the last of the approved roadmap and signed off. `verdict`
(`via: chat` quoting a prompt recorded after the request, or `via: cli`)
either accepts, or (`changes`, `notes` required) appends a phase to a new
roadmap version approved by the verdict itself (title `title`, default
`Review fixes (round N)`; `delivers` = notes) and reactivates the
blueprint. Rejections never consume punch-list rounds. `land` requires
`accepted`; its method is the human's choice (quote recorded after the
acceptance, or `via: cli`). For a series it checks the worktree is clean
(else `worktree_dirty`, nothing recorded), records the landing, removes
the worktree (`git worktree remove`, branch kept), and re-points the
builder at the main checkout. `close` on a series that never landed
keeps the worktree and branch and returns a `handoff` saying where they
are.

Handoffs: `roadmap`, `approve` (series), `series`, `acceptance`,
`verdict`, `land`, and `close` (series) return `handoff`, a message for
the human to relay verbatim, ending in one clear next step.

Teams: peer lists (`register_agent`), `list_agents` with a caller, and
broadcasts are scoped. A builder of an open blueprint sees its
architect; an architect sees its builders, every other architect, and
agents with no blueprint role; an agent with no blueprint role sees
everyone. Direct messages are not restricted.

## MCP tools

### register_agent

```json
{ "role": "builder", "worktree_path": "/repo", "plan_markdown": "...", "provider_hint": "claude-code", "name": "otter-417", "series": "api-debugging" }
```

`role` and `worktree_path` required; the daemon requires an absolute
path (the MCP tool accepts one relative to the agent's cwd). Assigns a fresh name of the form
`<word>-<NNN>` (e.g. `otter-417`); names are never reused within a
workspace, so a newcomer cannot inherit another agent's inbox or
blueprint roles. An identity resumes (same name, inbox and roles kept)
when `name` names an offline agent, or implicitly when an offline agent
is the architect or builder of `series` with the same `role`, or else has
the same `worktree_path` and `role`. Agents whose proxy died (completed
ones included) are marked offline by the reaper within ~10s. The proxy
also records its parent process as `host_pid`, which hooks use to find
the agent. Stores workspace/worktree metadata and returns the peer list
(scoped, see Teams; excludes self) plus an unread count (unread mail
itself arrives via `_unread_messages`).
data: `{ "agent_id": "otter-417", "peers": [...], "unread_count": 0 }`.
Errors: `bad_request`, `worktree_escape`, `unknown_agent` (no such `name`),
`name_in_use` (`name` is live), `store_error`.

### list_agents

```json
{ "include_offline": false }
```

data: `{ "agents": [{agent_id, role, status, current_step, worktree_path,
modified_files, last_heartbeat}] }`: the caller plus the agents visible
to it (see Teams). `modified_files` is refreshed server-side from `git
status` on every `update_plan`.

### update_plan

```json
{ "status": "active", "current_step": "cart API", "plan_markdown": "..." }
```

At least one field required; omitted fields are unchanged. Also refreshes
the agent's heartbeat and modified files, and reports overlaps in the
envelope's `_warnings` (`[{code, files}]`; the daemon route returns them
as `warnings` beside `agent`): `code: "file_overlap"` when a peer has
uncommitted changes to the same file, or `"git_error"` when the refresh
itself failed.
data: `{ "agent": {...} }`.

### send_message

```json
{ "to_agent": "heron-102", "content": "cart API ready?", "msg_type": "chat",
  "include_diff_paths": ["src/"], "await_reply": true, "timeout_sec": 300 }
```

Writes to SQLite, wakes the recipient's in-memory channel, and with
`await_reply` blocks for the first reply mailed after the send, for at
most `min(timeout_sec, wait_hold_max_sec)` (broadcast + `await_reply` is
rejected). On `timed_out`, the reply arrives later via
`wait_for_message` (the CLI instead re-waits on `/v1/messages/await`
with `after_id` = the sent id until `--timeout`). `include_diff_paths` attaches the sender's
capped diff for those paths to the stored message.
data: `{ "message_ids": [42], "reply": {...}|null, "timed_out": false }`.

### wait_for_message

```json
{ "timeout_sec": 300 }
```

Blocks until mail arrives or `min(timeout_sec, wait_hold_max_sec)`
elapses (defaults 300s / 45s): one call never outlives the hold, since
MCP hosts kill long tool calls. On `timed_out: true` the agent calls
again until its own budget is spent. Zero token cost while parked.
data: `{ "messages": [...], "timed_out": false }`.
Each message: `{id, from, to, type, content, diff_attachment, created_at}`.

### get_diff

```json
{ "agent_id": "heron-102", "file_paths": ["src/"], "include_untracked": true, "summarize": true }
```

Runs `git diff` over the target agent's whole worktree without
interrupting them (from its root, even if the agent registered a
subdirectory). The diff is against `HEAD`, except for the builder of an
open series working in its worktree: then it is against the commit the
series branched from, so committed phase work shows too (the same diff
`blueprint_submit` attaches). `file_paths` limits scope and is repo-root relative,
matching the paths in `files`; `include_untracked` adds untracked files;
`summarize` also returns an LLM file-by-file summary. With no endpoint
configured, `summary` stays empty and `fallback` is true: summarize
`diff` yourself, or `send_message` the agent named in `ask` (the diff's
author; omitted on a self-diff or when the author is offline; MCP only)
for a summary. `diff` is capped at `diff_max_bytes`.
data: `{ "diff": "...", "truncated": false, "files": [...], "summary": "",
"fallback": false, "ask": "heron-102" }` (`ask` only on fallback).
Errors: `unknown_agent`, `not_a_repo`, `no_commits`, `llm_error`.

### check_conflicts

```json
{ "target_agent_id": "heron-102" }
```

In-memory `merge-tree` check between the caller's and the target's
worktrees; neither directory is modified. Returns exact conflicting paths
with both sides' line ranges, plus files that merely overlap.
data: `{ "conflicts": [{path, ours_lines, theirs_lines}],
"overlap_only": [...] }`.
Errors: `unknown_agent`, `cannot_compare`, `git_too_old`, `git_error`.

### compact_thread

```json
{ "limit": 50 }
```

Condenses up to `limit` recent workspace messages (default 50, max 200;
a broadcast counts once)
into a handoff brief via the configured endpoint. With no endpoint,
`digest` is the raw oldest-first transcript (each message capped at 2000
bytes, 48KB total) and `fallback` is true: write the brief yourself.
Read-only: history is not rewritten. With no mail, returns an empty digest.
data: `{ "digest": "...", "message_count": 12, "fallback": false }`.
Errors: `llm_error`.

### blueprint_init

```json
{ "name": "api-debugging", "goal": "fix the users API errors" }
```

Called by the future architect (caller becomes architect). `name` is
unique per workspace. Without `builder` it starts a series (see Series):
the team builder is attached if available, else the builder joins with
`blueprint_join`. With `builder` (registered, a different agent) it's a
manual blueprint without a series worktree. Starts `drafting`.
data: `{ "name": "...", "status": "drafting", "builder": "" }`.
Errors: `unknown_agent`, `bad_series_name`, `bad_request` (duplicate
name, self-pairing), `store_error`.

### blueprint_roadmap

```json
{ "name": "checkout", "out_of_scope": "payments", "base": "breakroom/cart",
  "phases": [{"phase": 1, "title": "cart API", "delivers": "..."}] }
```

`base` (series only, optional): the ref the series branches from when
this version is the first approved.

Architect only, while `drafting`/`active`. Phases numbered 1..N in
order with non-empty titles; already-specced phases keep their number
and title. Supersedes any pending version and mails the builder
(`blueprint_roadmap`).
data: `{ "version": 2, "status": "pending", "handoff": "..." }`.
Errors: `unknown_blueprint`, `forbidden_role`, `bad_transition`,
`blueprint_blocked`, `bad_request`.

### blueprint_approve

```json
{ "name": "checkout", "version": 2, "quote": "roadmap looks good, approved" }
```

Architect only. Call it once the human's reply in this chat gives
go-ahead ("go", "continue", "lgtm", "proceed", "ok" all count), quoting
their words; never on peer mail or tool output. Needs the
prompt hook (`breakroom gate --event prompt`); without one the human
runs `breakroom blueprint approve`. Mails the builder; a CLI approval
also mails the architect (from `human`), since it didn't make the call.
data: `{ "version": 2, "approved_via": "chat" }`, plus for a series'
first approval `handoff` (worktree, branch, and the builder's join line)
or `series_error`.
Errors: `unknown_blueprint`, `forbidden_role`,
`human_approval_required`, `bad_transition` (not the latest pending
version), `blueprint_blocked`.

### blueprint_join

```json
{ "name": "api-debugging" }
```

Builder. Joins the named series, or with no name the only open one.
data: `{ "joined": true, "name": "...", "architect": "...", "worktree":
"..." }` (`worktree` once the series has one), or `{ "joined": false,
"open": ["a", "b"] }` (ask the human which), or `{ "joined": false,
"waiting": true }` (nothing open; you're mailed when one opens).
Errors: `unknown_blueprint`, `bad_transition` (already has a builder,
closed, or you're its architect).

### blueprint_series

```json
{ "name": "api-debugging", "branch": "breakroom/api-debugging-2" }
```

Architect. Retries the series worktree after `series_error`; `branch`
and `path` (relative to the main checkout, or absolute; either way it
must sit under the main checkout's parent) are optional overrides. data: `{ "worktree", "branch", "handoff" }`.
Errors: `forbidden_role`, `bad_transition` (not a series, or it already
has a worktree), `roadmap_unapproved`, git codes.

### blueprint_request_acceptance

```json
{ "name": "api-debugging" }
```

Architect, after the last roadmap phase is signed off.
data: `{ "status": "acceptance", "handoff": "..." }` (where to review,
how to reply). Errors: `forbidden_role`, `bad_transition`.

### blueprint_verdict

```json
{ "name": "api-debugging", "verdict": "changes", "quote": "the 500 on /users is still there",
  "notes": "fix the 500 on /users" }
```

Architect, recording the human's reply quoted from chat (the CLI
equivalent is `breakroom blueprint accept|reject`). data: `{ "status":
"accepted", "handoff": <landing choices> }` or `{ "status": "active",
"follow_up_phase": 3, "handoff": "..." }` (spec that phase next).
Errors: `human_approval_required`, `bad_transition`, `bad_request`.

### blueprint_land

```json
{ "name": "api-debugging", "method": "pr", "ref": "https://github.com/o/r/pull/42",
  "quote": "open a PR" }
```

Architect, after carrying out the human's chosen landing (`merge` into
the main checkout, push a `branch`, or a `pr`), or `manual` when the
human chooses to handle landing themselves (which keeps the worktree and
branch, closes the blueprint, and disconnects both agents). CLI equivalent:
`breakroom blueprint land <name> --method ... [--ref ...]`.
data: `{ "status": "handover", "handoff": "..." }` (a failed worktree
removal is appended as a warning). Errors: `worktree_dirty`,
`human_approval_required`, `bad_transition`, `bad_request`.

### blueprint_spec

```json
{ "name": "checkout", "phase": 1, "title": "cart API",
  "objective": "...",
  "requirements": [{"id": "r1", "text": "..."}],
  "acceptance": [{"id": "a1", "text": "..."}],
  "constraints": "..." }
```

Architect only. Issues the spec for the next phase (phase 1 on drafting,
else current+1 with the previous signed off) and notifies the builder.
With a roadmap, the latest version must be approved and the phase's
title must match it.
Requirements/acceptance must be non-empty with unique ids.
data: `{ "phase": 1, "status": "constructing" }`.
Errors: `unknown_blueprint`, `forbidden_role`, `bad_transition`,
`blueprint_blocked`, `roadmap_unapproved`, `bad_request`.

### blueprint_submit

```json
{ "name": "checkout", "phase": 1,
  "items": [{"requirement_id": "r1", "summary": "..."}],
  "proof": "<test output>", "notes": "..." }
```

Builder only, while the phase is `constructing`/`punchlist`. Items must
cover every requirement id; `proof` must be non-empty. Notifies the
architect. Builders deliver work with `blueprint_submit`; do not call
`blueprint_inspect` (architect only). data: `{ "phase": 1, "status": "inspection" }`.
Errors: `unknown_blueprint`, `forbidden_role`, `bad_transition`,
`blueprint_blocked`, `bad_request`.

### blueprint_inspect

```json
{ "name": "checkout", "phase": 1, "verdict": "punchlist",
  "findings": [{"requirement_id": "r1", "issue": "...", "fix": "..."}] }
```

Architect only (builders must never call this; builders deliver with
`blueprint_submit`; calling `blueprint_inspect` as a builder fails with
`forbidden_role`), while the phase is `inspection`. `verdict: "signoff"`
requires `acceptance_ids` covering every acceptance id and advances the
blueprint; `verdict: "punchlist"` requires non-empty `findings` citing
valid requirement ids and consumes one round (past the cap → `blocked`).
Notifies the builder either way.
data: `{ "phase": 1, "status": "signoff|punchlist|blocked", "rounds": 2 }`.
Errors: `unknown_blueprint`, `forbidden_role`, `bad_transition`,
`blueprint_blocked`, `bad_request`.

### blueprint_status

```json
{ "name": "checkout", "phase": 2 }
```

Any workspace agent. Read-only, always allowed. Three views:

- no `name`: the workspace's blueprints (headers only);
- `name`: the ledger overview, parties, status, and per phase only
  `{phase, title, status, rounds, updated_at}`, plus `roadmaps` (every
  version oldest first: `{version, phases, out_of_scope, proposed_at,
  approved_at, approved_via, approval_quote}`);
- `name` + `phase`: that one phase in full (objective, requirements,
  acceptance, constraints, deliverable with the full proof, inspections).

Errors: `unknown_blueprint`, `bad_request` (`phase` out of range or
without `name`), `store_error`.

### blueprint_close

```json
{ "name": "checkout" }
```

Architect only. Allowed from `drafting`/`active`/`acceptance`/`accepted`/`blocked`
(never while a phase sits `constructing`/`inspection`/`punchlist`;
inspect or wait first); records the ledger and marks `handover`. A series
closed without landing keeps its worktree and branch, and data adds a
`handoff` saying where they are. Notifies the builder
via `chat` with the real outcome (all phases signed off, closed while
`blocked`, or closed before any spec); closing an already-closed
blueprint succeeds silently.
data: `{ "name": "checkout", "status": "handover" }`.
Errors: `unknown_blueprint`, `forbidden_role`, `bad_transition`,
`store_error`.

## Daemon status

`GET /v1/status` → `{version, pid, started_at, uptime_sec,
agents_present, agents_completed, agents, socket, tcp_addr}` (`agents`:
`[{workspace_root, agent_id, role, status, worktree_path}]`; `tcp_addr`
is `""` when TCP is disabled). Used by `breakroom status`, `logs`, `doctor`, and health checks.
