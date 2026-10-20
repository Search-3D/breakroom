package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// `breakroom stop` (or a daemon crash) mid-session must not strand
// connected agents: state lives in SQLite, so the proxy restarts the daemon
// and the session carries on under the same name.
func TestProxySurvivesDaemonRestart(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e MCP test")
	}
	bin := buildBinary(t)
	home, err := os.MkdirTemp("", "br-rs-")
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
	a := startMCP(t, home, bin, ws)
	a.call("initialize", map[string]any{
		"protocolVersion": "2026-07-28", "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "e2e", "version": "0"},
	})
	a.notify("notifications/initialized", map[string]any{})
	a1, _ := wantOK(t, a.tool("register_agent", map[string]any{"role": "backend", "worktree_path": ws}))["agent_id"].(string)

	if out, code := runBin(t, home, bin, "stop"); code != 0 {
		t.Fatalf("stop = %d %q", code, out)
	}
	data := wantOK(t, a.tool("list_agents", map[string]any{}))
	agents, _ := data["agents"].([]any)
	if len(agents) != 1 || agents[0].(map[string]any)["agent_id"] != a1 {
		t.Fatalf("after restart agents = %v, want [%s]", agents, a1)
	}
	if out, code := runBin(t, home, bin, "status"); code != 0 {
		t.Fatalf("daemon not back: %d %q", code, out)
	}
}

// After an upgrade the old daemon keeps running and speaks an older API.
// doctor flags it; the first MCP session swaps it when nobody is attached.
func TestStaleDaemonAfterUpgrade(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e MCP test")
	}
	build := func(ver string) string {
		t.Helper()
		bin := filepath.Join(t.TempDir(), "breakroom")
		cmd := exec.Command("go", "build", "-o", bin,
			"-ldflags", "-X github.com/search-3d/breakroom/internal/version.Version="+ver, ".")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("build %s: %v\n%s", ver, err, out)
		}
		return bin
	}
	oldBin, newBin := build("1.0.0"), build("1.1.0")
	home, err := os.MkdirTemp("", "br-up-")
	if err != nil {
		t.Fatal(err)
	}
	ws := t.TempDir()
	t.Cleanup(func() { os.RemoveAll(home) })
	t.Cleanup(func() { runBin(t, home, newBin, "stop") })
	if out, code := runBin(t, home, oldBin, "start"); code != 0 {
		t.Fatalf("old start = %d %q", code, out)
	}
	if out, _ := runBin(t, home, newBin, "doctor"); !strings.Contains(out, "STALE: daemon 1.0.0, this binary 1.1.0") {
		t.Fatalf("doctor = %q, want stale warning", out)
	}
	a := startMCP(t, home, newBin, ws)
	a.call("initialize", map[string]any{
		"protocolVersion": "2026-07-28", "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "e2e", "version": "0"},
	})
	a.notify("notifications/initialized", map[string]any{})
	wantOK(t, a.tool("register_agent", map[string]any{"role": "backend", "worktree_path": ws}))
	if out, code := runBin(t, home, newBin, "status", "--format", "json"); code != 0 || !strings.Contains(out, `"version":"1.1.0"`) {
		t.Fatalf("status = %d %q, want the new daemon", code, out)
	}
}
