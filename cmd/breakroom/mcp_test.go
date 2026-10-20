package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

// Write appends to the buffer under its lock.
func (s *safeBuffer) Write(p []byte) (n int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

// String returns the buffer's contents under its lock.
func (s *safeBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// mcpConn drives one `breakroom mcp` process over newline-delimited JSON-RPC.
type mcpConn struct {
	t      *testing.T
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	dec    *json.Decoder
	stderr *safeBuffer
	nextID int
	closed bool
}

// startMCP starts `breakroom mcp` in dir and completes the MCP handshake.
func startMCP(t *testing.T, home, bin, dir string, args ...string) *mcpConn {
	t.Helper()
	cmd := exec.Command(bin, append([]string{"mcp"}, args...)...)
	cmd.Env = append(os.Environ(), "BREAKROOM_HOME="+home)
	cmd.Dir = dir
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr safeBuffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	c := &mcpConn{t: t, cmd: cmd, stdin: stdin, dec: json.NewDecoder(stdout), stderr: &stderr}
	t.Cleanup(c.close)
	return c
}

// close ends the MCP process by closing its stdin, and waits for it to exit.
func (c *mcpConn) close() {
	if c.closed {
		return
	}
	c.closed = true
	_ = c.stdin.Close() // EOF tells the proxy to release and exit
	done := make(chan error, 1)
	go func() { done <- c.cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		_ = c.cmd.Process.Kill()
		c.t.Fatalf("proxy did not exit; stderr: %s", c.stderr.String())
	}
}

type readRes struct {
	msg map[string]any
	err error
}

// readOne reads the next JSON-RPC message, failing the test after timeout.
func (c *mcpConn) readOne(timeout time.Duration) map[string]any {
	c.t.Helper()
	resCh := make(chan readRes, 1)
	go func() {
		var msg map[string]any
		err := c.dec.Decode(&msg)
		resCh <- readRes{msg, err}
	}()
	select {
	case res := <-resCh:
		if res.err != nil {
			c.t.Fatalf("read: %v; stderr: %s", res.err, c.stderr.String())
		}
		return res.msg
	case <-time.After(timeout):
		c.t.Fatalf("timed out waiting for response; stderr: %s", c.stderr.String())
		return nil
	}
}

// call sends a request and reads until the matching response id.
func (c *mcpConn) call(method string, params any) map[string]any {
	c.t.Helper()
	c.nextID++
	req := map[string]any{"jsonrpc": "2.0", "id": c.nextID, "method": method}
	if params != nil {
		req["params"] = params
	}
	data, err := json.Marshal(req)
	if err != nil {
		c.t.Fatal(err)
	}
	data = append(data, '\n')
	if _, err := c.stdin.Write(data); err != nil {
		c.t.Fatalf("write: %v; stderr: %s", err, c.stderr.String())
	}
	for {
		msg := c.readOne(30 * time.Second)
		id, _ := msg["id"].(float64)
		if int(id) != c.nextID {
			continue // server notification; keep waiting
		}
		if msg["error"] != nil {
			c.t.Fatalf("%s: protocol error %v", method, msg["error"])
		}
		return msg
	}
}

// notify sends a JSON-RPC notification (no reply expected).
func (c *mcpConn) notify(method string, params any) {
	c.t.Helper()
	req := map[string]any{"jsonrpc": "2.0", "method": method, "params": params}
	data, err := json.Marshal(req)
	if err != nil {
		c.t.Fatal(err)
	}
	data = append(data, '\n')
	if _, err := c.stdin.Write(data); err != nil {
		c.t.Fatalf("notify: %v; stderr: %s", err, c.stderr.String())
	}
}

// tool calls an MCP tool and returns the breakroom envelope.
func (c *mcpConn) tool(name string, args map[string]any) map[string]any {
	c.t.Helper()
	msg := c.call("tools/call", map[string]any{"name": name, "arguments": args})
	result, _ := msg["result"].(map[string]any)
	if result == nil {
		c.t.Fatalf("%s: no result in %v", name, msg)
	}
	if text := firstText(result); text != "" {
		var env map[string]any
		if err := json.Unmarshal([]byte(text), &env); err != nil {
			c.t.Fatalf("%s: bad envelope %q: %v", name, text, err)
		}
		return env
	}
	if sc, ok := result["structuredContent"].(map[string]any); ok {
		return sc
	}
	c.t.Fatalf("%s: no content in %v", name, result)
	return nil
}

// firstText returns the text of a tool result's first content item.
func firstText(result map[string]any) string {
	content, _ := result["content"].([]any)
	if len(content) == 0 {
		return ""
	}
	item, _ := content[0].(map[string]any)
	text, _ := item["text"].(string)
	return text
}

// wantOK fails the test unless env succeeded, and returns its data.
func wantOK(t *testing.T, env map[string]any) map[string]any {
	t.Helper()
	if env["ok"] != true {
		t.Fatalf("envelope not ok: %v", env)
	}
	data, _ := env["data"].(map[string]any)
	if data == nil {
		t.Fatalf("no data in %v", env)
	}
	return data
}

func TestMCPChat(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e MCP test")
	}
	bin := buildBinary(t)
	home, err := os.MkdirTemp("", "br-mcp-")
	if err != nil {
		t.Fatal(err)
	}
	ws, err := os.MkdirTemp("", "br-ws-")
	if err != nil {
		t.Fatal(err)
	}
	// Cleanups run LIFO: stop first while home (pidfile) still exists.
	t.Cleanup(func() { os.RemoveAll(home) })
	t.Cleanup(func() { os.RemoveAll(ws) })
	t.Cleanup(func() { runBin(t, home, bin, "stop") })

	initParams := map[string]any{
		"protocolVersion": "2026-07-28", "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "e2e", "version": "0"},
	}
	a := startMCP(t, home, bin, ws)
	res := a.call("initialize", initParams)
	if info, _ := res["result"].(map[string]any)["serverInfo"].(map[string]any); info["name"] != "breakroom" {
		t.Fatalf("serverInfo = %v", info)
	}
	a.notify("notifications/initialized", map[string]any{})
	data := wantOK(t, a.tool("register_agent", map[string]any{"role": "backend", "worktree_path": ws}))
	a1, _ := data["agent_id"].(string)
	if a1 == "" {
		t.Fatalf("a registered as %v", data)
	}

	b := startMCP(t, home, bin, ws)
	b.call("initialize", initParams)
	b.notify("notifications/initialized", map[string]any{})
	data = wantOK(t, b.tool("register_agent", map[string]any{"role": "frontend", "worktree_path": ws}))
	a2, _ := data["agent_id"].(string)
	if a2 == "" {
		t.Fatalf("b registered as %v", data)
	}

	// CLI send + check against the same daemon.
	out, code := runIn(t, home, bin, ws, "send", "--from", a1, "--to", a2, "--message", "via-cli")
	if code != 0 || !strings.HasPrefix(out, "sent message ") {
		t.Fatalf("cli send = %d %q", code, out)
	}
	out, code = runIn(t, home, bin, ws, "check", "--agent", a2)
	if code != 0 || !strings.Contains(out, "via-cli") || !strings.Contains(out, "["+a1+" -> "+a2+"]") {
		t.Fatalf("cli check = %d %q", code, out)
	}
	// Hook mode never fails: ambiguous identity stays silent with exit 0.
	out, code = runIn(t, home, bin, ws, "check", "--hook")
	if code != 0 || out != "" {
		t.Fatalf("hook check = %d %q, want silent 0", code, out)
	}

	// MCP send + blocking wait across proxies.
	data = wantOK(t, a.tool("send_message", map[string]any{"to_agent": a2, "content": "hello"}))
	if ids, _ := data["message_ids"].([]any); len(ids) != 1 {
		t.Fatalf("send data = %v", data)
	}
	data = wantOK(t, b.tool("wait_for_message", map[string]any{"timeout_sec": 5}))
	msgs, _ := data["messages"].([]any)
	if len(msgs) != 1 || data["timed_out"] != false {
		t.Fatalf("wait data = %v", data)
	}
	if m := msgs[0].(map[string]any); m["content"] != "hello" || m["from"] != a1 {
		t.Fatalf("message = %v", m)
	}

	// Piggyback: mail sent after the wait rides the next tool response.
	wantOK(t, a.tool("send_message", map[string]any{"to_agent": a2, "content": "nudge"}))
	env := b.tool("list_agents", map[string]any{})
	data, _ = env["data"].(map[string]any)
	if agents, _ := data["agents"].([]any); len(agents) != 2 {
		t.Fatalf("agents = %v", data)
	}
	unread, _ := env["_unread_messages"].([]any)
	if len(unread) != 1 || unread[0].(map[string]any)["content"] != "nudge" {
		t.Fatalf("piggyback = %v", env["_unread_messages"])
	}

	// Closing stdin releases the lease; presence drops accordingly.
	a.close()
	out, code = runBin(t, home, bin, "status")
	if code != 0 || !strings.Contains(out, "agents present: 1") {
		t.Fatalf("status after a close = %d %q", code, out)
	}
	b.close()
	out, code = runBin(t, home, bin, "status")
	if code != 0 || !strings.Contains(out, "agents present: 0") {
		t.Fatalf("status after b close = %d %q", code, out)
	}
}

// sh runs a git command in dir, failing the test on error.
func sh(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// writeGitFile writes a file in dir.
func writeGitFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(dir+"/"+name, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestMCPGit(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e git test")
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
	bin := buildBinary(t)
	home, err := os.MkdirTemp("", "br-githome-")
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp("", "br-gitws-")
	if err != nil {
		t.Fatal(err)
	}
	// Cleanups run LIFO: stop first while home (pidfile) still exists.
	t.Cleanup(func() { os.RemoveAll(home) })
	t.Cleanup(func() { os.RemoveAll(root) })
	t.Cleanup(func() { runBin(t, home, bin, "stop") })
	sh(t, root, "init", "-b", "main", ".")
	writeGitFile(t, root, "f.txt", "a\nb\nc\nd\ne\nf\n")
	sh(t, root, "add", "-A")
	sh(t, root, "commit", "-m", "init")
	wtParent, err := os.MkdirTemp("", "br-gitwt-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(wtParent) })
	wt := wtParent + "/wt"
	sh(t, root, "worktree", "add", wt, "-b", "side")

	initParams := map[string]any{
		"protocolVersion": "2026-07-28", "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "e2e", "version": "0"},
	}
	a := startMCP(t, home, bin, root)
	a.call("initialize", initParams)
	a.notify("notifications/initialized", map[string]any{})
	data := wantOK(t, a.tool("register_agent", map[string]any{"role": "backend", "worktree_path": root}))
	a1, _ := data["agent_id"].(string)
	if a1 == "" {
		t.Fatalf("a registered as %v", data)
	}
	// Sibling worktree joins the SAME workspace via the shared git root.
	// Started like Cursor does (cwd outside the project) with --dir, and
	// "." must still mean the worktree.
	b := startMCP(t, home, bin, home, "--dir", wt)
	b.call("initialize", initParams)
	b.notify("notifications/initialized", map[string]any{})
	data = wantOK(t, b.tool("register_agent", map[string]any{"role": "frontend", "worktree_path": "."}))
	a2, _ := data["agent_id"].(string)
	if a2 == "" {
		t.Fatalf("b registered as %v (want a second agent in the shared workspace)", data)
	}

	writeGitFile(t, root, "f.txt", "a\nB-OURS\nc\nd\ne\nf\n")
	writeGitFile(t, wt, "f.txt", "a\nB-THEIRS\nc\nd\ne\nf\n")

	// update_plan over MCP: overlap warnings ride the envelope's _warnings.
	wantOK(t, b.tool("update_plan", map[string]any{"status": "active", "current_step": "theirs"}))
	env := a.tool("update_plan", map[string]any{"status": "active", "current_step": "ours"})
	data = wantOK(t, env)
	if agent, _ := data["agent"].(map[string]any); agent["current_step"] != "ours" {
		t.Fatalf("update_plan data = %v", data)
	}
	warns, _ := env["_warnings"].([]any)
	if len(warns) != 1 || warns[0].(map[string]any)["code"] != "file_overlap" ||
		fmt.Sprint(warns[0].(map[string]any)["files"]) != "[f.txt]" {
		t.Fatalf("_warnings = %v, want file_overlap [f.txt]", env["_warnings"])
	}

	data = wantOK(t, a.tool("get_diff", map[string]any{"agent_id": a2}))
	if diff, _ := data["diff"].(string); !strings.Contains(diff, "B-THEIRS") {
		t.Fatalf("diff = %q", diff)
	}
	if files, _ := data["files"].([]any); len(files) != 1 {
		t.Fatalf("files = %v", files)
	}
	data = wantOK(t, a.tool("check_conflicts", map[string]any{"target_agent_id": a2}))
	conflicts, _ := data["conflicts"].([]any)
	if len(conflicts) != 1 {
		t.Fatalf("conflicts = %v", conflicts)
	}
	if c := conflicts[0].(map[string]any); c["path"] != "f.txt" || c["ours_lines"] != "2" || c["theirs_lines"] != "2" {
		t.Fatalf("conflict = %v", c)
	}

	out, code := runIn(t, home, bin, root, "diff", "--agent", a2)
	if code != 0 || !strings.Contains(out, "B-THEIRS") {
		t.Fatalf("cli diff = %d %q", code, out)
	}
	if out, code := runIn(t, home, bin, wt, "diff"); code == 0 {
		t.Fatalf("cli diff without --agent = 0 (%q), want ambiguous error", out)
	}

	a.close()
	b.close()
}

func TestMCPUnexpandedTemplateDir(t *testing.T) {
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@t")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@t")
	bin := buildBinary(t)
	home, err := os.MkdirTemp("", "br-tmplhome-")
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp("", "br-tmplws-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	t.Cleanup(func() { os.RemoveAll(root) })
	t.Cleanup(func() { runBin(t, home, bin, "stop") })
	sh(t, root, "init", "-b", "main", ".")
	writeGitFile(t, root, "f.txt", "hello")
	sh(t, root, "add", "-A")
	sh(t, root, "commit", "-m", "init")

	initParams := map[string]any{
		"protocolVersion": "2026-07-28", "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "e2e", "version": "0"},
	}

	// Started like Cursor when a template variable isn't expanded by the plugin host.
	c := startMCP(t, home, bin, home, "--dir", "${workspaceFolder}")
	defer c.close()
	c.call("initialize", initParams)
	c.notify("notifications/initialized", map[string]any{})

	// Should successfully register using an absolute worktree path in the git repo.
	data := wantOK(t, c.tool("register_agent", map[string]any{"role": "solo", "worktree_path": root}))
	id, _ := data["agent_id"].(string)
	if id == "" {
		t.Fatalf("agent registration failed: %v", data)
	}
}
