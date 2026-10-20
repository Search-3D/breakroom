package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The zero-token hook path, as Claude Code runs it: no --agent (names are
// generated), cwd = the agent's project dir, and PostToolUse needs a JSON
// additionalContext envelope (plain stdout never reaches the model there).
func TestHookDelivery(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e MCP test")
	}
	bin := buildBinary(t)
	home, err := os.MkdirTemp("", "br-hk-")
	if err != nil {
		t.Fatal(err)
	}
	ws, err := os.MkdirTemp("", "br-ws-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	t.Cleanup(func() { os.RemoveAll(ws) })
	t.Cleanup(func() { runBin(t, home, bin, "stop") })
	t.Setenv("BREAKROOM_WORKSPACE", ws) // one workspace, two worktree dirs
	wtA, wtB := filepath.Join(ws, "wt-a"), filepath.Join(ws, "wt-b")
	for _, d := range []string{wtA, wtB} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	initParams := map[string]any{
		"protocolVersion": "2026-07-28", "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "e2e", "version": "0"},
	}
	a := startMCP(t, home, bin, wtA)
	a.call("initialize", initParams)
	a.notify("notifications/initialized", map[string]any{})
	a1, _ := wantOK(t, a.tool("register_agent", map[string]any{"role": "backend", "worktree_path": wtA}))["agent_id"].(string)
	b := startMCP(t, home, bin, wtB)
	b.call("initialize", initParams)
	b.notify("notifications/initialized", map[string]any{})
	wantOK(t, b.tool("register_agent", map[string]any{"role": "frontend", "worktree_path": wtB}))

	// Nothing waiting: silent, exit 0.
	if out, code := runIn(t, home, bin, wtA, "check", "--hook", "--hook-event", "PostToolUse"); code != 0 || out != "" {
		t.Fatalf("empty hook = %d %q, want silent 0", code, out)
	}
	wantOK(t, b.tool("send_message", map[string]any{"to_agent": a1, "content": "cart API ready"}))
	// From a's dir, unprompted: resolves a and emits the PostToolUse envelope.
	out, code := runIn(t, home, bin, wtA, "check", "--hook", "--hook-event", "PostToolUse")
	var env struct {
		HookSpecificOutput struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if code != 0 || json.Unmarshal([]byte(out), &env) != nil ||
		env.HookSpecificOutput.HookEventName != "PostToolUse" ||
		!strings.Contains(env.HookSpecificOutput.AdditionalContext, "cart API ready") {
		t.Fatalf("hook = %d %q, want PostToolUse envelope with the message", code, out)
	}
	// Delivered once: the next hook is silent.
	if out, code := runIn(t, home, bin, wtA, "check", "--hook", "--hook-event", "PostToolUse"); code != 0 || out != "" {
		t.Fatalf("second hook = %d %q, want silent", code, out)
	}
	// Plain hook mode (UserPromptSubmit/SessionStart pass stdout through).
	wantOK(t, b.tool("send_message", map[string]any{"to_agent": a1, "content": "second note"}))
	if out, code := runIn(t, home, bin, wtA, "check", "--hook"); code != 0 || !strings.Contains(out, "second note") {
		t.Fatalf("plain hook = %d %q", code, out)
	}
	// From the workspace root (neither agent's dir): ambiguous, silent, 0.
	if out, code := runIn(t, home, bin, ws, "check", "--hook"); code != 0 || out != "" {
		t.Fatalf("ambiguous hook = %d %q, want silent 0", code, out)
	}
}
