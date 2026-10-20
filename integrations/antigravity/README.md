# Antigravity

Breakroom integration for [Antigravity](https://antigravity.google).

The [`breakroom-plugin/`](breakroom-plugin/) folder is a self-contained **Antigravity Plugin** (`breakroom`), packaging:

- [`plugin.json`](breakroom-plugin/plugin.json): Antigravity plugin manifest.
- [`mcp_config.json`](breakroom-plugin/mcp_config.json): MCP server configuration (`breakroom mcp`).
- [`hooks.json`](breakroom-plugin/hooks.json): Lifecycle hooks configuration (`PreInvocation` prompt recording and `Stop` hold/mail gate).
- [`skills/breakroom/`](breakroom-plugin/skills/breakroom/): The Breakroom coordination skill and reference documentation ([`SKILL.md`](breakroom-plugin/skills/breakroom/SKILL.md), [`docs/tool-contracts.md`](breakroom-plugin/skills/breakroom/docs/tool-contracts.md), [`docs/blueprint-spec.md`](breakroom-plugin/skills/breakroom/docs/blueprint-spec.md)).

## How plugins work in Antigravity

Antigravity natively discovers and loads plugins placed in `.agents/plugins/<name>` (workspace scope) or `~/.gemini/config/plugins/<name>` (global user scope).

When bundled as a plugin:

1. **MCP servers:** Antigravity launches the MCP server in `mcp_config.json` and exposes it through `call_mcp_tool` as `ServerName: "breakroom_breakroom"` (see [Dispatcher & Tool Calling](#dispatcher--tool-calling)).
2. **Lifecycle hooks:** Antigravity registers the lifecycle hooks defined in `hooks.json` (`PreInvocation` for chat prompt recording and `Stop` for peer holds and mail delivery).
3. **Skills:** Antigravity discovers the `breakroom` skill from `skills/breakroom/SKILL.md`.

**No manual edits to global files (`~/.gemini/config/mcp_config.json` or `~/.gemini/config/hooks.json`) are required.**

---

## Install instructions

### Option 1: Automatic CLI install (Recommended)

To install globally for all Antigravity sessions and worktrees (default):

```sh
breakroom install --host antigravity
```

This deploys the complete Breakroom plugin directly to `~/.gemini/config/plugins/breakroom` (global user scope across all workspaces).

To install into a specific project instead:

```sh
breakroom install --host antigravity /path/to/project
```

This deploys the plugin to `.agents/plugins/breakroom` in that project.

### Option 2: Manual plugin installation

#### Globally (User scope - across all projects -- recommended)

Copy or symlink `breakroom-plugin/` to `~/.gemini/config/plugins/breakroom`:

```sh
mkdir -p ~/.gemini/config/plugins
cp -r integrations/antigravity/breakroom-plugin ~/.gemini/config/plugins/breakroom
```

#### Per-Project (Workspace scope)

**Note:** Global-scope has been found to work much better for Antigravity.

Copy or symlink `breakroom-plugin/` to `.agents/plugins/breakroom` in your workspace:

```sh
mkdir -p /path/to/project/.agents/plugins
cp -r integrations/antigravity/breakroom-plugin /path/to/project/.agents/plugins/breakroom
```

Or install using the Antigravity CLI:

```sh
agy plugin install integrations/antigravity/breakroom-plugin
```

#### Validate installation

Verify the plugin with (use `.agents/plugins/breakroom` for a project install):

```sh
agy plugin validate ~/.gemini/config/plugins/breakroom
```

In an `agy` session, `/hooks` lists the registered `breakroom` hooks.

You should see:

```text
Validating breakroom (~/.gemini/config/plugins/breakroom/plugin.json)
  skills 1 · agents 0 · rules 0 · hooks 1 · mcpServers 1 · sidecars 0
✔ Validation passed
```

---

## Dispatcher & Tool Calling

Antigravity routes MCP server tools through its meta-tool:

```text
call_mcp_tool(ServerName: "breakroom_breakroom", ToolName: "<tool_name>", Arguments: "{...}")
```

> [!NOTE]
> When loaded from an Antigravity plugin, MCP servers are namespaced as `<plugin_name>_<server_name>` (hence `breakroom_breakroom`). If configured as a standalone global MCP server, the server name is `breakroom`.

The bundled `SKILL.md` instructs the agent to invoke Breakroom operations (such as `register_agent`, `send_message`, `blueprint_spec`, `wait_for_message`) via `call_mcp_tool`.

---

## Hook behaviors & limitations

### 1. `PreInvocation` hook (Prompt recording, approvals & mail injection)

Runs `breakroom gate --event prompt --host antigravity`.

- **When it fires:** Before the model executes on each turn.
- **Function:** Extracts the latest human prompt from the session transcript (`transcript.jsonl`) and records it in Breakroom (`RecordPrompt`). If unread messages are waiting, it injects them into the model's context via an `injectSteps` ephemeral message envelope.
- **Why it matters:** Backs `blueprint_approve` (roadmap approval) and `blueprint_verdict` (acceptance/fixes) so they can be accepted directly from chat without manual CLI commands, and delivers pending peer mail directly to the agent before it begins answering a user prompt.

### 2. `Stop` hook (Stop gate, holds & reminders)

Runs `breakroom gate --event stop --host antigravity`.

- **When it fires:** When the agent execution loop attempts to terminate.
- **Function:** When waiting on blueprint peers (e.g. builder waiting on phase specs, architect waiting on deliverables), the gate holds the agent at zero token cost until mail arrives or the hold timeout expires. When mail arrives, the hook returns `{"decision":"continue"}` to wake the model immediately with the unread messages.
- **Blueprint reminders:** Antigravity supplies `executionNum` in its Stop hook payload. On the first stop attempt (`executionNum: 1`), if an open blueprint phase or inspection is pending, Breakroom returns `{"decision":"continue"}` with a reminder so the agent finishes work. If the agent stops a second time (`executionNum > 1`), Breakroom allows termination without looping. Every allowed stop returns `{"decision":"stop"}`: Antigravity requires a `decision`, and any value other than `continue` lets the agent stop.

### 3. PostToolUse (`check`) hook

There is no `check` (`PostToolUse`) hook: Antigravity's `PostToolUse` hook cannot inject model context. Unread mail is delivered via `PreInvocation` on prompt, `Stop` hook wakeups, and `_unread_messages` attached to Breakroom tool responses.

| Capability | Supported |
| --- | :---: |
| Autonomous | ✅ |
| Stop gate (hold + mail) | ✅ |
| Blueprint reminders | ✅ |
| Approve roadmap from chat | ✅ |
| Mail on prompt | ✅ |
| Mail between tool calls | ❌ |
| Self-contained plugin | ✅ |
| Global scope | ✅ (default: `~/.gemini/config/plugins/breakroom`; works best) |
| Project scope | ✅ (`.agents/plugins/breakroom`) |
| Tested end to end | ✅ |

> [!NOTE]
> Validated on Antigravity CLI v1.4.2.
---

## Troubleshooting

| Symptom | Cause | Fix |
| --- | --- | --- |
| `command not found`, server won't start | GUI app without your shell's `PATH` | Set absolute binary path (`which breakroom`) in `mcp_config.json` and `hooks.json` |
| `call_mcp_tool` missing in agent session | Plugin not discovered or invalid | Run `agy plugin list`, then `agy plugin validate ~/.gemini/config/plugins/breakroom` |
| `worktree_escape` on `register_agent` | Server started outside the repo | Add `--dir <worktree>` to args in `mcp_config.json` |
| Agents in one repo can't see each other | Different workspace roots | Ensure the plugin is installed in each worktree or globally |
