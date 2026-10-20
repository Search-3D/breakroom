# Codex

Breakroom integration for Codex (CLI and IDE extension), packaged as a native plugin in [`breakroom-plugin/`](breakroom-plugin/).

The plugin bundles:

- `.codex-plugin/plugin.json`: Plugin manifest pointing to skills
- `.mcp.json`: stdio MCP server (`breakroom mcp`)
- `hooks/hooks.json`: prompt and stop gate lifecycle hooks
- `skills/breakroom/`: Agent coordination skill instructions

## Install instructions

### Automated install (recommended)

Install Breakroom to your global plugins directory (`~/.agents/plugins/breakroom`):

```sh
breakroom install --host codex
```

Or for a specific project:

```sh
breakroom install --host codex /path/to/project
```

The installer:

1. Copies the plugin bundle into `.agents/plugins/breakroom`.
2. Registers the plugin in `.agents/plugins/marketplace.json`.

`.codex-plugin/` and `.mcp.json` are dotfiles, so a plain `ls` shows only `hooks` and `skills`; use `ls -a`. The marketplace file sits in `.agents/plugins/`, beside the plugin folder, by design.

`breakroom uninstall --host codex` removes the plugin, its marketplace entry, and Breakroom's entries in `config.toml` (`[plugins."breakroom@…"]`, trusted hook hashes, and any `[mcp_servers.breakroom]`).

### Activating in Codex

1. Launch `codex` (CLI or desktop app).
2. Open `/plugins` and enable **breakroom**.
3. Open `/hooks` to review and trust the plugin lifecycle hooks.
4. Keep `tool_timeout_sec` at `60` or higher in `~/.codex/config.toml` (or `<repo>/.codex/config.toml`).

## How it works

Every host runs the same thing: a stdio MCP server whose command is `breakroom mcp`. No URL, port, or key. The IDE extension may start servers outside the project; for that case, pass the project with `breakroom mcp --dir /path/to/worktree`.

### Hook behaviors

#### Stop hook

`breakroom gate --event stop --host codex`

Unread mail blocks the stop. When waiting on a blueprint peer, the agent is held at zero model tokens until mail arrives or `--hold` (default 540s) expires. If the builder connects early, Breakroom parks it in the stop gate and wakes it immediately upon phase approval or peer handoff. Keep the Stop hook's `timeout` (600) above `--hold` (default 540).

#### UserPromptSubmit hook

`breakroom gate --event prompt --host codex`

Records your reply (such as roadmap approval) and delivers unread mail.

#### PostToolUse (`check`) hook

There is no `check` (`PostToolUse`) hook: its output has no model-visible context channel in Codex, so mail sent there would be marked read unseen.

| Capability | Supported |
| --- | :---: |
| Autonomous | ✅ |
| Stop gate (hold + mail) | ✅ |
| Blueprint reminders | ✅ |
| Approve roadmap from chat | ✅ |
| Mail on prompt | ✅ |
| Mail between tool calls | ❌ |
| Self-contained plugin | ✅ |
| Global scope | ✅ (default: `~/.agents/plugins/breakroom`) |
| Project scope | ✅ (`.agents/plugins/breakroom`) |
| Tested end to end | ✅ |

> [!NOTE]
> Validated on Codex CLI v0.163.0-alpha.10.2.

## Check it worked

In Codex, verify the plugin under `/plugins` and hooks under `/hooks`. Ask the agent to call `register_agent`: the reply's `peers` lists active agents in its workspace. `breakroom status` in a terminal displays connected agents.
