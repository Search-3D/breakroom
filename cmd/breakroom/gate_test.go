package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestExtractTranscriptPrompt covers the Antigravity transcript fallback:
// the last USER_INPUT wins, and an unreadable tail fails open with "".
func TestExtractTranscriptPrompt(t *testing.T) {
	dir := t.TempDir()
	ok := filepath.Join(dir, "ok.jsonl")
	data := "{\"type\":\"USER_INPUT\",\"content\":\"<USER_REQUEST>\\nfirst\\n</USER_REQUEST>\"}\n" +
		"{\"type\":\"OTHER\",\"content\":\"x\"}\n" +
		"{\"type\":\"USER_INPUT\",\"content\":\"<USER_REQUEST>\\nsecond\\n</USER_REQUEST>\\n<ADDITIONAL_METADATA>\\ny\\n</ADDITIONAL_METADATA>\"}\n"
	if err := os.WriteFile(ok, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := extractTranscriptPrompt(ok); got != "second" {
		t.Fatalf("extract = %q, want %q", got, "second")
	}
	// Scan error (line over the 1MB buffer): fail open, no stale prompt.
	bad := filepath.Join(dir, "bad.jsonl")
	huge := strings.Repeat("a", 2*1024*1024)
	if err := os.WriteFile(bad, []byte("{\"type\":\"USER_INPUT\",\"content\":\"stale\"}\n"+huge+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := extractTranscriptPrompt(bad); got != "" {
		t.Fatalf("extract with scan error = %q, want %q", got, "")
	}
	if got := extractTranscriptPrompt(filepath.Join(dir, "missing.jsonl")); got != "" {
		t.Fatalf("extract missing = %q, want %q", got, "")
	}
}

func TestCleanTranscriptPrompt(t *testing.T) {
	for _, tc := range []struct {
		name, input, want string
	}{
		{"empty", "", ""},
		{"whitespace", "   \n\t  ", ""},
		{"plain", "hello world", "hello world"},
		{"terminated", "<USER_REQUEST>\nhello world\n</USER_REQUEST>", "hello world"},
		{"terminated with metadata", "<USER_REQUEST>\nhello world\n</USER_REQUEST>\n<ADDITIONAL_METADATA>\ntime=now\n</ADDITIONAL_METADATA>", "hello world"},
		{"unterminated", "<USER_REQUEST>\nhello world", "hello world"},
		{"unterminated with metadata", "<USER_REQUEST>\nhello world\n<ADDITIONAL_METADATA>\ntime=now\n</ADDITIONAL_METADATA>", "hello world"},
		{"metadata only", "hello world\n<ADDITIONAL_METADATA>\ntime=now\n</ADDITIONAL_METADATA>", "hello world"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := cleanTranscriptPrompt(tc.input); got != tc.want {
				t.Fatalf("cleanTranscriptPrompt(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestGateHostCaseInsensitive(t *testing.T) {
	for _, tc := range []struct {
		host string
		want string
	}{
		{"Cursor", "{\"continue\":true}\n"},
		{"cursor", "{\"continue\":true}\n"},
		{"Antigravity", "{}\n"},
		{"antigravity", "{}\n"},
	} {
		t.Run(tc.host, func(t *testing.T) {
			oldStdout := os.Stdout
			r, w, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			os.Stdout = w

			code := gateCommand(context.Background(), []string{"--event", "prompt", "--host", tc.host})

			_ = w.Close()
			os.Stdout = oldStdout

			var buf bytes.Buffer
			_, _ = io.Copy(&buf, r)
			_ = r.Close()

			if code != 0 {
				t.Fatalf("gateCommand exit code = %d, want 0", code)
			}
			if buf.String() != tc.want {
				t.Fatalf("gateCommand output = %q, want %q", buf.String(), tc.want)
			}
		})
	}
}

func TestBlueprintLandInvalidMethod(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want int
	}{
		{"missing method", []string{"land", "bp"}, 2},
		{"invalid method", []string{"land", "bp", "--method", "invalid"}, 2},
		{"invalid command", []string{"invalid", "bp"}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			oldStderr := os.Stderr
			r, w, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			os.Stderr = w

			code := blueprintCommand(context.Background(), tc.args)

			_ = w.Close()
			os.Stderr = oldStderr
			_ = r.Close()

			if code != tc.want {
				t.Fatalf("blueprintCommand exit code = %d, want %d", code, tc.want)
			}
		})
	}
}

func TestGateInvalidEventOrHost(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want int
	}{
		{"invalid event", []string{"--event", "invalid", "--host", "claude"}, 2},
		{"invalid host", []string{"--event", "prompt", "--host", "invalid"}, 2},
		{"missing event", []string{"--host", "claude"}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			oldStderr := os.Stderr
			r, w, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			os.Stderr = w

			code := gateCommand(context.Background(), tc.args)

			_ = w.Close()
			os.Stderr = oldStderr
			_ = r.Close()

			if code != tc.want {
				t.Fatalf("gateCommand exit code = %d, want %d", code, tc.want)
			}
		})
	}
}

// runStdin runs the binary in dir with stdin, returning stdout only (hooks
// must keep stdout clean for the host's JSON parser) and the exit code.
func runStdin(t *testing.T, home, bin, dir, stdin string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), "BREAKROOM_HOME="+home)
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.Output()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return string(out), exitErr.ExitCode()
	}
	if err != nil {
		t.Fatalf("run %v: %v", args, err)
	}
	return string(out), 0
}

