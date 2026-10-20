package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// MCP calls return within one broker hold (hosts kill long tool calls) and
// the late reply arrives via wait_for_message; the CLI, which no host
// times out, keeps re-waiting across holds until its --timeout.
func TestAwaitAcrossHolds(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e MCP test")
	}
	bin := buildBinary(t)
	home, err := os.MkdirTemp("", "br-aw-")
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
	if err := os.WriteFile(home+"/config.json", []byte(`{"wait_hold_max_sec": 1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	initParams := map[string]any{
		"protocolVersion": "2026-07-28", "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "e2e", "version": "0"},
	}
	a := startMCP(t, home, bin, ws)
	a.call("initialize", initParams)
	a.notify("notifications/initialized", map[string]any{})
	a1, _ := wantOK(t, a.tool("register_agent", map[string]any{"role": "backend", "worktree_path": ws}))["agent_id"].(string)
	b := startMCP(t, home, bin, ws)
	b.call("initialize", initParams)
	b.notify("notifications/initialized", map[string]any{})
	a2, _ := wantOK(t, b.tool("register_agent", map[string]any{"role": "frontend", "worktree_path": ws}))["agent_id"].(string)

	// MCP: one hold, then timed_out; the reply lands in the inbox.
	start := time.Now()
	data := wantOK(t, a.tool("send_message", map[string]any{
		"to_agent": a2, "content": "q?", "await_reply": true, "timeout_sec": 30,
	}))
	if el := time.Since(start); data["timed_out"] != true || el > 5*time.Second {
		t.Fatalf("mcp await = %v after %v, want timed_out within the 1s hold", data, el)
	}
	wantOK(t, b.tool("send_message", map[string]any{"to_agent": a1, "content": "late answer"}))
	data = wantOK(t, a.tool("wait_for_message", map[string]any{"timeout_sec": 30}))
	if msgs, _ := data["messages"].([]any); len(msgs) != 1 || msgs[0].(map[string]any)["content"] != "late answer" {
		t.Fatalf("wait = %v, want the late answer", data)
	}

	// CLI: parks across several 1s holds until the reply arrives.
	type result struct {
		out string
		err error
	}
	done := make(chan result, 1)
	go func() {
		cmd := exec.Command(bin, "send", "--from", a1, "--to", a2, "--message", "q2?", "--await", "--timeout", "20")
		cmd.Env = append(os.Environ(), "BREAKROOM_HOME="+home)
		cmd.Dir = ws
		out, err := cmd.CombinedOutput()
		done <- result{string(out), err}
	}()
	time.Sleep(2500 * time.Millisecond)
	wantOK(t, b.tool("send_message", map[string]any{"to_agent": a1, "content": "cli answer"}))
	select {
	case res := <-done:
		if res.err != nil || !strings.Contains(res.out, "cli answer") {
			t.Fatalf("cli await = %v %q, want the reply", res.err, res.out)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("cli await never returned")
	}
}
