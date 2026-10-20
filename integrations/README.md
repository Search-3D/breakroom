# Integrations

Per-host Breakroom setup: MCP configs, hooks, and workarounds for each
agent host. Each folder holds its config files plus a README with install
steps.

Every host runs the same thing: a stdio MCP server whose command is
`breakroom mcp`. No URL, port, or key. Each agent session spawns its own
`breakroom mcp` process, which starts the shared daemon if it isn't
running.

The server must run inside the repo (the project folder or any of its
worktrees). Breakroom picks the workspace (the repo, shared by all its
worktrees) from the directory `breakroom mcp` starts in, and
`register_agent`'s `worktree_path: "."` means that directory too. CLI
hosts start servers in the directory you launched them from, so they
just work. Some GUI hosts start servers in `$HOME` or `/`; for those,
pass the project with `--dir`:

```sh
breakroom mcp --dir /path/to/worktree
```

GUI apps may not see your shell's `PATH`. If the host says the command
isn't found, use the absolute path from `which breakroom` (typically
`/opt/homebrew/bin/breakroom` for Homebrew on Apple Silicon,
`/usr/local/bin/breakroom` on Intel, `~/go/bin/breakroom` for
`go install`).

On macOS, if the path is right and the server still won't start right
after installing or upgrading, Gatekeeper is likely blocking the binary:
allow it once as described in the main README's
[Install](../README.md#install) section.

| Host | Folder | Hooks |
| --- | --- | --- |
| Claude Code | [claude/](claude/README.md) | Self-contained plugin ([plugin.json](claude/breakroom-plugin/.claude-plugin/plugin.json)), Stop + prompt + `check`, tested ✅ |
| Muse Code | [muse/](muse/README.md) | Self-contained plugin ([plugin.json](muse/breakroom-plugin/.muse-plugin/plugin.json)), Stop + prompt hooks, tested ✅ |
| Codex | [codex/](codex/README.md) | Self-contained plugin ([plugin.json](codex/breakroom-plugin/.codex-plugin/plugin.json)), Stop + prompt (`--host codex`), tested ✅ |
| Antigravity | [antigravity/](antigravity/README.md) | Self-contained plugin ([plugin.json](antigravity/breakroom-plugin/plugin.json)), Stop + PreInvocation hooks, tested ✅ |
| OpenCode | [opencode/](opencode/README.md) | Self-contained plugin ([breakroom.ts](opencode/breakroom.ts)), Stop + prompt hooks, tested ✅ |
| Cursor | [cursor/](cursor/README.md) | Self-contained plugin ([plugin.json](cursor/breakroom-plugin/plugin.json)), Stop + prompt hooks, tested ✅ |
| VS Code (GitHub Copilot) | [vscode/](vscode/README.md) | Self-contained plugin ([plugin.json](vscode/breakroom-plugin/plugin.json)), Stop + prompt + `check` hooks, tested ✅ |

## Other hosts

Anything that speaks MCP over stdio works: command `breakroom`, args
`["mcp"]`, started in the agent's worktree (or
`["mcp", "--dir", "<worktree>"]`). Hosts without Claude Code-style hooks
still get mail: every Breakroom tool response carries `_unread_messages`;
they approve roadmaps with `breakroom blueprint approve <name>`.

`breakroom install --host generic [DIR]` installs just the skill and its
reference docs into `.agents/skills/breakroom`; add the MCP server to the
host yourself. Without hooks, agents wait with `wait_for_message` instead
of ending their turn.
