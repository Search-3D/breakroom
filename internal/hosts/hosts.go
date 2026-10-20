// Package hosts defines agent host capabilities, hook configurations,
// and skill installation targets.
package hosts

import (
	"path/filepath"
	"strings"
)

// PromptContextBehavior dictates how a host responds to UserPromptSubmit events.
type PromptContextBehavior int

const (
	PromptContextSilent PromptContextBehavior = iota // Drops prompt hook output (Codex, Cursor, VS Code, Muse, Copilot, OpenCode; Antigravity injects mail itself)
	PromptContextText                                // Emits check text output into prompt context (Claude Code)
)

// Target defines one supported agent host.
type Target struct {
	ID                     string
	Name                   string
	RelativeSkillDir       string
	StopDecision           string // "decision" value on a block: "block" (default) or "continue" (Antigravity)
	StopEnvelopeFormat     string // "" (flat) or "hookSpecificOutput" (VS Code)
	PromptContext          PromptContextBehavior
	StaticWakeupReason     string // Non-empty if host requires static wakeup string to preserve prompt cache
	SuppressGateMailAck    bool   // True if host leaves mail unread for MCP wait_for_message
	NormalizeStopActive    bool   // True if host has no stop_hook_active flag and assumes true
	RequireCompletedStatus bool   // True if host sends status on abort/error that shouldn't wake mail
	Installable            bool   // Whether shown in interactive install menu
}

// FormatStopPayload formats the host-specific JSON response for a stop gate block.
func (t Target) FormatStopPayload(reason string) any {
	decision := t.StopDecision
	if decision == "" {
		decision = "block"
	}
	payload := map[string]string{"decision": decision, "reason": reason}
	if t.StopEnvelopeFormat == "hookSpecificOutput" {
		payload["hookEventName"] = "Stop"
		return map[string]any{"hookSpecificOutput": payload}
	}
	return payload
}

// Registry maps lowercase host IDs to their Target configuration.
var Registry = map[string]Target{
	"claude": {
		ID:               "claude",
		Name:             "Claude Code",
		RelativeSkillDir: filepath.Join(".claude", "skills", "breakroom"),
		PromptContext:    PromptContextText,
		Installable:      true,
	},
	"antigravity": {
		ID:               "antigravity",
		Name:             "Antigravity",
		RelativeSkillDir: filepath.Join(".agents", "plugins", "breakroom"),
		StopDecision:     "continue",
		PromptContext:    PromptContextSilent,
		Installable:      true,
	},
	"cursor": {
		ID:                     "cursor",
		Name:                   "Cursor",
		RelativeSkillDir:       filepath.Join(".cursor", "plugins", "local", "breakroom"),
		PromptContext:          PromptContextSilent,
		RequireCompletedStatus: true,
		Installable:            true,
	},
	"codex": {
		ID:               "codex",
		Name:             "Codex",
		RelativeSkillDir: filepath.Join(".agents", "plugins", "breakroom"),
		PromptContext:    PromptContextSilent,
		Installable:      true,
	},
	"vscode": {
		ID:                 "vscode",
		Name:               "VS Code (GitHub Copilot)",
		RelativeSkillDir:   filepath.Join(".vscode", "plugins", "breakroom"),
		StopEnvelopeFormat: "hookSpecificOutput",
		PromptContext:      PromptContextSilent,
		Installable:        true,
	},
	"copilot": {
		ID:               "copilot",
		Name:             "GitHub Copilot",
		RelativeSkillDir: filepath.Join(".github", "skills", "breakroom"),
		PromptContext:    PromptContextSilent,
		Installable:      false,
	},
	"muse": {
		ID:                  "muse",
		Name:                "Muse Code",
		RelativeSkillDir:    filepath.Join(".muse", "plugins", "breakroom"),
		PromptContext:       PromptContextSilent,
		SuppressGateMailAck: true,
		StaticWakeupReason:  "Breakroom: unread messages waiting (if not yet read in this turn, call wait_for_message once; after blueprint_submit or blueprint_spec, end your turn so the stop gate holds you).",
		Installable:         true,
	},
	"opencode": {
		ID:               "opencode",
		Name:             "OpenCode",
		RelativeSkillDir: filepath.Join(".opencode", "skills", "breakroom"),
		PromptContext:    PromptContextSilent,
		Installable:      true,
	},
	"generic": {
		ID:               "generic",
		Name:             "Generic",
		RelativeSkillDir: filepath.Join(".agents", "skills", "breakroom"),
		PromptContext:    PromptContextSilent,
		Installable:      true,
	},
}

var installOrder = []string{"claude", "antigravity", "cursor", "codex", "vscode", "muse", "opencode", "generic"}

// InstallTargets returns targets in canonical display order for interactive installation.
func InstallTargets() []Target {
	targets := make([]Target, 0, len(installOrder))
	for _, id := range installOrder {
		if t, ok := Registry[id]; ok {
			targets = append(targets, t)
		}
	}
	return targets
}

// Find looks up a host by ID (case-insensitive).
func Find(id string) *Target {
	lower := strings.ToLower(strings.TrimSpace(id))
	t, ok := Registry[lower]
	if !ok {
		return nil
	}
	return &t
}

// IsSupported reports whether the host is registered.
func IsSupported(id string) bool {
	return Find(id) != nil
}

var supportedOrder = []string{"claude", "antigravity", "cursor", "codex", "vscode", "copilot", "muse", "opencode", "generic"}

// InstallHostsString returns a comma-separated list of installable host IDs.
func InstallHostsString() string {
	return strings.Join(installOrder, ", ")
}

// SupportedHostsString returns a comma-separated list of all supported host IDs.
func SupportedHostsString() string {
	return strings.Join(supportedOrder, ", ")
}
