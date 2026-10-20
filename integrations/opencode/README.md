# OpenCode

Breakroom integration for [OpenCode](https://opencode.ai).

This folder provides the integration for OpenCode, packaged as a native TypeScript plugin in [`breakroom.ts`](breakroom.ts):

- [`breakroom.ts`](breakroom.ts): Complete OpenCode plugin implementing dynamic MCP registration, `session.idle` stop gating & session wakeup, and chat prompt recording.
- [`opencode.json`](opencode.json): Standalone MCP server config, for a setup without the plugin.

---

## How plugins work in OpenCode

According to the [OpenCode Plugins specification](https://opencode.ai/docs/plugins/), OpenCode discovers and executes JavaScript and TypeScript plugins placed in:

- `.opencode/plugins/` (project scope), or
- `~/.config/opencode/plugins/` (global user scope across all projects).

OpenCode runs Bun under the hood and executes TypeScript plugin files directly without any build or compile step.

When bundled as a plugin:

1. **MCP servers:** OpenCode dynamically registers the `breakroom` MCP server (`type: "local"`, `command: ["breakroom", "mcp"]`) via `ctx.mcp.transform` (v2) or `config` hook (v1). No manual editing of `opencode.json` is necessary.
2. **Stop gate & session wakeup:** The `session.idle` event runs `breakroom gate --event stop --host opencode`. When waiting on blueprint peers (e.g. builder waiting on phase specs, architect waiting on deliverables), the gate holds at zero model tokens until mail arrives or the hold timeout expires. When new mail arrives, the plugin wakes the session by sending the unread messages as a new prompt (`session.promptAsync`, falling back to `session.prompt`).
3. **Prompt recording:** Prompt and message hooks (`ctx.session.hook("prompt")` in v2; `experimental.chat.messages.transform` and `message.updated` in v1) record user prompts with `breakroom gate --event prompt --host opencode`, enabling roadmap approvals (`blueprint_approve`) and verdicts directly from chat.
4. **Skills:** OpenCode automatically discovers and loads skills placed in `.opencode/skills/breakroom/SKILL.md` or `~/.config/opencode/skills/breakroom/SKILL.md`.

---

## Install instructions

### Option 1: Automatic CLI install (Recommended)

To install globally for all OpenCode projects and worktrees (default):

```sh
breakroom install --host opencode
```

This deploys `breakroom.ts` to `~/.config/opencode/plugins/breakroom.ts` and the Breakroom skills and reference documentation to `~/.config/opencode/skills/breakroom/`.

To install into a specific project instead:

```sh
breakroom install --host opencode /path/to/project
```

This deploys to `.opencode/plugins/breakroom.ts` and `.opencode/skills/breakroom/` in that repository.

After installation, launch or restart OpenCode:

```sh
opencode
```

> [!TIP]
> Verify that `breakroom` is in your system `PATH` (e.g. `/opt/homebrew/bin/breakroom` on Apple Silicon, `/usr/local/bin/breakroom` on Intel, or `~/go/bin/breakroom` for `go install`).

---

### Option 2: Manual plugin installation

#### Globally (User scope - across all projects -- recommended)

```sh
mkdir -p ~/.config/opencode/plugins ~/.config/opencode/skills/breakroom/docs
cp integrations/opencode/breakroom.ts ~/.config/opencode/plugins/breakroom.ts
cp SKILL.md ~/.config/opencode/skills/breakroom/SKILL.md
cp docs/tool-contracts.md ~/.config/opencode/skills/breakroom/docs/tool-contracts.md
cp docs/blueprint-spec.md ~/.config/opencode/skills/breakroom/docs/blueprint-spec.md
```

#### Per-Project (Workspace scope)

```sh
mkdir -p .opencode/plugins .opencode/skills/breakroom/docs
cp integrations/opencode/breakroom.ts .opencode/plugins/breakroom.ts
cp SKILL.md .opencode/skills/breakroom/SKILL.md
cp docs/tool-contracts.md .opencode/skills/breakroom/docs/tool-contracts.md
cp docs/blueprint-spec.md .opencode/skills/breakroom/docs/blueprint-spec.md
```

---

### Option 3: Standalone MCP configuration (no plugin)

If you prefer not to use plugins:

1. Copy or merge [`opencode.json`](opencode.json) into `opencode.json` at the repo root or `~/.config/opencode/opencode.json`.
2. Copy the skill into `.opencode/skills/breakroom/` (the `cp SKILL.md` and `cp docs/...` lines from Option 2; `breakroom install` would add the plugin too).
3. Note: Without the plugin, execution cannot automatically hold or wake sessions across turns; agents park with `wait_for_message` and roadmaps are approved with `breakroom blueprint approve <name>`.

---

## How it works

Every host runs the same thing: a stdio MCP server whose command is `breakroom mcp`. No URL, port, or key. Breakroom automatically connects to the daemon and resolves the workspace repository root.

OpenCode starts the MCP server defined by the plugin and routes model tool calls to `breakroom`.

### Hook behaviors

#### Stop gate (`session.idle`)

`breakroom gate --event stop --host opencode`

When the model finishes its turn and the session transitions to idle:

- If unread mail is waiting, or a blueprint peer handoff arrives, the gate outputs `{"decision":"block","reason":"..."}`.
- While waiting on a peer (e.g. builder waiting on phase spec), the OS process is held at zero model tokens for up to `--hold` (default 540s).
- If mail arrives, the plugin wakes the OpenCode session by sending the delivered message text as a new prompt (`client.session.promptAsync`, falling back to `client.session.prompt`). Gate state is tracked per session.

#### Chat prompt recording

`breakroom gate --event prompt --host opencode`

Records user prompt submissions, allowing the architect to verify human roadmap approvals and acceptance verdicts directly from chat messages.

#### No post-tool mail check

The plugin does not run `breakroom check` after tool calls: `tool.execute.after` output never reaches the model, and `check` marks what it prints as read, so mail would be lost. Mail arrives through the stop gate and on Breakroom tool responses.

| Capability | Supported |
| --- | :---: |
| Autonomous | ✅ |
| Stop gate (hold + mail) | ✅ |
| Blueprint reminders | ✅ |
| Approve roadmap from chat | ✅ |
| Mail on prompt | ❌ (records chat prompt; mail wakes session via stop gate) |
| Mail between tool calls | ❌ (delivery via stop gate / tool response) |
| Self-contained plugin | ✅ |
| Global scope | ✅ (default: `~/.config/opencode/plugins/breakroom.ts`) |
| Project scope | ✅ (`.opencode/plugins/breakroom.ts`) |
| Tested end to end | ✅ |

> [!NOTE]
> Validated on OpenCode v2.0.32.

---

## Check it worked

In OpenCode, run `opencode mcp list` to verify that `breakroom` is connected.

Ask the agent to call `register_agent`: the reply's `peers` lists the other agents in its workspace, so a second agent in another worktree of the same repo should see the first.

Run `breakroom status` in a terminal to inspect active agents and blueprints.

| Symptom | Cause | Fix |
| --- | --- | --- |
| `command not found`, server won't start | OpenCode environment missing `PATH` | Ensure `breakroom` binary is in `/usr/local/bin` or `/opt/homebrew/bin` |
| `worktree_escape` on `register_agent` | Server started outside the repo | Run `opencode` inside the project worktree |
| Agents in one repo can't see each other | Landed in different workspaces | Verify worktree paths and ensure both run in worktrees of the same repo |
