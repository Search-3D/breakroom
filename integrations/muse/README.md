# Muse Code

Breakroom integration for [Muse Code](https://dev.meta.ai/docs/muse-code), packaged as a native Muse plugin in [`breakroom-plugin/`](breakroom-plugin/):

- [`.muse-plugin/plugin.json`](breakroom-plugin/.muse-plugin/plugin.json): plugin manifest declaring the MCP server (`breakroom mcp`), lifecycle hooks (`Stop` hold gate and `UserPromptSubmit` prompt recorder), and the coordination skill.
- [`.claude-plugin/marketplace.json`](breakroom-plugin/.claude-plugin/marketplace.json): marketplace catalog descriptor used by Muse.
- [`skills/breakroom/`](breakroom-plugin/skills/breakroom/): the coordination skill and reference documents ([`SKILL.md`](breakroom-plugin/skills/breakroom/SKILL.md), [`docs/tool-contracts.md`](breakroom-plugin/skills/breakroom/docs/tool-contracts.md), [`docs/blueprint-spec.md`](breakroom-plugin/skills/breakroom/docs/blueprint-spec.md)).

## Install instructions

### 1. Write the plugin bundle

Globally (default):

```sh
breakroom install --host muse
```

This writes the bundle to `~/.config/muse/plugins/breakroom`.

For a single project instead:

```sh
breakroom install --host muse /path/to/project
```

This writes the bundle to `.muse/plugins/breakroom` in that project.

### 2. Register and approve it in Muse

Muse installs plugins into its own cache and picks the scope at this step. For a global bundle:

```sh
muse plugins install ~/.config/muse/plugins/breakroom --scope user
muse plugins approve breakroom
```

For a project bundle, run `muse plugins install /path/to/project/.muse/plugins/breakroom --scope project` instead, then approve.

> [!IMPORTANT]
> `muse plugins approve breakroom` is required. It trusts the plugin's MCP server and lifecycle hooks; without it, the plugin is installed but inert.

Launch Muse and verify:

- `/plugins` lists `breakroom` as installed and approved.
- `/mcp` lists the Breakroom tools (`register_agent`, `send_message`, …).

`muse plugins validate <bundle path>` checks a bundle without installing it.

### Updating

After installing a new Breakroom version, run `breakroom install --host muse` again (with the same directory, if you used one), then refresh Muse's cached copy:

```sh
muse plugins update breakroom
```

To remove it: `breakroom uninstall --host muse` (add the project directory for a project install), then `muse plugins remove breakroom`.

### Manual setups

Breakroom also works configured by hand: the MCP server under `mcpServers.breakroom` and hooks under `hooks` in `~/.config/muse/settings.json` (user scope), or hooks in `<project>/.muse/hooks.json` (project scope). Use either that or the plugin, not both: with both, every hook runs twice. `breakroom install --host muse` warns when it finds a manual setup, and `breakroom uninstall --host muse` removes one in the scope it uninstalls (user settings for a global uninstall, the project's `.muse/hooks.json` for a project one). It removes only Breakroom's entries, keeps the rest of the file and its key order, and deletes a hooks file left empty.

---

## How it works

Every host connects to the Breakroom daemon using stdio: `breakroom mcp`. Each agent session spawns its own stdio process, which attaches to the background broker daemon.

### Hook behaviors and cache optimization

The Stop gate and UserPromptSubmit hooks run with `--host muse`:

#### 1. Stop gate (`breakroom gate --event stop --host muse`)

Parks waiting agents inside the OS process at zero token cost while waiting on blueprint peers.

- **Cache-safe wakeup**: Muse Code places Stop hook reasons into the top-of-prompt developer context. Varying message text there would invalidate the model's prefix prompt cache. With `--host muse`, Breakroom emits a **constant static wakeup string** on the Stop hook, leaving unread messages intact for the agent to pull on its next Breakroom tool call. This keeps the prompt cache intact.

#### 2. UserPromptSubmit gate (`breakroom gate --event prompt --host muse`)

Records user prompts for direct roadmap approvals (`blueprint_approve`) and acceptance verdicts. In Muse, prompt hook text is suppressed from injecting into prompt headers to prevent prefix cache churn.

#### 3. PostToolUse hook

Deliberately omitted: Muse Code injects hook output at the top of the conversation context, which would disrupt prompt cache reuse.

| Capability | Supported |
| --- | :---: |
| Autonomous | ✅ |
| Stop gate (hold + mail) | ✅ (static wakeup; mail via next tool call) |
| Blueprint reminders | ✅ |
| Approve roadmap from chat | ✅ |
| Mail on prompt | ❌ (records prompt only) |
| Mail between tool calls | ❌ (no `check` hook) |
| Self-contained plugin | ✅ |
| Global scope | ✅ (default: `~/.config/muse/plugins/breakroom`, `--scope user`) |
| Project scope | ✅ (`.muse/plugins/breakroom`, `--scope project`) |
| Tested end to end | ✅ |

> [!NOTE]
> Validated on Muse CLI v1.4.10.

---

## Troubleshooting

| Symptom | Cause | Solution |
| --- | --- | --- |
| Tools not appearing in session | Plugin not approved | Run `muse plugins approve breakroom` |
| Old hooks or tools after updating Breakroom | Muse runs its cached copy | Run `muse plugins update breakroom` |
| Command `breakroom` not found | GUI app didn't inherit `$PATH` | Reinstall with `breakroom install --host muse --bin-path "$(which breakroom)"`, then `muse plugins update breakroom` |
| Unread mail not displaying inline | Muse cache protection | Expected behavior: call `wait_for_message` or check `_unread_messages` on your next tool call |
