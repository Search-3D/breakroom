# Cursor

Breakroom integration for [Cursor](https://cursor.com).

This folder provides the integration for [Cursor](https://cursor.com), packaging a self-contained **Cursor Plugin** in [`breakroom-plugin/`](breakroom-plugin/):

- [`breakroom-plugin/plugin.json`](breakroom-plugin/plugin.json): Cursor plugin manifest.
- [`breakroom-plugin/hooks/hooks.json`](breakroom-plugin/hooks/hooks.json): prompt + stop gate hooks.
- [`breakroom-plugin/mcp.json`](breakroom-plugin/mcp.json): stdio MCP server config (`breakroom mcp`).
- [`breakroom-plugin/skills/breakroom/`](breakroom-plugin/skills/breakroom/): The Breakroom coordination skill and reference documentation ([`SKILL.md`](breakroom-plugin/skills/breakroom/SKILL.md), [`docs/tool-contracts.md`](breakroom-plugin/skills/breakroom/docs/tool-contracts.md), [`docs/blueprint-spec.md`](breakroom-plugin/skills/breakroom/docs/blueprint-spec.md)).

## How plugins work in Cursor

According to the [Cursor Plugins specification](https://cursor.com/docs/reference/plugins), Cursor automatically discovers and loads local plugins placed in `~/.cursor/plugins/local/<plugin-name>`.

When bundled as a plugin:

1. **MCP servers:** Cursor registers the MCP server defined in `mcp.json` and starts `breakroom mcp`.
2. **Lifecycle hooks:** Cursor registers the lifecycle hooks defined in `hooks/hooks.json` (`beforeSubmitPrompt` for prompt recording and `stop` for peer holds and mail delivery).
3. **Skills:** Cursor discovers the `breakroom` skill from `skills/breakroom/SKILL.md` (and its reference documents in `docs/`).

---

## Install instructions

### Option 1: Automatic CLI install (Recommended)

To install globally for all Cursor sessions and worktrees (default):

```sh
breakroom install --host cursor
```

This deploys the complete Breakroom plugin directly to `~/.cursor/plugins/local/breakroom` (global user scope across all workspaces).

To install into a specific project instead:

```sh
breakroom install --host cursor /path/to/project
```

Cursor loads plugins only from the global `~/.cursor/plugins/local`, so a project install writes native project config instead: it merges the `breakroom` server into `.cursor/mcp.json`, merges the Stop and prompt hooks into `.cursor/hooks.json` (existing entries are kept), and copies the skill to `.cursor/skills/breakroom/`. `breakroom uninstall --host cursor /path/to/project` removes only Breakroom's entries.

After installation:

1. Restart Cursor or open Command Palette (`Cmd+Shift+P` / `Ctrl+Shift+P`) and run **Developer: Reload Window**.
2. Open **Customize** in the sidebar to confirm that the `breakroom` plugin, skills, and MCP server appear.

> [!TIP]
> GUI apps may not inherit your shell's `PATH`. If Cursor reports that the command is not found, verify that `breakroom` is in your system path (e.g., `/opt/homebrew/bin/breakroom` on Apple Silicon, `/usr/local/bin/breakroom` on Intel, or `~/go/bin/breakroom` for `go install`).

---

### Option 2: Manual plugin installation

#### Globally (User scope - across all projects -- recommended)

Copy or symlink `breakroom-plugin` to `~/.cursor/plugins/local/breakroom`:

```sh
mkdir -p ~/.cursor/plugins/local
cp -r integrations/cursor/breakroom-plugin ~/.cursor/plugins/local/breakroom
```

Then reload Cursor via **Developer: Reload Window**.

#### Per-Project (Workspace scope)

Cursor does not read plugins from a project folder. Use the project config files in Option 3 below, or run `breakroom install --host cursor /path/to/project`.

---

### Option 3: Standalone project copy

If you prefer not to use plugins:

1. Copy [`breakroom-plugin/mcp.json`](breakroom-plugin/mcp.json) to `.cursor/mcp.json` in your project root.
2. Copy [`breakroom-plugin/hooks/hooks.json`](breakroom-plugin/hooks/hooks.json) to `.cursor/hooks.json` in your project root.
3. Copy [`breakroom-plugin/skills/breakroom/SKILL.md`](breakroom-plugin/skills/breakroom/SKILL.md) and docs to `.cursor/skills/breakroom/`.

---

## How it works

Every host runs the same thing: a stdio MCP server whose command is `breakroom mcp`. No URL, port, or key. Breakroom automatically connects to the daemon and resolves the workspace repository root. Connection logs are in View → Output → MCP.

Cursor starts one server per window and routes every chat in that window through it, so chats share one process. Each chat registers its own identity and passes it as `caller_id` on every later call; without it, calls fail with `caller_required` instead of acting as the wrong chat. Hooks can't tell chats in one window apart either, so there they stay conservative: a stop wakeup says only that mail may be waiting (the chat reads it with its next Breakroom call), and your prompts back approvals only when one of the chats is an architect. For the smoothest blueprint, run the architect and builder in separate windows.

### Hook behaviors

Cursor loads hooks from `hooks/hooks.json` (or `.cursor/hooks.json`).

#### stop hook

`breakroom gate --event stop --host cursor`

Unread mail blocks the stop and arrives as an automatic follow-up. Blueprint waiting is held at zero model tokens until mail arrives or `--hold` (default 540s) expires. Follow-ups are capped at 5 per conversation, so a parked wait ends after about 45 minutes. Aborted or errored turns are skipped, so interrupting the agent doesn't resurrect it.

#### beforeSubmitPrompt hook

`breakroom gate --event prompt --host cursor`

Record-only: prompt hooks have no channel to show mail, so delivery would mark mail read unseen. Recording still enables roadmap approval from chat.

#### Third-party imports and fallbacks

Cursor also loads `.claude/settings.json` hooks when Third-Party Imports is enabled (the default): in repos shared with Claude Code, point the shared hooks at `--host cursor` too (safe on both; Claude prompt-mail then waits for the stop gate instead).

There is no `check` hook. Park mid-turn with `wait_for_message`. Unread mail still comes in `_unread_messages` on any Breakroom tool response.

| Capability | Supported |
| --- | :---: |
| Autonomous | ✅ |
| Stop gate (hold + mail) | ✅ |
| Blueprint reminders | ✅ |
| Approve roadmap from chat | ✅ |
| Mail on prompt | ❌ (prompt hook records only) |
| Mail between tool calls | ❌ (no `check` hook) |
| Self-contained plugin | ✅ |
| Global scope | ✅ (default: `~/.cursor/plugins/local/breakroom`) |
| Project scope | ✅ (`.cursor/mcp.json`, `.cursor/hooks.json`, `.cursor/skills/breakroom`) |
| Tested end to end | ✅ |

> [!NOTE]
> Validated on Cursor v3.25.9.

## Check it worked

Ask the agent to call `register_agent`: the reply's `peers` lists the other agents in its workspace, so a second agent in another worktree of the same repo should see the first. `breakroom status` in a terminal shows how many agents are present.

| Symptom | Cause | Fix |
| --- | --- | --- |
| `command not found`, server won't start | GUI app without your `PATH` | absolute path from `which breakroom` |
| `worktree_escape` on `register_agent` | server started outside the repo | add `--dir <worktree>` |
| agents in one repo can't see each other | they landed in different (wrong) workspaces | add `--dir`, then re-register |
| `--dir: no such file or directory` | a variable didn't expand (wrong host, or global config) | use a project config or an absolute path |
