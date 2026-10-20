# Breakroom

**Breakroom lets AI coding agents collaborate safely in the same repository without stepping on each other.**

Agents can work solo on their own tasks while publishing plans, messaging peers, and checking for diff conflicts, or pair up in stage-gated Architect/Builder blueprints where one plans and reviews while the other writes the code. Coordination happens over isolated git worktrees, with no runtime dependencies and zero-token waiting. Runs via MCP `stdio` or plain CLI.

- **Agent instructions:** [`SKILL.md`](SKILL.md)
- **Reference docs (keep with SKILL.md):** [`docs/tool-contracts.md`](docs/tool-contracts.md) and [`docs/blueprint-spec.md`](docs/blueprint-spec.md)

---

## Highlights

- **Solo & Peer Collaboration**: Agents can fly solo on their own tasks while publishing live status (`update_plan`), messaging other agents working in the repo (`send_message`), inspecting peer worktree diffs (`get_diff`), and detecting merge conflicts early (`check_conflicts`) without interrupting anyone.
- **Stage-Gated Architect / Builder Loop (`Blueprint`)**: Pair a high-reasoning Architect model with a fast Builder model on complex features. The broker deterministically enforces roles, roadmap approval, phase-by-phase specs, requirement coverage, verification proof, and inspection sign-offs. Out-of-order calls are rejected by the broker, not left to chance.
- **Prompt-Cache Friendly (up to 95% cache hit)**: Multi-agent setups often break prompt caches by changing top-of-prompt instructions or polling in loops. Breakroom keeps wakeup hooks constant, delivers mail at the tail of the conversation, batches multi-step transitions into single turns, and attaches filtered diffs automatically.
- **Zero-Token Waiting via Lifecycle Hooks**: When an agent is waiting on a peer (e.g. the Architect waiting for a phase deliverable, or the Builder waiting for roadmap approval or the next phase spec), the `Stop` hook holds the agent inside the OS process at **zero token cost** and wakes it the instant mail arrives. *(Zero-token waiting needs a host with lifecycle hooks; see the **Autonomous?** column under [Connect your agent host](#connect-your-agent-host-mcp). Elsewhere agents fall back to `wait_for_message`.)*
- **Self-Contained `.workspaces/<name>` Worktrees (Auto `git init`)**: Every blueprint series gets its own isolated git worktree inside `<workspace>/.workspaces/<name>` (branch `breakroom/<name>`), automatically hidden via `.git/info/exclude`. Point Breakroom at a brand-new folder with no git repo and it automatically initializes git with a baseline commit so worktrees just work (dependency/build folders and likely secrets such as `.env` and `*.key` are left out of that commit; the handoff says how many files went in).
- **Non-Interrupting Diffs & Conflict Detection**: Inspect any peer's uncommitted worktree (`get_diff`, with untracked source files included and build/dependency folders filtered out) or run `git merge-tree` conflict detection (`check_conflicts`) without interrupting their session.

---

## Platforms

macOS and Linux. The daemon relies on Unix domain sockets, `flock`, and `setsid`; Windows is supported via WSL. Requires `git` on `PATH` (`check_conflicts` needs git 2.38+; `breakroom doctor` checks your environment).

## Install

**Homebrew** (macOS, Linux):

```sh
brew install search-3d/tap/breakroom
```

**Go** (1.26+):

```sh
go install github.com/search-3d/breakroom/cmd/breakroom@latest
```

**Via Install Script**

```sh
curl -fsSL https://raw.githubusercontent.com/Search-3D/breakroom/main/install.sh | bash
```

**Manual**
Download the archive for your OS and arch from [Releases](https://github.com/search-3d/breakroom/releases), verify it against `checksums.txt`, and put `breakroom` on your `PATH`.

> [!IMPORTANT]
> **macOS, manual download:** release binaries aren't notarized, so macOS blocks `breakroom` the first time it runs, and agent hosts then fail to start the MCP server. (Homebrew clears this for you.) Allow it once:
>
> 1. Run `breakroom version` in Terminal. When macOS says it can't verify the developer, click **Done**.
> 2. Open **System Settings → Privacy & Security**, scroll down to the message that `breakroom` was blocked, click **Open Anyway**, and confirm with your password or Touch ID.
> 3. Run `breakroom version` again and choose **Open**. It runs normally from then on, including when hosts start it.
> 4. Optionally, run the command `/usr/bin/xattr -dr com.apple.quarantine  /path/to/breakroom` to remove the quarantine flag (this is what the install script does).
>
> Repeat after each new download. `go install` builds are not affected. Signed binaries are coming soon.

Then check your environment:

```sh
breakroom doctor
```

## Quickstart

### 1. Open your project folder

Open your agents in the project folder. No manual git or worktree setup is needed: for [Blueprint](#blueprint-tech-lead-loop) work, Breakroom automatically initializes git if needed, creates an isolated worktree at `.workspaces/<series>` on branch `breakroom/<series>`, and moves the builder into it.

### 2. Install Breakroom for your agent host

Breakroom packages complete native plugins (MCP server, lifecycle hooks, and skills) for all major hosts. Run `breakroom install` with your host (or omit `--host` to choose interactively):

```sh
breakroom install --host claude
```

Supported hosts include Claude Code, Codex, Cursor, VS Code (GitHub Copilot), Antigravity, OpenCode, and Muse Code. The installer sets up the MCP server, lifecycle hooks, and skills in a single command. See [`integrations/`](integrations/README.md) for host-specific details, permissions notes, and manual configurations.

The background daemon starts automatically on the first tool call; you never need to run `breakroom start`.

## Connect your agent host (MCP)

Every host runs the exact same command: a `stdio` MCP server invoked via `breakroom mcp`. No port, URL, or API key required.

> [!NOTE]
> We offer full plugins for most hosts that can be dropped in by `breakroom install`. For manual setups, ready-to-copy MCP configs, hooks, and host-specific notes, see [`integrations/`](integrations/README.md).

| Host | Architect | Builder | Autonomous?* | Setup | Hooks |
| --- | :---: | :---: | :---: | --- | --- |
| **Claude Code** | ✅ | ✅ | ✅ | [claude](integrations/claude/README.md) | Self-contained plugin, Stop + prompt + `check`, tested ✅ |
| **Muse Code** | ✅ | ✅ | ✅ | [muse](integrations/muse/README.md) | Self-contained plugin, Stop + prompt hooks, tested ✅ |
| **Codex** | ✅ | ✅ | ✅ | [codex](integrations/codex/README.md) | Self-contained plugin, Stop + prompt (`--host codex`), tested ✅ |
| **Cursor** | ✅ | ✅ | ⚠️ | [cursor](integrations/cursor/README.md) | Self-contained plugin, Stop + prompt hooks, tested ✅ |
| **Antigravity** | ✅ | ✅ | ✅ | [antigravity](integrations/antigravity/README.md) | Self-contained plugin, Stop + PreInvocation hooks, tested ✅ |
| **VS Code (GitHub Copilot)** | ✅ | ✅ | ✅ | [vscode](integrations/vscode/README.md) | Self-contained plugin, Stop + prompt + `check` hooks, tested ✅ |
| **OpenCode** | ✅ | ✅ | ✅ | [opencode](integrations/opencode/README.md) | Self-contained plugin ([breakroom.ts](integrations/opencode/breakroom.ts)), Stop + prompt hooks, tested ✅ |

*\*Autonomous:*

- ✅ **Autonomous**: Runs unattended after initial roadmap approval. Execution lifecycle hooks automatically hold and wake agents inside the OS process at zero token cost.
- ⚠️ **Unverified**: Lifecycle hooks are mapped and configured, but have not yet been verified end-to-end on this host. If hooks do not fire, agents may require manual prompts to wake between turns.
- ❌ **Manual**: The host lacks blocking hook capabilities. Requires manual user prompts to wake agents between turns.

---

## Token & prompt-cache efficiency

Multi-agent setups often spend far more tokens than a solo agent, mostly on polling and broken prompt caches. Breakroom avoids both:

### 1. Cache-Safe Stop Hook Wakeups

Some hosts (such as Muse Code) inject `Stop` hook block reasons into a top-of-prompt developer context block. Putting variable message bodies into that block invalidates the LLM's prefix cache on every wakeup. Breakroom detects `--host muse` and emits a **constant wakeup string** on the `Stop` hook, leaving the actual message unread so the agent pulls it once at the tail of the conversation history, keeping the entire prefix cached.

### 2. Zero-Token OS Holds Instead of Polling

Agents never poll in a loop waiting for peers. The `Stop` hook (`breakroom gate --event stop`) parks the waiting agent inside the OS process at zero token cost. When a builder joins before the roadmap is approved, Breakroom silently attaches the lone waiting builder on `blueprint_init` without waking it until Phase 1 is actually specced.

### 3. Batched Blueprint Tool Calls

The MCP server serializes concurrent blueprint calls on a session and briefly retries if the host dispatches parallel tool calls out of order. This lets agents batch multi-step transitions in a single LLM turn:

- **Roadmap approval:** `blueprint_approve` + `blueprint_spec(phase=1)` in one turn.
- **Phase sign-off:** `blueprint_inspect(signoff)` + next `blueprint_spec` (or `blueprint_request_acceptance` on the final phase) in one turn.

`register_agent` always gets a turn of its own: later calls pass the agent id it returns as `caller_id`.

### 4. Automatic Diff Attachment & Noise Filtering

When the builder calls `blueprint_submit`, Breakroom automatically attaches up to 12 KB of the builder's `git diff` (for a series, against the commit the series branched from, so work the builder already committed shows too; untracked source files included, minus noisy folders: `node_modules`, `.next`, `.venv`, `venv`, `__pycache__`, and `.workspaces` anywhere, and `dist`, `build`, `target`, and `vendor` at the repo root) directly on the `review_req` deliverable mail. The Architect reviews the proof and diff immediately without spending an extra turn calling `get_diff`.

---

## Hooks (zero-token delivery)

Three hooks, all optional, all fail-open (no daemon, no mail, or an ambiguous agent mean silent exit 0):

### Stop hook: `breakroom gate --event stop`

When the agent tries to end its turn: unread mail blocks the stop and wakes the model.

An agent waiting on its blueprint peer (architect waiting on a submission; builder at any point it isn't actively building, from roadmap to landing) is **held inside the hook** at zero model tokens until mail arrives or `--hold` (default 540s) expires; on expiry it is told to end its turn again to keep waiting. If the peer goes offline, it is told once to notify you, then may stop. An agent that forgot a blueprint step is reminded once. Two agents trading mail are released after 20 back-to-back mail blocks in one turn. The architect stops normally while waiting on *you* (pending roadmap, acceptance), and both stop on a blocked blueprint.

### UserPromptSubmit hook: `breakroom gate --event prompt`

Records what you type so the architect can verify and record your roadmap approval or acceptance verdict from a chat reply (see [Blueprint](#blueprint-tech-lead-loop)), and delivers unread mail.

### PostToolUse hook: `breakroom check --hook-event PostToolUse`

Delivers mail between tool calls.

> [!NOTE]
> Zero-token holds need a host with lifecycle hooks. Without them, agents call `wait_for_message` instead.

Hook mode never fails the host: no daemon, no mail, or an ambiguous agent all mean silent exit 0. Without `--agent`, the recipient is resolved via process ancestry (walking up from the hook process to match the host session that spawned `breakroom mcp`), so multiple agents can share the same project folder safely. Failing that, it resolves to the most specific worktree containing the hook's working directory (so a builder in `.workspaces/<name>` never collides with an architect in the repo root). Chats sharing one host process and folder (two chats in one Cursor window) can't be told apart; there hooks never act on a guess: your prompt is recorded only when one architect is among them, and a wakeup carries no message text, so each chat reads its own mail on its next Breakroom call.

## CLI

| Command | What it does |
| --- | --- |
| `breakroom mcp [--dir DIR]` | MCP stdio server (hosts spawn this; starts the daemon if needed) |
| `breakroom install [--host H] [--global] [--bin-path P] [DIR]` | Install native plugins, skills, MCP configs, and hooks for an agent host (interactive if `--host` is omitted). `--host generic` installs only the skill, to `.agents/skills/breakroom` |
| `breakroom uninstall --host H\|all [--global] [DIR]` | Remove what `install` wrote, hooks included. For Muse it also strips a manual setup from `~/.config/muse/settings.json` or `.muse/hooks.json`; then run `muse plugins remove breakroom` |
| `breakroom start` / `stop` / `status` | Daemon lifecycle (`start --idle-timeout 30m`; `status --format json`) |
| `breakroom prune [--force]` | Clear state from previous runs: stops the daemon and deletes `state.db` (past agent names, inboxes, messages, blueprints, recorded prompts) plus `daemon.log`, `daemon.log.1` and `trace-*.log`. Config and token are kept. Refuses while agents are connected unless `--force` |
| `breakroom check [--hook] [--hook-event E] [--agent ID]` | Drain unread mail |
| `breakroom gate --event stop\|prompt [--host claude\|codex\|vscode\|muse\|antigravity\|cursor\|copilot\|opencode\|generic] [--hold SEC]` | Host hook: hold/block a stop, record your prompts (see [Hooks](#hooks-zero-token-delivery)) |
| `breakroom blueprint approve <name> [--version N]` | Approve a blueprint's latest pending roadmap from your terminal |
| `breakroom blueprint accept <name>` / `reject <name> --notes TEXT` | Accept finished work, or ask for changes (they become a follow-up phase) |
| `breakroom blueprint land <name> --method merge\|branch\|pr\|manual [--ref REF]` | Record how accepted work landed (removes the series worktree and keeps its branch; `manual` keeps both) |
| `breakroom send --to ID [--from ID] [--await] [--diff-path P]` | Send a message (reads stdin without `--message`) |
| `breakroom diff [--agent ID] [--path P] [--untracked=false] [--summarize]` | Show an agent's uncommitted diff (untracked files included, as with `get_diff`) |
| `breakroom compact [--limit N]` | LLM handoff digest of recent mail (raw thread without an LLM) |
| `breakroom doctor` / `logs` / `version` | Environment check, daemon log, version |

The CLI resolves the workspace like agents do (repo root, shared by all worktrees) unless `$BREAKROOM_WORKSPACE` is set.

## Blueprint (tech-lead loop)

Pair a strong Architect model with a fast Builder model on phased work. The Architect decomposes the goal, issues one phase spec at a time, and inspects each deliverable; the Builder implements, submits verification proof, and remediates punch lists. The broker enforces the state machine: roles, one phase at a time, requirement coverage, proof before inspection.

Typical flow (all MCP tools):

### 1. Start agents & join blueprint

Start both agents in the project folder (one as architect with your goal, one as builder). The builder calls `register_agent`, then `blueprint_join` on its next turn, and parks on the Stop gate until Phase 1 is specced.

### 2. Architect proposes the roadmap

Calls `register_agent`, then `blueprint_init` (naming the series, e.g. `api-debugging`) and `blueprint_roadmap` (every phase + out-of-scope), shows the roadmap to you, and stops.

### 3. Approve roadmap & spin up worktree

Reply in the architect's chat (`go`, `lgtm`, `yes`, `proceed`, etc.) or run `breakroom blueprint approve <name>`. The architect calls `blueprint_approve` + `blueprint_spec` (for Phase 1) in a single turn. Breakroom initializes git if needed, creates the isolated series worktree at `.workspaces/api-debugging` (branch `breakroom/api-debugging`), moves the builder into it, and wakes the builder with the Phase 1 spec.

### 4. Builder implements & submits proof

Works strictly inside `.workspaces/<name>`, verifies its work, cleans up any temporary background servers/processes, and calls `blueprint_submit` with per-requirement summaries and real test output as `proof`. Breakroom automatically attaches up to 12 KB of the builder's git diff.

### 5. Architect inspects & advances

Wakes with the deliverable proof and diff attachment, verifies every acceptance criterion, and calls `blueprint_inspect` (`signoff` + next `blueprint_spec` in the same turn, or `punchlist` with specific fixes).

### 6. Human acceptance & landing

When the final phase is signed off, the architect calls `blueprint_request_acceptance` in the same turn and asks you to review the worktree. Reply *accept* (or describe any changes to spin up a follow-up phase). On accept, choose how to land it (merge into your main checkout, push as a branch, open a PR, or leave it in the worktree (`manual`)), or close without landing (`blueprint_close`). Breakroom cleans up the worktree (refusing if uncommitted changes remain) and keeps the branch.

At every point where it needs you, the architect relays a message from Breakroom that ends with exactly what to do next. Run several teams (an architect and a builder each) in parallel in one project: each series has its own `.workspaces/<name>` worktree, peer lists stay within a team, and architects can message each other to keep their plans from overlapping.

With the [Stop hook](#hooks-zero-token-delivery) installed, the entire multi-phase build-and-inspect loop between roadmap approval and final acceptance runs unattended.

The chat-approval check proves the words the architect quotes came from you, typed in its own session after the request, in a clause without a negation. That stops invented approvals and approvals smuggled in through peer mail or diffs. It can't judge what you meant: if you only asked a question, an architect that quotes part of it still passes, so reply plainly ("go", "accept"). The negation check is English-only. It is not a security boundary either: an agent with shell access runs as you and could call `breakroom` itself to forge a hook call.

**Perks:** a strong model where judgment matters (architecture, decomposition, code review) and a fast one for the building; every phase gated on real test output; the full ledger in `blueprint_status`; and capped punch-list rounds (`blueprint_max_rounds`, default 5), so a stuck loop blocks for you instead of burning tokens.

**Trade-offs:** Two agents have two base context windows, making Blueprints ideal for multi-phase features and overkill for one-line fixes. The broker checks evidence *presence* and requirement mapping, while the Architect model judges code quality. A blocked blueprint is terminal (close it and start fresh).

## Compaction (optional)

`get_diff` summarization and `compact_thread` use an OpenAI-compatible chat endpoint when one is configured. Without one they return the raw diff or thread with `fallback: true`, and the calling agent summarizes it itself (costs its tokens, never fails). Nothing else touches a model: routing, delivery, waits, and blueprint gating stay deterministic and work without one.

### Local model (Ollama, no key)

```json
{
  "endpoint_url": "http://localhost:11434/v1/chat/completions",
  "model": "gemma4:e2b",
  "reasoning_effort": "none"
}
```

That's the pick from `bench/sidecar` (diff and thread cases, including a prompt injection planted in agent mail): about 4.5s per call locally (benchmarked on an Apple M1 Pro), and the only small model tested that never obeyed the injection. Digests feed sign-off decisions, so test a replacement against those cases first.

### Hosted API (faster, stronger)

Any OpenAI-compatible provider works. Groq, for example, in `~/.breakroom/config.json`:

```json
{
  "endpoint_url": "https://api.groq.com/openai/v1/chat/completions",
  "api_key_env": "GROQ_API_KEY",
  "model": "openai/gpt-oss-120b",
  "reasoning_effort": "low"
}
```

and export the key wherever your agent hosts start:

```sh
export GROQ_API_KEY=gsk_...
```

`api_key_env` is the **name** of an env var, never the key itself, so the config file holds no secret. Other providers only change the URL, the variable name, and the model id:

| Provider | `endpoint_url` |
| --- | --- |
| OpenAI | `https://api.openai.com/v1/chat/completions` |
| OpenRouter (many models, one key) | `https://openrouter.ai/api/v1/chat/completions` |
| Groq | `https://api.groq.com/openai/v1/chat/completions` |

The key is read from the **daemon's** environment, which it inherits from whatever first started it (usually the first agent host). If you export the key later, run `breakroom stop`; connected agents reconnect on their next call and the new daemon picks the key up.

### Remote is global, local is per-project

Hosted endpoints go in the **global** `~/.breakroom/config.json`. A repo can also ship `.breakroom/config.json` to set caps (it can shorten `wait_hold_max_sec`, never lengthen it) or point at its own local model, but its `endpoint_url` must be `localhost` or a loopback IP; anything else is ignored (with a warning in `breakroom logs`) and the global endpoint is used instead, with the global `model` and `reasoning_effort`.

Why: any repo you clone can ship that file, and summarization sends the endpoint your uncommitted diffs and the workspace's agent mail. A repo that could name a remote endpoint could quietly collect both. You chose the global file; a repo's file is the repo author's choice, so it only gets endpoints on your own machine. For the same reason a repo never sets `api_key_env`, your key only ever goes to the global endpoint.

### Thinking models

Thinking models (Qwen3, SmolLM3, DeepSeek-R1, …) can spend the whole reply budget reasoning and return nothing. Add `"reasoning_effort": "none"` to turn thinking off (`"low"` for models without an off switch, like gpt-oss); it's sent as the standard `reasoning_effort` field, which Ollama and OpenAI honor and non-thinking models ignore. `ollama show <model>` lists `thinking` under Capabilities when it applies. An empty reply fails with `llm_error` instead of returning a blank summary.

Servers that need their own fields take `extra_body`, merged into every request (it never overrides `model`, `messages`, `temperature`, `max_completion_tokens`, or `max_tokens`). vLLM and SGLang ignore `reasoning_effort` for Qwen3-style models; turn thinking off with:

```json
{ "extra_body": { "chat_template_kwargs": { "enable_thinking": false } } }
```

## Configuration

Breakroom requires zero configuration to start. To customize settings, create a flat JSON file at `~/.breakroom/config.json` (globally) or `<workspace>/.breakroom/config.json` (per-project). All fields are optional; a project file ignores the fields marked **Global only** below. A full global file:

```json
{
  "wait_hold_max_sec": 45,
  "diff_max_bytes": 65536,
  "summarize_max_bytes": 16384,
  "blueprint_max_rounds": 5,
  "retention_days": 14,
  "tcp_listen": "127.0.0.1:7420",
  "endpoint_url": "http://127.0.0.1:11434/v1/chat/completions",
  "model": "gemma4:e2b",
  "api_key_env": "OLLAMA_API_KEY",
  "reasoning_effort": "none",
  "extra_body": { "chat_template_kwargs": { "enable_thinking": false } }
}
```

### Settings reference

| Setting | Default | Scope | Description |
| --- | --- | --- | --- |
| `wait_hold_max_sec` | `45` | Global & Project | Maximum seconds `wait_for_message` or `await_reply` holds before returning (clamped to $\le 300$s). Projects can only shorten this. |
| `diff_max_bytes` | `65536` | Global & Project | Maximum size in bytes for uncommitted diffs attached or inspected (1KB–16MB). |
| `summarize_max_bytes` | `16384` | Global & Project | Maximum size in bytes sent to sidecar LLM for diff summarization (1KB–1MB). |
| `blueprint_max_rounds` | `5` | Global & Project | Maximum punch-list remediation rounds per phase (1–20) before blocking for human triage. |
| `retention_days` | `14` | Global only | Days to keep read mail and closed blueprint history before cleanup (`-1` keeps forever; `0` or unset means the default 14). |
| `tcp_listen` | unset | Global only | Optional loopback address (e.g. `127.0.0.1:7420`) to serve the daemon API over TCP. |
| `endpoint_url` | unset | Global & Project | OpenAI-compatible chat completions URL (or its base, e.g. `http://localhost:11434/v1`) for diff/thread summarization. Projects are limited to loopback/localhost. |
| `model` | unset | Global & Project | Model identifier for summarization (e.g. `gemma4:e2b`, `openai/gpt-oss-120b`). |
| `api_key_env` | unset | Global only | Name of environment variable holding the sidecar API key. |
| `reasoning_effort` | unset | Global & Project | Request field (`"none"`, `"low"`, `"medium"`, `"high"`). Set `"none"` to turn thinking off on Ollama and OpenAI reasoning models. |
| `extra_body` | unset | Global & Project | JSON object merged into sidecar LLM requests for server-specific kwargs (e.g. vLLM/SGLang chat templates). |

## TCP access (optional)

Set `"tcp_listen": "127.0.0.1:7420"` in `~/.breakroom/config.json` and restart the daemon. The same API is then served over localhost TCP behind a per-start bearer token:

```sh
curl -H "Authorization: Bearer $(cat ~/.breakroom/token)" \
  http://127.0.0.1:7420/v1/status
```

Only loopback addresses are accepted. The token changes on every daemon start. Same-binary clients keep using the faster Unix socket.

## Gotchas

- **Hooks consume mail.** A hook that delivers mail marks it read. Wire `breakroom check` only to an event whose output the model sees (Claude Code's `PostToolUse` via `--hook-event PostToolUse`); on any other event the mail is marked read and never seen. The installed plugins already get this right.
- **Workspaces are repo roots.** All worktrees of a repo (bare clones included) share one workspace. For a bare clone, series worktrees go in `.workspaces/` beside the bare repo folder. Outside git, the workspace is the directory itself; set `BREAKROOM_WORKSPACE` to group non-git sibling folders.
- **Waits are per call.** `wait_for_message` and `await_reply` return within `wait_hold_max_sec` (default 45s) because hosts kill long tool calls; `timed_out: true` means "call `wait_for_message` again", not "no reply is coming".
- **Agent names are never reused** (`otter-417`). An agent that reconnects from the same worktree with the same role gets its name, inbox, and blueprint roles back; otherwise it can pass `name` to `register_agent`. A host that crashed is marked offline within ~10s.
- **Upgrades:** an old daemon keeps running after you upgrade the binary. The next agent session swaps it automatically if no agents are attached; otherwise `breakroom doctor` flags it, run `breakroom stop` (connected agents reconnect on their next call).
- **History is kept 14 days.** The daemon deletes mail read more than 14 days ago and closed blueprints (with their proofs) untouched for 14 days; unread mail and open blueprints are never deleted. Set `"retention_days"` in `~/.breakroom/config.json` to change it (`-1` keeps everything). Prompts you type in an agent's chat (recorded so chat approvals can be checked) are not age-limited: the newest 50 per agent are kept until `breakroom prune`. `daemon.log` moves to `daemon.log.1` once it passes 10MB (checked when the daemon starts). `breakroom prune` stops the daemon and deletes all of it now (`config.json` is kept).
- **Same-user trust model.** Anything running as your user can talk to the socket and act as any agent. Blueprint roles stop model mistakes, not adversaries.
- **`get_diff` paths are repo-root relative**, even for an agent that registered a subdirectory.
- **Keep `BREAKROOM_HOME` short.** Unix socket paths are capped at ~104 bytes on macOS.
- **Debugging a host:** set `BREAKROOM_TRACE=1` in the MCP server's `env` to log raw JSON-RPC, exactly as the host sends it, to `~/.breakroom/trace-<pid>.log`. Logs hold full payloads and are never cleaned up; unset it and delete them when done.
- **Idle shutdown:** the daemon exits after 30 minutes with no agents present (`breakroom status` polling does not count as activity).

## Develop

```sh
./scripts/lint.sh    # skill copy drift, gofmt, go vet, golangci-lint
./scripts/test.sh    # go test -race ./...
./scripts/build.sh   # lint + test + build ./breakroom
./scripts/fmt.sh     # gofmt -w, and sync skill copies
./scripts/ci_test.sh # Test CI before push
```

`SKILL.md` and `docs/{tool-contracts,blueprint-spec}.md` are the sources; each plugin under `integrations/` carries a copy. Edit the source, then run `./scripts/fmt.sh` (or `./scripts/sync-skills.sh`); CI fails on drift.

End-to-end tests run the real binary. To include them in coverage:

```sh
mkdir -p /tmp/brcov && GOCOVERDIR=/tmp/brcov go test ./cmd/...
go tool covdata percent -i=/tmp/brcov
```

## Contributing

Required developer tools:

- **Go 1.26+**
- **Git 2.38+** (required for `git merge-tree` conflict detection)
- **golangci-lint** (for code quality and lint checks)
- **goreleaser** (for release builds and packaging)
- **[act](https://github.com/nektos/act)** (for testing CI)