type gateEnv struct {
	bin, home, ws, wtA, wtB string
	a, b                    *mcpConn
	arch, build             string
}

// twoAgents starts an architect in wt-a and a builder in wt-b of one
// workspace, both registered over MCP.
func twoAgents(t *testing.T) *gateEnv {
	t.Helper()
	if testing.Short() {
		t.Skip("e2e gate test")
	}
	e := &gateEnv{bin: buildBinary(t)}
	var err error
	if e.home, err = os.MkdirTemp("", "br-gt-"); err != nil {
		t.Fatal(err)
	}
	if e.ws, err = os.MkdirTemp("", "br-gtws-"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(e.home) })
	t.Cleanup(func() { os.RemoveAll(e.ws) })
	t.Cleanup(func() { runBin(t, e.home, e.bin, "stop") })
	t.Setenv("BREAKROOM_WORKSPACE", e.ws)
	e.wtA, e.wtB = filepath.Join(e.ws, "wt-a"), filepath.Join(e.ws, "wt-b")
	for _, d := range []string{e.wtA, e.wtB} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	initParams := map[string]any{
		"protocolVersion": "2026-07-28", "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "e2e", "version": "0"},
	}
	e.a = startMCP(t, e.home, e.bin, e.wtA)
	e.a.call("initialize", initParams)
	e.a.notify("notifications/initialized", map[string]any{})
	e.arch, _ = wantOK(t, e.a.tool("register_agent", map[string]any{"role": "architect", "worktree_path": e.wtA}))["agent_id"].(string)
	e.b = startMCP(t, e.home, e.bin, e.wtB)
	e.b.call("initialize", initParams)
	e.b.notify("notifications/initialized", map[string]any{})
	e.build, _ = wantOK(t, e.b.tool("register_agent", map[string]any{"role": "builder", "worktree_path": e.wtB}))["agent_id"].(string)
	return e
}

func TestGateFailsOpen(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e gate test")
	}
	bin := buildBinary(t)
	home, err := os.MkdirTemp("", "br-gfo-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	t.Cleanup(func() { runBin(t, home, bin, "stop") })
	dir := t.TempDir()
	for _, stdin := range []string{"{}", "not json"} {
		if out, code := runStdin(t, home, bin, dir, stdin, "gate", "--event", "stop", "--host", "claude"); code != 0 || out != "" {
			t.Fatalf("no daemon, stdin %q = %d %q, want silent 0", stdin, code, out)
		}
	}
	if _, code := runStdin(t, home, bin, dir, "{}", "gate", "--event", "stop", "--host", "nope"); code != 2 {
		t.Fatalf("unknown host exit = %d, want 2", code)
	}
	if out, code := runStdin(t, home, bin, dir, "{}", "gate", "--event", "prompt", "--host", "Cursor"); code != 0 || out != "{\"continue\":true}\n" {
		t.Fatalf("capitalized cursor exit = %d, out = %q, want continue:true", code, out)
	}
	if out, code := runStdin(t, home, bin, dir, "{}", "gate", "--event", "prompt", "--host", "Antigravity"); code != 0 || out != "{}\n" {
		t.Fatalf("capitalized antigravity exit = %d, out = %q, want {}", code, out)
	}
	if out, code := runStdin(t, home, bin, dir, "{}", "gate", "--event", "stop", "--host", "Antigravity"); code != 0 || strings.TrimSpace(out) != `{"decision":"stop"}` {
		t.Fatalf("capitalized antigravity stop exit = %d, out = %q, want decision:stop", code, out)
	}
	if _, code := runStdin(t, home, bin, dir, "{}", "gate", "--event", "invalid", "--host", "claude"); code != 2 {
		t.Fatalf("invalid event exit = %d, want 2", code)
	}

	e := twoAgents(t)
	// Two agents, and the hook runs where neither worktree claims it.
	if out, code := runStdin(t, e.home, e.bin, e.ws, "{}", "gate", "--event", "stop", "--host", "claude"); code != 0 || out != "" {
		t.Fatalf("ambiguous = %d %q, want silent 0", code, out)
	}
	if out, code := runStdin(t, e.home, e.bin, e.ws, `{"prompt":"hi"}`, "gate", "--event", "prompt", "--host", "claude"); code != 0 || out != "" {
		t.Fatalf("ambiguous prompt = %d %q, want silent 0", code, out)
	}
}

