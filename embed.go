package breakroom

import _ "embed"

// SkillMD contains the contents of SKILL.md.
//
//go:embed SKILL.md
var SkillMD []byte

// ToolContractsMD contains the contents of docs/tool-contracts.md.
//
//go:embed docs/tool-contracts.md
var ToolContractsMD []byte

// BlueprintSpecMD contains the contents of docs/blueprint-spec.md.
//
//go:embed docs/blueprint-spec.md
var BlueprintSpecMD []byte

// AntigravityPluginJSON contains the contents of integrations/antigravity/breakroom-plugin/plugin.json.
//
//go:embed integrations/antigravity/breakroom-plugin/plugin.json
var AntigravityPluginJSON []byte

// AntigravityMcpConfigJSON contains the contents of integrations/antigravity/breakroom-plugin/mcp_config.json.
//
//go:embed integrations/antigravity/breakroom-plugin/mcp_config.json
var AntigravityMcpConfigJSON []byte

// AntigravityHooksJSON contains the contents of integrations/antigravity/breakroom-plugin/hooks.json.
//
//go:embed integrations/antigravity/breakroom-plugin/hooks.json
var AntigravityHooksJSON []byte

// CursorPluginJSON contains the contents of integrations/cursor/breakroom-plugin/plugin.json.
//
//go:embed integrations/cursor/breakroom-plugin/plugin.json
var CursorPluginJSON []byte

// CursorMcpJSON contains the contents of integrations/cursor/breakroom-plugin/mcp.json.
//
//go:embed integrations/cursor/breakroom-plugin/mcp.json
var CursorMcpJSON []byte

// CursorHooksJSON contains the contents of integrations/cursor/breakroom-plugin/hooks/hooks.json.
//
//go:embed integrations/cursor/breakroom-plugin/hooks/hooks.json
var CursorHooksJSON []byte

// OpenCodePluginTS contains the contents of integrations/opencode/breakroom.ts.
//
//go:embed integrations/opencode/breakroom.ts
var OpenCodePluginTS []byte

// VSCodePluginJSON contains the contents of integrations/vscode/breakroom-plugin/plugin.json.
//
//go:embed integrations/vscode/breakroom-plugin/plugin.json
var VSCodePluginJSON []byte

// VSCodeMcpJSON contains the contents of integrations/vscode/breakroom-plugin/mcp.json.
//
//go:embed integrations/vscode/breakroom-plugin/mcp.json
var VSCodeMcpJSON []byte

// VSCodeHooksJSON contains the contents of integrations/vscode/breakroom-plugin/com.github.copilot/hooks/hooks.json.
//
//go:embed integrations/vscode/breakroom-plugin/com.github.copilot/hooks/hooks.json
var VSCodeHooksJSON []byte

// VSCodeCopilotHooksJSON contains the contents of integrations/vscode/github-hooks/breakroom-copilot.json.
//
//go:embed integrations/vscode/github-hooks/breakroom-copilot.json
var VSCodeCopilotHooksJSON []byte

// MusePluginJSON contains the contents of integrations/muse/breakroom-plugin/.muse-plugin/plugin.json.
//
//go:embed integrations/muse/breakroom-plugin/.muse-plugin/plugin.json
var MusePluginJSON []byte

// MuseMarketplaceJSON contains the contents of integrations/muse/breakroom-plugin/.claude-plugin/marketplace.json.
//
//go:embed integrations/muse/breakroom-plugin/.claude-plugin/marketplace.json
var MuseMarketplaceJSON []byte

// ClaudePluginJSON contains the contents of integrations/claude/breakroom-plugin/.claude-plugin/plugin.json.
//
//go:embed integrations/claude/breakroom-plugin/.claude-plugin/plugin.json
var ClaudePluginJSON []byte

// ClaudeMcpJSON contains the contents of integrations/claude/breakroom-plugin/.mcp.json.
//
//go:embed integrations/claude/breakroom-plugin/.mcp.json
var ClaudeMcpJSON []byte

// ClaudeHooksJSON contains the contents of integrations/claude/breakroom-plugin/hooks/hooks.json.
//
//go:embed integrations/claude/breakroom-plugin/hooks/hooks.json
var ClaudeHooksJSON []byte

// CodexPluginJSON contains the contents of integrations/codex/breakroom-plugin/.codex-plugin/plugin.json.
//
//go:embed integrations/codex/breakroom-plugin/.codex-plugin/plugin.json
var CodexPluginJSON []byte

// CodexMcpJSON contains the contents of integrations/codex/breakroom-plugin/.mcp.json.
//
//go:embed integrations/codex/breakroom-plugin/.mcp.json
var CodexMcpJSON []byte

// CodexHooksJSON contains the contents of integrations/codex/breakroom-plugin/hooks/hooks.json.
//
//go:embed integrations/codex/breakroom-plugin/hooks/hooks.json
var CodexHooksJSON []byte
