# VS Code (GitHub Copilot)

Breakroom integration for VS Code and GitHub Copilot.

This folder provides the integration for VS Code, packaged as a self-contained **Agent Plugin** in [`breakroom-plugin/`](breakroom-plugin/) following the [VS Code Agent Plugins specification](https://code.visualstudio.com/docs/agent-customization/agent-plugins) and the open [Agent Plugins 1.0 standard](https://agent-plugins.org/):

- [`breakroom-plugin/plugin.json`](breakroom-plugin/plugin.json): Agent Plugins 1.0 manifest with canonical `$schema`.
- [`breakroom-plugin/mcp.json`](breakroom-plugin/mcp.json): Portable MCP server configuration (`breakroom mcp`).
- [`breakroom-plugin/com.github.copilot/hooks/hooks.json`](breakroom-plugin/com.github.copilot/hooks/hooks.json): Lifecycle hooks in the `com.github.copilot` extension namespace (`Stop` hold gate, `UserPromptSubmit` prompt recording, and `PostToolUse` checks).
- [`breakroom-plugin/skills/breakroom/`](breakroom-plugin/skills/breakroom/): Coordination skill and reference documentation ([`SKILL.md`](breakroom-plugin/skills/breakroom/SKILL.md), [`docs/tool-contracts.md`](breakroom-plugin/skills/breakroom/docs/tool-contracts.md), [`docs/blueprint-spec.md`](breakroom-plugin/skills/breakroom/docs/blueprint-spec.md)).

---

## How plugins work in VS Code

According to the [VS Code Agent Plugins documentation](https://code.visualstudio.com/docs/agent-customization/agent-plugins), VS Code supports Agent Plugins to bundle skills, tools, and hooks into an installable package:

1. **Discovery:** VS Code automatically discovers plugins installed globally under `~/.copilot/installed-plugins/` (the same folder used by the GitHub Copilot CLI), or locally registered in settings via `chat.pluginLocations`.
2. **MCP servers:** VS Code starts the portable MCP server defined in `mcp.json` (`breakroom mcp`) and makes Breakroom tools available to agents.
3. **Lifecycle hooks:** VS Code reads hooks defined under `com.github.copilot/hooks/hooks.json`:
   - `Stop`: Runs `breakroom gate --event stop --host vscode`, holding the session at zero model tokens when waiting on blueprint peers and delivering peer mail upon wakeup.
   - `UserPromptSubmit`: Runs `breakroom gate --event prompt --host vscode`, recording chat prompts for direct roadmap approvals and acceptance verdicts.
   - `PostToolUse`: Runs `breakroom check --hook-event PostToolUse` after tool executions.
4. **Skills:** VS Code automatically discovers the `breakroom` skill from `skills/breakroom/SKILL.md` (and reference documents in `docs/`).

---

## Install instructions

### Option 1: Automatic CLI install (Recommended)

To install globally across all VS Code workspaces and Copilot sessions (default):

```sh
breakroom install --host vscode
```

This deploys the plugin to `~/.copilot/installed-plugins/_direct/breakroom`. VS Code and Copilot CLI discover it automatically.

To install into a specific project instead:

```sh
breakroom install --host vscode /path/to/project
```

This deploys to `.vscode/plugins/breakroom` in that project and enables it in `.vscode/settings.json` (created if missing, merged if present) with:

```json
{
  "chat.pluginLocations": {
    ".vscode/plugins/breakroom": true
  }
}
```

The path is relative to the workspace, so the settings file works for anyone who clones the repo. If `settings.json` has comments the installer can't edit safely, it prints this snippet for you to add. `breakroom uninstall --host vscode /path/to/project` removes the entry again.

After installation:

1. Reload VS Code (Command Palette: `Developer: Reload Window`).
2. Start the MCP server: Command Palette (`Cmd+Shift+P`) → `MCP: List Servers` → `breakroom` → `Start Server`.
3. Open the Agent Customizations editor (`Chat: Open Customizations`) or run `/plugin list` in Copilot chat to verify that `breakroom` is active.

> [!TIP]
> GUI apps may not inherit your shell's `PATH`. If VS Code reports that `breakroom` is not found, verify that the binary is in `/usr/local/bin` or `/opt/homebrew/bin` (or `~/go/bin/breakroom`).

---

### Option 2: Manual plugin installation

#### Globally (User scope - across all projects -- recommended)

Copy `breakroom-plugin` to `~/.copilot/installed-plugins/_direct/breakroom`:

```sh
mkdir -p ~/.copilot/installed-plugins/_direct
cp -r integrations/vscode/breakroom-plugin ~/.copilot/installed-plugins/_direct/breakroom
```

Then reload the VS Code window (`Developer: Reload Window`).

#### Per-Project (Workspace scope)

Copy `breakroom-plugin` to `.vscode/plugins/breakroom` in your workspace:

```sh
mkdir -p .vscode/plugins
cp -r integrations/vscode/breakroom-plugin .vscode/plugins/breakroom
```

And add to `.vscode/settings.json`:

```json
{
  "chat.pluginLocations": {
    ".vscode/plugins/breakroom": true
  }
}
```

#### Copilot harness (hooks only)

The plugin's hooks target the Local harness. The Copilot harness uses a different hook schema (`agentStop`, `userPromptSubmitted`), so its hooks ship separately in [`github-hooks/breakroom-copilot.json`](github-hooks/breakroom-copilot.json). `breakroom install --host vscode` writes it next to the plugin: to `~/.copilot/hooks/` for a global install, or to `.github/hooks/` in the repository for a workspace install (commit it; requires a trusted workspace).

It has no `PostToolUse` check hook, because Copilot sends a different payload there. Verify discovery with **Chat: Configure Hooks**.

---

## How it works

Every host runs the same thing: a stdio MCP server whose command is `breakroom mcp`. No URL, port, or key. Breakroom automatically connects to the daemon and resolves the workspace repository root.

### Hook behaviors

#### Stop gate (`Stop` / `agentStop`)

`breakroom gate --event stop --host vscode`

When the agent finishes its turn:

- If unread mail is waiting, or a blueprint peer handoff arrives, the gate outputs a block decision to wake the agent.
- While waiting on a peer (e.g. builder waiting on phase spec), the gate holds at zero model tokens for up to `--hold` (default 540s).
- The stop timeout (600s) exceeds the hold duration (540s).

#### Prompt recording (`UserPromptSubmit` / `userPromptSubmitted`)

`breakroom gate --event prompt --host vscode`

Records user chat submissions, enabling roadmap approvals (`blueprint_approve`) and verdicts directly from chat.

#### Tool checks (`PostToolUse`)

`breakroom check --hook-event PostToolUse`

Runs a fail-open post-tool check after every tool call.

| Capability | Supported |
| --- | :---: |
| Autonomous | ✅ |
| Stop gate (hold + mail) | ✅ |
| Blueprint reminders | ✅ |
| Approve roadmap from chat | ✅ |
| Mail on prompt | ❌ (records chat prompt; mail wakes agent via stop gate) |
| Mail between tool calls | ✅ (Local harness) |
| Self-contained plugin | ✅ |
| Global scope | ✅ (default: `~/.copilot/installed-plugins/_direct/breakroom`) |
| Project scope | ✅ (`.vscode/plugins/breakroom`, enabled via `chat.pluginLocations`) |
| Tested end to end | ✅ |

> [!NOTE]
> Validated on VS Code v1.139.1.

---

## Check it worked

In VS Code Chat, verify the plugin appears in `Chat: Open Customizations` or `/plugin list`.

Ask the agent to call `register_agent`: the reply's `peers` lists the other agents in its workspace. `breakroom status` in a terminal shows active agents and blueprints.

| Symptom | Cause | Fix |
| --- | --- | --- |
| `command not found`, server won't start | VS Code environment missing `PATH` | Ensure `breakroom` is in `/usr/local/bin`, `/opt/homebrew/bin`, or `~/go/bin` |
| `worktree_escape` on `register_agent` | Server started outside the repo | Add `--dir <worktree>` to MCP server args |
| Agents in one repo can't see each other | Landed in different workspaces | Verify worktrees belong to the same git repository |
| Plugin does not appear | Plugin support disabled | Verify `"chat.plugins.enabled": true` in VS Code settings |