func TestGateStopClaudeE2E(t *testing.T) {
	e := twoAgents(t)
	wantOK(t, e.b.tool("send_message", map[string]any{"to_agent": e.arch, "content": "cart API ready"}))
	out, code := runStdin(t, e.home, e.bin, e.wtA, `{"stop_hook_active":false}`, "gate", "--event", "stop", "--host", "claude")
	var got struct {
		Decision, Reason string
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil || code != 0 {
		t.Fatalf("gate = %d %q: %v", code, out, err)
	}
	if got.Decision != "block" || !strings.Contains(got.Reason, "cart API ready") {
		t.Fatalf("gate = %+v", got)
	}
	if out, _ := runIn(t, e.home, e.bin, e.wtA, "check", "--format", "json"); strings.Contains(out, "cart API ready") {
		t.Fatalf("mail not acked after delivery: %s", out)
	}
}

func TestGateVSCodeFormat(t *testing.T) {
	e := twoAgents(t)
	wantOK(t, e.b.tool("send_message", map[string]any{"to_agent": e.arch, "content": "hi"}))
	out, _ := runStdin(t, e.home, e.bin, e.wtA, `{}`, "gate", "--event", "stop", "--host", "vscode")
	var got struct {
		HookSpecificOutput struct {
			HookEventName, Decision, Reason string
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("gate = %q: %v", out, err)
	}
	if h := got.HookSpecificOutput; h.HookEventName != "Stop" || h.Decision != "block" || !strings.Contains(h.Reason, "hi") {
		t.Fatalf("vscode gate = %+v", got)
	}
}

// Muse places both prompt-hook and Stop-hook reasons in top-of-prompt
// developer context blocks: the prompt hook only records, and the Stop gate
// blocks with a constant wakeup string while leaving mail unread for the
// next Breakroom tool call.
func TestGateMuseHost(t *testing.T) {
	e := twoAgents(t)
	wantOK(t, e.b.tool("send_message", map[string]any{"to_agent": e.arch, "content": "hi"}))
	stdin, _ := json.Marshal(map[string]any{"prompt": "go on", "cwd": e.wtA})
	if out, code := runStdin(t, e.home, e.bin, e.ws, string(stdin), "gate", "--event", "prompt", "--host", "muse"); code != 0 || out != "" {
		t.Fatalf("muse prompt hook = %d %q, want silent 0", code, out)
	}
	out, _ := runStdin(t, e.home, e.bin, e.wtA, `{}`, "gate", "--event", "stop", "--host", "muse")
	var got struct{ Decision, Reason string }
	if err := json.Unmarshal([]byte(out), &got); err != nil || got.Decision != "block" || !strings.Contains(got.Reason, "unread messages waiting") {
		t.Fatalf("muse stop gate = %q (%v), want constant wakeup block", out, err)
	}
	if out, _ := runIn(t, e.home, e.bin, e.wtA, "check", "--format", "json"); !strings.Contains(out, "hi") {
		t.Fatalf("muse stop gate consumed mail instead of leaving it for the next tool call: %s", out)
	}
}

// Cursor accepts the flat Claude-format stop block as an automatic
// follow-up, but its prompt hooks can't display mail (record-only) and its
// stop hook also fires for aborted and errored turns (skipped).
func TestGateCursorHost(t *testing.T) {
	e := twoAgents(t)
	wantOK(t, e.b.tool("send_message", map[string]any{"to_agent": e.arch, "content": "hi-prompt"}))
	stdin, _ := json.Marshal(map[string]any{"prompt": "go on", "workspace_roots": []string{e.wtA}})
	if out, code := runStdin(t, e.home, e.bin, e.ws, string(stdin), "gate", "--event", "prompt", "--host", "cursor"); code != 0 || out != "{\"continue\":true}\n" {
		t.Fatalf("cursor prompt hook = %d %q, want continue:true", code, out)
	}
	if out, _ := runIn(t, e.home, e.bin, e.wtA, "check", "--format", "json"); !strings.Contains(out, "hi-prompt") {
		t.Fatalf("mail acked by record-only prompt hook: %s", out)
	}
	for _, status := range []string{"aborted", "error"} {
		wantOK(t, e.b.tool("send_message", map[string]any{"to_agent": e.arch, "content": "hi-" + status}))
		stdin, _ := json.Marshal(map[string]any{"status": status, "workspace_roots": []string{e.wtA}})
		if out, code := runStdin(t, e.home, e.bin, e.ws, string(stdin), "gate", "--event", "stop", "--host", "cursor"); code != 0 || out != "" {
			t.Fatalf("cursor %s stop = %d %q, want silent 0", status, code, out)
		}
		if out, _ := runIn(t, e.home, e.bin, e.wtA, "check", "--format", "json"); !strings.Contains(out, "hi-"+status) {
			t.Fatalf("mail acked by skipped %s stop: %s", status, out)
		}
	}
	wantOK(t, e.b.tool("send_message", map[string]any{"to_agent": e.arch, "content": "hi-stop"}))
	stopStdin, _ := json.Marshal(map[string]any{"status": "completed", "workspace_roots": []string{e.wtA}})
	out, _ := runStdin(t, e.home, e.bin, e.ws, string(stopStdin), "gate", "--event", "stop", "--host", "cursor")
	var got struct{ Decision, Reason string }
	if err := json.Unmarshal([]byte(out), &got); err != nil || got.Decision != "block" || !strings.Contains(got.Reason, "hi-stop") {
		t.Fatalf("cursor stop gate = %q (%v), want a flat block with the mail", out, err)
	}
}

// Copilot drops userPromptSubmitted hook output, so the prompt hook only
// records; agentStop takes the flat Claude-format block.
func TestGateCopilotHost(t *testing.T) {
	e := twoAgents(t)
	wantOK(t, e.b.tool("send_message", map[string]any{"to_agent": e.arch, "content": "hi"}))
	stdin, _ := json.Marshal(map[string]any{"prompt": "go on", "cwd": e.wtA})
	if out, code := runStdin(t, e.home, e.bin, e.ws, string(stdin), "gate", "--event", "prompt", "--host", "copilot"); code != 0 || out != "" {
		t.Fatalf("copilot prompt hook = %d %q, want silent 0", code, out)
	}
	out, _ := runStdin(t, e.home, e.bin, e.wtA, `{"stop_hook_active":false}`, "gate", "--event", "stop", "--host", "copilot")
	var got struct{ Decision, Reason string }
	if err := json.Unmarshal([]byte(out), &got); err != nil || got.Decision != "block" || !strings.Contains(got.Reason, "hi") {
		t.Fatalf("copilot stop gate = %q (%v), want a flat block with the mail", out, err)
	}
}

// Codex prompt hooks do not expose a model-visible context channel, so the
// prompt hook only records; stop gate blocks with the flat reason and mail.
func TestGateCodexHost(t *testing.T) {
	e := twoAgents(t)
	wantOK(t, e.b.tool("send_message", map[string]any{"to_agent": e.arch, "content": "hi"}))
	stdin, _ := json.Marshal(map[string]any{"prompt": "go on", "cwd": e.wtA})
	if out, code := runStdin(t, e.home, e.bin, e.ws, string(stdin), "gate", "--event", "prompt", "--host", "codex"); code != 0 || out != "" {
		t.Fatalf("codex prompt hook = %d %q, want silent 0", code, out)
	}
	out, _ := runStdin(t, e.home, e.bin, e.wtA, `{"stop_hook_active":false}`, "gate", "--event", "stop", "--host", "codex")
	var got struct{ Decision, Reason string }
	if err := json.Unmarshal([]byte(out), &got); err != nil || got.Decision != "block" || !strings.Contains(got.Reason, "hi") {
		t.Fatalf("codex stop gate = %q (%v), want a flat block with the mail", out, err)
	}
}

func TestChatApprovalE2E(t *testing.T) {
	e := twoAgents(t)
	wantOK(t, e.a.tool("blueprint_init", map[string]any{"name": "bp", "builder": e.build}))
	wantOK(t, e.a.tool("blueprint_roadmap", map[string]any{"name": "bp",
		"phases": []any{map[string]any{"phase": 1, "title": "t"}}}))
	wantFail(t, e.a.tool("blueprint_approve", map[string]any{"name": "bp", "version": 1, "quote": "approved"}), "human_approval_required")
	stdin, _ := json.Marshal(map[string]any{"prompt": "roadmap approved", "cwd": e.wtA})
	if out, code := runStdin(t, e.home, e.bin, e.ws, string(stdin), "gate", "--event", "prompt", "--host", "claude"); code != 0 {
		t.Fatalf("prompt hook = %d %q", code, out)
	}
	wantOK(t, e.a.tool("blueprint_approve", map[string]any{"name": "bp", "version": 1, "quote": "approved"}))
	wantOK(t, e.a.tool("blueprint_spec", map[string]any{
		"name": "bp", "phase": 1, "title": "t", "objective": "o",
		"requirements": []any{map[string]any{"id": "r1", "text": "req"}},
		"acceptance":   []any{map[string]any{"id": "a1", "text": "acc"}},
	}))
}

func TestChatApprovalAntigravityE2E(t *testing.T) {
	e := twoAgents(t)
	wantOK(t, e.a.tool("blueprint_init", map[string]any{"name": "bp", "builder": e.build}))
	wantOK(t, e.a.tool("blueprint_roadmap", map[string]any{"name": "bp",
		"phases": []any{map[string]any{"phase": 1, "title": "t"}}}))
	wantFail(t, e.a.tool("blueprint_approve", map[string]any{"name": "bp", "version": 1, "quote": "approved"}), "human_approval_required")

	// Create a mock transcript file with USER_INPUT step.
	transcriptPath := filepath.Join(t.TempDir(), "transcript.jsonl")
	transcriptData := `{"step_index":0,"type":"USER_INPUT","content":"<USER_REQUEST>\nconnect\n</USER_REQUEST>"}
{"step_index":1,"type":"PLANNER_RESPONSE","content":"ok"}
{"step_index":2,"type":"USER_INPUT","content":"<USER_REQUEST>\nroadmap approved\n</USER_REQUEST>\n<ADDITIONAL_METADATA>\ntime=now\n</ADDITIONAL_METADATA>"}
`
	if err := os.WriteFile(transcriptPath, []byte(transcriptData), 0o644); err != nil {
		t.Fatal(err)
	}

	stdin, _ := json.Marshal(map[string]any{
		"conversationId": "c1",
		"workspacePaths": []string{e.wtA},
		"transcriptPath": transcriptPath,
	})
	if out, code := runStdin(t, e.home, e.bin, e.ws, string(stdin), "gate", "--event", "prompt", "--host", "antigravity"); code != 0 || strings.TrimSpace(out) != "{}" {
		t.Fatalf("antigravity prompt hook = code %d out %q, want code 0 and output {}", code, out)
	}
	wantOK(t, e.a.tool("blueprint_approve", map[string]any{"name": "bp", "version": 1, "quote": "approved"}))
}

func TestChatApprovalCursorE2E(t *testing.T) {
	e := twoAgents(t)
	wantOK(t, e.a.tool("blueprint_init", map[string]any{"name": "bp", "builder": e.build}))
	wantOK(t, e.a.tool("blueprint_roadmap", map[string]any{"name": "bp",
		"phases": []any{map[string]any{"phase": 1, "title": "t"}}}))
	wantFail(t, e.a.tool("blueprint_approve", map[string]any{"name": "bp", "version": 1, "quote": "approved"}), "human_approval_required")

	// Cursor passes workspace_roots (no cwd) and expects {"continue":true}
	stdin, _ := json.Marshal(map[string]any{
		"prompt":          "roadmap approved",
		"workspace_roots": []string{e.wtA},
		"conversation_id": "cursor-chat-1",
	})
	if out, code := runStdin(t, e.home, e.bin, e.ws, string(stdin), "gate", "--event", "prompt", "--host", "cursor"); code != 0 || strings.TrimSpace(out) != `{"continue":true}` {
		t.Fatalf("cursor prompt hook = %d %q, want continue:true", code, out)
	}
	wantOK(t, e.a.tool("blueprint_approve", map[string]any{"name": "bp", "version": 1, "quote": "approved"}))
	wantOK(t, e.a.tool("blueprint_spec", map[string]any{
		"name": "bp", "phase": 1, "title": "t", "objective": "o",
		"requirements": []any{map[string]any{"id": "r1", "text": "req"}},
		"acceptance":   []any{map[string]any{"id": "a1", "text": "acc"}},
	}))
}

func TestGateAntigravityReminders(t *testing.T) {
	e := twoAgents(t)
	wantOK(t, e.a.tool("blueprint_init", map[string]any{"name": "bp", "builder": e.build}))
	wantOK(t, e.a.tool("blueprint_roadmap", map[string]any{"name": "bp",
		"phases": []any{map[string]any{"phase": 1, "title": "t"}}}))
	stdin, _ := json.Marshal(map[string]any{"prompt": "approved", "cwd": e.wtA})
	runStdin(t, e.home, e.bin, e.ws, string(stdin), "gate", "--event", "prompt", "--host", "claude")
	wantOK(t, e.a.tool("blueprint_approve", map[string]any{"name": "bp", "version": 1, "quote": "approved"}))
	wantOK(t, e.a.tool("blueprint_spec", map[string]any{
		"name": "bp", "phase": 1, "title": "t", "objective": "o",
		"requirements": []any{map[string]any{"id": "r1", "text": "req"}},
		"acceptance":   []any{map[string]any{"id": "a1", "text": "acc"}},
	}))
	// Drain mail for builder
	runIn(t, e.home, e.bin, e.ws, "check", "--agent", e.build)

	// Builder tries to stop on executionNum: 1 while phase is open: should get reminder
	stdin1, _ := json.Marshal(map[string]any{
		"conversationId": "c1",
		"workspacePaths": []string{e.wtB},
		"executionNum":   1,
	})
	out, code := runStdin(t, e.home, e.bin, e.ws, string(stdin1), "gate", "--event", "stop", "--host", "antigravity")
	if code != 0 {
		t.Fatalf("antigravity stop gate = %d %q", code, out)
	}
	var got struct{ Decision, Reason string }
	if err := json.Unmarshal([]byte(out), &got); err != nil || got.Decision != "continue" || !strings.Contains(got.Reason, "phase 1 is still open") {
		t.Fatalf("antigravity stop gate = %q (%v), want continue with reminder", out, err)
	}

	// Builder tries to stop on executionNum: 2: should allow stop
	stdin2, _ := json.Marshal(map[string]any{
		"conversationId": "c1",
		"workspacePaths": []string{e.wtB},
		"executionNum":   2,
	})
	out2, code2 := runStdin(t, e.home, e.bin, e.ws, string(stdin2), "gate", "--event", "stop", "--host", "antigravity")
	if code2 != 0 || strings.TrimSpace(out2) != `{"decision":"stop"}` {
		t.Fatalf("antigravity stop gate executionNum 2 = %d %q, want allow decision stop", code2, out2)
	}
}

func TestGateAntigravityMailOnPrompt(t *testing.T) {
	e := twoAgents(t)
	wantOK(t, e.b.tool("send_message", map[string]any{"to_agent": e.arch, "content": "hello architect"}))

	transcriptPath := filepath.Join(t.TempDir(), "transcript.jsonl")
	transcriptData := `{"step_index":0,"type":"USER_INPUT","content":"<USER_REQUEST>\ncheck mail\n</USER_REQUEST>"}`
	if err := os.WriteFile(transcriptPath, []byte(transcriptData), 0o644); err != nil {
		t.Fatal(err)
	}

	stdin, _ := json.Marshal(map[string]any{
		"conversationId": "c1",
		"workspacePaths": []string{e.wtA},
		"transcriptPath": transcriptPath,
	})
	out, code := runStdin(t, e.home, e.bin, e.ws, string(stdin), "gate", "--event", "prompt", "--host", "antigravity")
	if code != 0 {
		t.Fatalf("prompt gate = %d %q", code, out)
	}
	var payload struct {
		InjectSteps []struct {
			EphemeralMessage string `json:"ephemeralMessage"`
		} `json:"injectSteps"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil || len(payload.InjectSteps) != 1 || !strings.Contains(payload.InjectSteps[0].EphemeralMessage, "hello architect") {
		t.Fatalf("prompt gate payload = %q (%v), want injectSteps with mail", out, err)
	}
}

func TestBlueprintApproveCLI(t *testing.T) {
	e := twoAgents(t)
	wantOK(t, e.a.tool("blueprint_init", map[string]any{"name": "bp", "builder": e.build}))
	wantOK(t, e.a.tool("blueprint_roadmap", map[string]any{"name": "bp", "out_of_scope": "docs",
		"phases": []any{map[string]any{"phase": 1, "title": "cart API"}}}))
	if out, code := runIn(t, e.home, e.bin, e.ws, "blueprint", "approve", "--version", "9", "bp"); code == 0 || strings.Contains(out, "Roadmap v") {
		t.Fatalf("approve v9 = %d %q, want failure without printing a roadmap", code, out)
	}
	out, code := runIn(t, e.home, e.bin, e.ws, "blueprint", "approve", "--version", "1", "bp")
	if code != 0 || !strings.Contains(out, "Roadmap v1") || !strings.Contains(out, "cart API") || !strings.Contains(out, "approved v1") {
		t.Fatalf("approve = %d %q", code, out)
	}
	if out, code := runIn(t, e.home, e.bin, e.ws, "blueprint", "approve", "bp"); code == 0 {
		t.Fatalf("second approve = %d %q, want failure", code, out)
	}
}

// TestMusePluginHooks keeps integrations/muse runnable: every hook command
// must parse, use --host muse, and fail open; Stop must outlast the default
// hold. No PostToolUse hook:
// Muse's model ignores hook context, so `check` there would mark mail read
// and lose it.
func TestMusePluginHooks(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e gate test")
	}

	bin := buildBinary(t)
	home := t.TempDir()
	t.Cleanup(func() { runBin(t, home, bin, "stop") })
	dir := t.TempDir()

	// Validate breakroom-plugin/.muse-plugin/plugin.json
	pluginRaw, err := os.ReadFile("../../integrations/muse/breakroom-plugin/.muse-plugin/plugin.json")
	if err != nil {
		t.Fatal(err)
	}
	var pluginCfg struct {
		Capabilities struct {
			Hooks []struct {
				ID        string   `json:"id"`
				Event     string   `json:"event"`
				Command   []string `json:"command"`
				TimeoutMs int      `json:"timeoutMs"`
			} `json:"hooks"`
			McpServers []struct {
				ID        string   `json:"id"`
				Transport string   `json:"transport"`
				Command   []string `json:"command"`
			} `json:"mcpServers"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(pluginRaw, &pluginCfg); err != nil {
		t.Fatal(err)
	}
	foundEvents := make(map[string]bool)
	for _, h := range pluginCfg.Capabilities.Hooks {
		foundEvents[h.Event] = true
		if len(h.Command) == 0 || h.Command[0] != "breakroom" {
			t.Errorf("hook %s command does not start with breakroom", h.ID)
		}
		cmdStr := strings.Join(h.Command, " ")
		if !strings.Contains(cmdStr, "--host muse") {
			t.Errorf("hook %s command %q must contain --host muse", h.ID, cmdStr)
		}
		if h.Event == "Stop" && h.TimeoutMs <= 540000 {
			t.Errorf("Stop hook timeoutMs %d must exceed 540000ms", h.TimeoutMs)
		}
		if out, code := runStdin(t, home, bin, dir, `{"prompt":"hi"}`, h.Command[1:]...); code != 0 || out != "" {
			t.Errorf("plugin hook %s: %q = %d %q, want silent 0", h.Event, cmdStr, code, out)
		}
	}
	if !foundEvents["Stop"] {
		t.Error("missing Stop hook in plugin.json")
	}
	if !foundEvents["UserPromptSubmit"] {
		t.Error("missing UserPromptSubmit hook in plugin.json")
	}
	if foundEvents["PostToolUse"] {
		t.Error("PostToolUse hook set; it would mark mail read that Muse's model ignores")
	}
	if len(pluginCfg.Capabilities.McpServers) == 0 {
		t.Error("missing mcpServers in plugin.json")
	}
}

// Antigravity sends workspacePaths (no cwd) and continues the loop on
// {"decision":"continue"}; integrations/antigravity/breakroom-plugin must run that gate with
// a Stop timeout above the default hold.
func TestGateAntigravityHost(t *testing.T) {
	e := twoAgents(t)
	wantOK(t, e.b.tool("send_message", map[string]any{"to_agent": e.arch, "content": "hi"}))
	stdin, _ := json.Marshal(map[string]any{"conversationId": "c1", "workspacePaths": []string{e.wtA}, "fullyIdle": true})
	out, _ := runStdin(t, e.home, e.bin, e.ws, string(stdin), "gate", "--event", "stop", "--host", "antigravity")
	var got struct{ Decision, Reason string }
	if err := json.Unmarshal([]byte(out), &got); err != nil || got.Decision != "continue" || !strings.Contains(got.Reason, "hi") {
		t.Fatalf("antigravity stop gate = %q (%v), want continue with the mail", out, err)
	}

	raw, err := os.ReadFile("../../integrations/antigravity/breakroom-plugin/hooks.json")
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]map[string][]struct {
		Command string  `json:"command"`
		Timeout float64 `json:"timeout"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	stop := cfg["breakroom"]["Stop"]
	if len(stop) != 1 {
		t.Fatalf("want one breakroom Stop hook, got %+v", cfg)
	}
	h := stop[0]
	if h.Command != "breakroom gate --event stop --host antigravity" || h.Timeout <= 540 {
		t.Fatalf("Stop hook = %+v, want the antigravity gate with timeout > 540", h)
	}

	pre := cfg["breakroom"]["PreInvocation"]
	if len(pre) != 1 || pre[0].Command != "breakroom gate --event prompt --host antigravity" {
		t.Fatalf("PreInvocation hook = %+v, want prompt hook for antigravity", pre)
	}
}

// TestIntegrationHookFiles keeps the shipped hook configs runnable: every
// command must be breakroom with the file's --host, stop timeouts must
// exceed the default --hold 540 (silently fatal otherwise), and every
// command must fail open with no daemon. Event sets are exact: a stray
// entry (e.g. postToolUse under Copilot, whose shape differs) would mark
// mail read that the host never displays.
func TestIntegrationHookFiles(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e gate test")
	}
	bin := buildBinary(t)
	home := t.TempDir()
	t.Cleanup(func() { runBin(t, home, bin, "stop") })
	dir := t.TempDir()
	for _, tc := range []struct {
		name, file, host, stop, prompt, check string
	}{
		{"claude", "../../integrations/claude/breakroom-plugin/hooks/hooks.json", "claude", "Stop", "UserPromptSubmit", "PostToolUse"},
		{"codex", "../../integrations/codex/breakroom-plugin/hooks/hooks.json", "codex", "Stop", "UserPromptSubmit", ""},
		{"cursor", "../../integrations/cursor/breakroom-plugin/hooks/hooks.json", "cursor", "stop", "beforeSubmitPrompt", ""},
		{"vscode-plugin", "../../integrations/vscode/breakroom-plugin/com.github.copilot/hooks/hooks.json", "vscode", "Stop", "UserPromptSubmit", "PostToolUse"},
		{"copilot", "../../integrations/vscode/github-hooks/breakroom-copilot.json", "copilot", "agentStop", "userPromptSubmitted", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := os.ReadFile(tc.file)
			if err != nil {
				t.Fatal(err)
			}
			var cfg struct {
				Hooks map[string][]hookEntry `json:"hooks"`
			}
			if err := json.Unmarshal(raw, &cfg); err != nil {
				t.Fatal(err)
			}
			want := map[string]string{tc.stop: "stop", tc.prompt: "prompt"}
			if tc.check != "" {
				want[tc.check] = "check"
			}
			if len(cfg.Hooks) != len(want) {
				t.Fatalf("%s has events %v, want %v", tc.file, cfg.Hooks, want)
			}
			for ev, kind := range want {
				pairs, ok := entries(cfg.Hooks[ev])
				if !ok || len(pairs) == 0 {
					t.Fatalf("%s: event %s missing or empty", tc.file, ev)
				}
				for _, p := range pairs {
					checkHookEntry(t, tc.file, ev, kind, tc.host, p.command, p.timeout)
					args := strings.Fields(p.command)
					out, code := runStdin(t, home, bin, dir, `{"prompt":"hi"}`, args[1:]...)
					if code != 0 {
						t.Errorf("%s: %q = %d %q, want code 0", ev, p.command, code, out)
					} else if tc.host == "cursor" && kind == "prompt" {
						if strings.TrimSpace(out) != `{"continue":true}` {
							t.Errorf("%s: %q = %d %q, want continue:true", ev, p.command, code, out)
						}
					} else if out != "" {
						t.Errorf("%s: %q = %d %q, want silent 0", ev, p.command, code, out)
					}
				}
			}
		})
	}
}

type hookPair struct {
	command string
	timeout float64
}

// hookEntry covers the shipped shapes: flat {command, timeout} /
// {command, timeoutSec} objects (cursor, vscode) or Claude-style groups
// wrapping them in a hooks array (codex).
type hookEntry struct {
	Command    string  `json:"command"`
	Timeout    float64 `json:"timeout"`
	TimeoutSec float64 `json:"timeoutSec"`
	Nested     []struct {
		Command string  `json:"command"`
		Timeout float64 `json:"timeout"`
	} `json:"hooks"`
}

// entries normalizes one event's entries into command/timeout pairs.
func entries(es []hookEntry) ([]hookPair, bool) {
	var out []hookPair
	for _, e := range es {
		if len(e.Nested) > 0 {
			for _, n := range e.Nested {
				out = append(out, hookPair{n.Command, n.Timeout})
			}
			continue
		}
		if e.Command == "" {
			return nil, false
		}
		out = append(out, hookPair{e.Command, e.Timeout + e.TimeoutSec})
	}
	return out, true
}

// checkHookEntry checks that one shipped hook entry runs the right breakroom
// command for its host, with a sane timeout.
func checkHookEntry(t *testing.T, file, ev, kind, host, command string, timeout float64) {
	t.Helper()
	args := strings.Fields(command)
	if len(args) == 0 || args[0] != "breakroom" {
		t.Fatalf("%s: %s command %q must start with breakroom", file, ev, command)
	}
	if kind == "check" {
		if len(args) < 2 || args[1] != "check" {
			t.Fatalf("%s: %s command %q must be breakroom check ...", file, ev, command)
		}
		return
	}
	got := ""
	for i, a := range args {
		if a == "--host" && i+1 < len(args) {
			got = args[i+1]
		}
	}
	if got != host {
		t.Errorf("%s: %s command %q uses --host %q, want %q", file, ev, command, got, host)
	}
	wantEvent := "--event " + kind
	if !strings.Contains(command, wantEvent) {
		t.Errorf("%s: %s command %q lacks %q", file, ev, command, wantEvent)
	}
	if kind == "stop" && timeout <= 540 {
		t.Errorf("%s: stop timeout %v must exceed the default --hold 540", file, timeout)
	}
}
