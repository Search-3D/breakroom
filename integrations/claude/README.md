# Claude Code

Breakroom integration for [Claude Code](https://docs.anthropic.com/en/docs/claude-code), packaged as a native plugin in [`breakroom-plugin/`](breakroom-plugin/).

Claude Code auto-loads any directory under `~/.claude/skills/` (or `<project>/.claude/skills/`) containing `.claude-plugin/plugin.json` as `breakroom@skills-dir` with no marketplace step required.

## Install instructions

### Automated install (recommended)

Install Breakroom directly to your global Claude skills directory (`~/.claude/skills/breakroom`):

```sh
breakroom install --host claude
```

Or for project-level installation:

```sh
breakroom install --host claude /path/to/project
```

The installer configures:

- `.claude-plugin/plugin.json`: Plugin manifest
- `.mcp.json`: stdio MCP server (`breakroom mcp`)
- `hooks/hooks.json`: Stop, UserPromptSubmit, and PostToolUse lifecycle gates
- `skills/breakroom/`: Agent coordination skill instructions

### Permissions configuration

Claude Code prefixes plugin MCP tools with `mcp__plugin_breakroom_breakroom__*`.

To auto-approve Breakroom tools and avoid confirmation prompts on each call, add the following to your `~/.claude/settings.json` (or `.claude/settings.json`):

```json
{
  "permissions": {
    "allow": ["mcp__plugin_breakroom_breakroom"]
  }
}
```

## How it works

Every host runs the same thing: a stdio MCP server whose command is `breakroom mcp`. No URL, port, or key. Claude Code starts servers in the directory you ran `claude` from: run it inside the worktree, and no `--dir` is needed.

### Hook behaviors

Claude Code supports all three hooks (all optional, all fail-open):

#### Stop hook

`breakroom gate --event stop --host claude`

Unread mail blocks the stop and is handed to the model. Blueprint waiting is held inside the hook at zero model tokens until mail arrives or `--hold` (default 540s) expires. Keep `--hold` below the Stop hook's `timeout: 600`.

#### UserPromptSubmit hook

`breakroom gate --event prompt --host claude`

Records prompt submissions (enabling the architect to record roadmap approval from a chat reply) and delivers unread mail into context.

#### PostToolUse hook

`breakroom check --hook-event PostToolUse`

Delivers mail between tool calls using the structured JSON envelope.

| Capability | Supported |
| --- | :---: |
| Autonomous | ✅ |
| Stop gate (hold + mail) | ✅ |
| Blueprint reminders | ✅ |
| Approve roadmap from chat | ✅ |
| Mail on prompt | ✅ |
| Mail between tool calls | ✅ |
| Self-contained plugin | ✅ |
| Global scope | ✅ (default: `~/.claude/skills/breakroom`) |
| Project scope | ✅ (`.claude/skills/breakroom`) |
| Tested end to end | ✅ |

> [!NOTE]
> Validated on Claude Code v2.1.302.

## Check it worked

In Claude Code, verify plugin discovery with `/plugin` (it should list `breakroom@skills-dir`) and verify MCP tools with `/mcp`. Ask the agent to call `register_agent`: `peers` lists other active agents in the workspace. Run `breakroom status` in your terminal to see connected agents.
