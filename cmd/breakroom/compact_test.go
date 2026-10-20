package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestMCPCompact(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e compact test")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@t")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@t")
	var llmInput string
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		llmInput = string(data)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"DIGEST"}}]}`))
	}))
	defer stub.Close()

	bin := buildBinary(t)
	home, err := os.MkdirTemp("", "br-compact-home-")
	if err != nil {
		t.Fatal(err)
	}
	ws, err := os.MkdirTemp("", "br-compact-ws-")
	if err != nil {
		t.Fatal(err)
	}
	// Cleanups run LIFO: stop first while home (pidfile) still exists.
	t.Cleanup(func() { os.RemoveAll(home) })
	t.Cleanup(func() { os.RemoveAll(ws) })
	t.Cleanup(func() { runBin(t, home, bin, "stop") })

	cfg := `{"endpoint_url": "` + stub.URL + `", "model": "test"}`
	if err := os.WriteFile(home+"/config.json", []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	sh(t, ws, "init", "-b", "main", ".")
	writeGitFile(t, ws, "f.txt", "a\nb\n")
	sh(t, ws, "add", "-A")
	sh(t, ws, "commit", "-m", "init")
	writeGitFile(t, ws, "f.txt", "a\nB\n")

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

	wantOK(t, a.tool("send_message", map[string]any{"to_agent": a2, "content": "decided X"}))
	wantOK(t, b.tool("send_message", map[string]any{"to_agent": a1, "content": "doing Y"}))

	data := wantOK(t, a.tool("compact_thread", map[string]any{}))
	if data["digest"] != "DIGEST" || data["message_count"] != float64(2) {
		t.Fatalf("compact = %v", data)
	}
	if !strings.Contains(llmInput, "decided X") || !strings.Contains(llmInput, `"model":"test"`) {
		t.Fatalf("llm input = %q", llmInput)
	}

	data = wantOK(t, a.tool("get_diff", map[string]any{"agent_id": a2, "summarize": true}))
	if data["summary"] != "DIGEST" {
		t.Fatalf("summary = %v", data["summary"])
	}

	out, code := runIn(t, home, bin, ws, "compact")
	if code != 0 || !strings.Contains(out, "DIGEST") {
		t.Fatalf("cli compact = %d %q", code, out)
	}
	out, code = runIn(t, home, bin, ws, "diff", "--summarize", "--agent", a2)
	if code != 0 || !strings.Contains(out, "DIGEST") {
		t.Fatalf("cli diff --summarize with LLM = %d %q, want DIGEST", code, out)
	}
	out, code = runIn(t, home, bin, ws, "send", "--from", a1, "--to", a2,
		"--message", "review", "--diff-path", "f.txt")
	if code != 0 || !strings.HasPrefix(out, "sent message ") {
		t.Fatalf("cli send = %d %q", code, out)
	}
	out, code = runIn(t, home, bin, ws, "check", "--agent", a2, "--format", "json")
	if code != 0 || !strings.Contains(out, "diff_attachment") || !strings.Contains(out, "-b") {
		t.Fatalf("cli check = %d %q", code, out)
	}

	// No LLM: get_diff falls back and names the author to ask, except on
	// a self-diff. Config is re-read per request.
	if err := os.Remove(home + "/config.json"); err != nil {
		t.Fatal(err)
	}
	out, code = runIn(t, home, bin, ws, "diff", "--summarize", "--agent", a2)
	if code != 0 || !strings.Contains(out, "no LLM configured; printing the raw diff") || !strings.Contains(out, "B") {
		t.Fatalf("cli diff --summarize fallback = %d %q, want warning and raw diff", code, out)
	}
	data = wantOK(t, a.tool("get_diff", map[string]any{"agent_id": a2, "summarize": true}))
	if data["fallback"] != true || data["ask"] != a2 {
		t.Fatalf("fallback diff = %v, want ask=%s", data, a2)
	}
	data = wantOK(t, a.tool("get_diff", map[string]any{"agent_id": a1, "summarize": true}))
	if _, has := data["ask"]; data["fallback"] != true || has {
		t.Fatalf("self diff = %v, want fallback without ask", data)
	}
	// An offline author gets no hint (close releases before exit).
	b.close()
	data = wantOK(t, a.tool("get_diff", map[string]any{"agent_id": a2, "summarize": true}))
	if _, has := data["ask"]; data["fallback"] != true || has {
		t.Fatalf("offline author diff = %v, want fallback without ask", data)
	}

	a.close()
	b.close()
}
