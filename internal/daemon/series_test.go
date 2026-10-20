package daemon

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// seriesRepo is a real repo with one commit, used as the workspace (and
// main checkout). Symlinks resolved, as the proxy reports workspaces.
func seriesRepo(t *testing.T) string {
	t.Helper()
	repo := initRepo(t)
	writeFile(t, repo, "README.md", "hi\n")
	sh(t, repo, "add", ".")
	sh(t, repo, "commit", "-m", "init")
	ws, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatal(err)
	}
	return ws
}

// gitOut runs a git command in dir and returns its trimmed output.
func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}

// proposeApprove proposes a roadmap (optionally with a base) and approves
// it from the CLI route, returning the approve response.
func proposeApprove(t *testing.T, h http.Handler, ws, name, arch, base string, titles ...string) map[string]any {
	t.Helper()
	phases := []any{}
	for i, title := range titles {
		phases = append(phases, map[string]any{"phase": i + 1, "title": title})
	}
	code, out := doPost(t, h, "/v1/blueprints/roadmap", map[string]any{
		"workspace_root": ws, "name": name, "caller": arch, "phases": phases, "base": base,
	})
	if code != 200 {
		t.Fatalf("roadmap: %d %v", code, out)
	}
	code, out = doPost(t, h, "/v1/blueprints/approve", map[string]any{"workspace_root": ws, "name": name, "via": "cli"})
	if code != 200 {
		t.Fatalf("approve: %d %v", code, out)
	}
	return out
}

// blueprintField returns one string field of a blueprint's status.
func blueprintField(t *testing.T, h http.Handler, ws, name, field string) string {
	t.Helper()
	_, out := doPost(t, h, "/v1/blueprints/status", map[string]any{"workspace_root": ws, "name": name})
	v, _ := out["blueprint"].(map[string]any)[field].(string)
	return v
}

// agentWorktree returns the worktree an agent is registered in.
func agentWorktree(t *testing.T, h http.Handler, ws, id string) string {
	t.Helper()
	srvAgents := listAgents(t, h, ws)
	for _, a := range srvAgents {
		if a["agent_id"] == id {
			return a["worktree_path"].(string)
		}
	}
	t.Fatalf("no agent %s", id)
	return ""
}

// mailContaining reports whether any message contains sub.
func mailContaining(msgs []map[string]any, sub string) bool {
	for _, m := range msgs {
		if strings.Contains(m["content"].(string), sub) {
			return true
		}
	}
	return false
}

func TestSeriesOnFirstApproval(t *testing.T) {
	ws := seriesRepo(t)
	h := newTestServer(t).Handler()
	arch := register(t, h, ws, ws, "architect")
	build := register(t, h, ws, ws, "builder")
	initSeries(t, h, ws, "s1", arch)
	join(t, h, ws, build, "s1")
	takeMail(t, h, ws, build)
	out := proposeApprove(t, h, ws, "s1", arch, "", "one")
	wt := filepath.Join(ws, ".workspaces", "s1")
	if got := blueprintField(t, h, ws, "s1", "worktree_path"); got != wt {
		t.Fatalf("worktree = %q, want %q (out %v)", got, wt, out)
	}
	if got := blueprintField(t, h, ws, "s1", "branch"); got != "breakroom/s1" {
		t.Fatalf("branch = %q", got)
	}
	if got := blueprintField(t, h, ws, "s1", "base_ref"); got != gitOut(t, ws, "rev-parse", "HEAD") {
		t.Fatalf("base = %q", got)
	}
	if got := agentWorktree(t, h, ws, build); got != wt {
		t.Fatalf("builder worktree = %q, want %q", got, wt)
	}
	if mail := takeMail(t, h, ws, build); len(mail) != 0 {
		t.Fatalf("builder mail before spec = %v, want none", mail)
	}
	handoff, _ := out["handoff"].(string)
	if !strings.Contains(handoff, ".workspaces/s1") || !strings.Contains(handoff, "breakroom/s1") {
		t.Fatalf("handoff = %q", handoff)
	}
	if status := gitOut(t, ws, "status", "--porcelain=v1", "-uall"); status != "" {
		t.Fatalf("main checkout git status dirty from .workspaces: %q", status)
	}
	// A later roadmap version does not create another worktree.
	out = proposeApprove(t, h, ws, "s1", arch, "", "one", "two")
	if out["series_error"] != nil || blueprintField(t, h, ws, "s1", "worktree_path") != wt {
		t.Fatalf("second approval: %v", out)
	}
}

func TestSeriesAutoInitsGitRepo(t *testing.T) {
	ws, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, ws, "main.go", "package main\n")
	h := newTestServer(t).Handler()
	arch := register(t, h, ws, ws, "architect")
	initSeries(t, h, ws, "s1", arch)
	out := proposeApprove(t, h, ws, "s1", arch, "", "one")
	wt := filepath.Join(ws, ".workspaces", "s1")
	if got := blueprintField(t, h, ws, "s1", "worktree_path"); got != wt {
		t.Fatalf("auto-init worktree = %q, want %q (out %v)", got, wt, out)
	}
	if h, _ := out["handoff"].(string); !strings.Contains(h, "initial commit of 1 file") {
		t.Fatalf("handoff doesn't report the initial commit: %q", h)
	}
	if status := gitOut(t, ws, "status", "--porcelain=v1", "-uall"); status != "" {
		t.Fatalf("auto-init git status dirty: %q", status)
	}
}

// A bare clone with linked worktrees: the workspace is the bare repo, so
// the series goes beside it (no work tree to check for dirt).
func TestSeriesBareClone(t *testing.T) {
	src := seriesRepo(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ws := filepath.Join(root, "repo.git")
	sh(t, root, "clone", "--bare", src, ws)
	mainWT := filepath.Join(root, "main")
	sh(t, ws, "worktree", "add", mainWT)
	h := newTestServer(t).Handler()
	arch := register(t, h, ws, mainWT, "architect")
	initSeries(t, h, ws, "s1", arch)
	out := proposeApprove(t, h, ws, "s1", arch, "", "one")
	wt := filepath.Join(root, ".workspaces", "s1")
	if got := blueprintField(t, h, ws, "s1", "worktree_path"); got != wt {
		t.Fatalf("worktree = %q, want %q (out %v)", got, wt, out)
	}
	if gitOut(t, wt, "rev-parse", "HEAD") != gitOut(t, src, "rev-parse", "HEAD") {
		t.Fatal("series not based on the bare repo's HEAD")
	}
}

func TestSeriesDirtyMain(t *testing.T) {
	ws := seriesRepo(t)
	h := newTestServer(t).Handler()
	arch := register(t, h, ws, ws, "architect")
	initSeries(t, h, ws, "s1", arch)
	writeFile(t, ws, "README.md", "edited\n")
	out := proposeApprove(t, h, ws, "s1", arch, "", "one")
	if blueprintField(t, h, ws, "s1", "worktree_path") == "" {
		t.Fatalf("no series created: %v", out)
	}
	if h, _ := out["handoff"].(string); !strings.Contains(h, "uncommitted") {
		t.Fatalf("handoff doesn't warn about the dirty main checkout: %q", h)
	}
}

func TestSeriesExistsRetry(t *testing.T) {
	ws := seriesRepo(t)
	h := newTestServer(t).Handler()
	arch := register(t, h, ws, ws, "architect")
	initSeries(t, h, ws, "s1", arch)
	sh(t, ws, "branch", "breakroom/s1")
	out := proposeApprove(t, h, ws, "s1", arch, "", "one")
	if e, _ := out["series_error"].(string); e == "" || blueprintField(t, h, ws, "s1", "worktree_path") != "" {
		t.Fatalf("want series_error and no worktree: %v", out)
	}
	code, out := doPost(t, h, "/v1/blueprints/series", map[string]any{
		"workspace_root": ws, "caller": arch, "name": "s1", "branch": "breakroom/s1-2",
	})
	if code != 200 || blueprintField(t, h, ws, "s1", "branch") != "breakroom/s1-2" || blueprintField(t, h, ws, "s1", "worktree_path") == "" {
		t.Fatalf("retry: %d %v", code, out)
	}
	code, out = doPost(t, h, "/v1/blueprints/series", map[string]any{"workspace_root": ws, "caller": "nobody", "name": "s1"})
	wantErr(t, code, out, "forbidden_role")
}

func TestJoinAfterSeries(t *testing.T) {
	ws := seriesRepo(t)
	h := newTestServer(t).Handler()
	arch := register(t, h, ws, ws, "architect")
	initSeries(t, h, ws, "s1", arch)
	out := proposeApprove(t, h, ws, "s1", arch, "", "one")
	if hd, _ := out["handoff"].(string); !strings.Contains(hd, "Join Breakroom as the builder for `s1`") {
		t.Fatalf("handoff lacks the builder line: %q", hd)
	}
	wt := blueprintField(t, h, ws, "s1", "worktree_path")
	build := register(t, h, ws, ws, "builder")
	if res := join(t, h, ws, build, "s1"); res["worktree"] != wt {
		t.Fatalf("join = %v, want worktree %s", res, wt)
	}
	if got := agentWorktree(t, h, ws, build); got != wt {
		t.Fatalf("builder worktree = %q", got)
	}
	if mail := takeMail(t, h, ws, build); len(mail) != 0 {
		t.Fatalf("joining builder mail before spec = %v, want none (join reply carries worktree)", mail)
	}
}

func TestSpecCarriesWorktree(t *testing.T) {
	ws := seriesRepo(t)
	h := newTestServer(t).Handler()
	arch := register(t, h, ws, ws, "architect")
	build := register(t, h, ws, ws, "builder")
	initSeries(t, h, ws, "s1", arch)
	join(t, h, ws, build, "s1")
	proposeApprove(t, h, ws, "s1", arch, "", "one")
	takeMail(t, h, ws, build)
	spec(t, h, ws, "s1", arch, 1, "one")
	wt := blueprintField(t, h, ws, "s1", "worktree_path")
	msgs := takeMail(t, h, ws, build)
	if !mailContaining(msgs, "Work only in") || !mailContaining(msgs, wt) {
		t.Fatalf("spec mail = %v", msgs)
	}
}

func TestRoadmapBase(t *testing.T) {
	ws := seriesRepo(t)
	sh(t, ws, "checkout", "-q", "-b", "breakroom/s0")
	writeFile(t, ws, "b.txt", "b\n")
	sh(t, ws, "add", ".")
	sh(t, ws, "commit", "-q", "-m", "s0 work")
	s0 := gitOut(t, ws, "rev-parse", "HEAD")
	sh(t, ws, "checkout", "-q", "main")
	h := newTestServer(t).Handler()
	arch := register(t, h, ws, ws, "architect")
	initSeries(t, h, ws, "s1", arch)
	proposeApprove(t, h, ws, "s1", arch, "breakroom/s0", "one")
	if got := blueprintField(t, h, ws, "s1", "base_ref"); got != s0 {
		t.Fatalf("base = %q, want s0's commit %q", got, s0)
	}
}

func TestHandoffRoadmapCopy(t *testing.T) {
	got := handoffRoadmap("s1", 2)
	if !strings.Contains(got, "Reply approved") || !strings.Contains(got, "s1") {
		t.Fatalf("handoff = %q", got)
	}
	h := newTestServer(t).Handler()
	arch := register(t, h, "/ws", "/ws/a", "architect")
	initSeries(t, h, "/ws", "s1", arch)
	_, out := doPost(t, h, "/v1/blueprints/roadmap", map[string]any{
		"workspace_root": "/ws", "name": "s1", "caller": arch,
		"phases": []any{map[string]any{"phase": 1, "title": "one"}},
	})
	if hd, _ := out["handoff"].(string); !strings.Contains(hd, "Reply approved") {
		t.Fatalf("roadmap response handoff = %v", out)
	}
}

// listAgents lists a workspace's agents through the API.
func listAgents(t *testing.T, h http.Handler, ws string) []map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/agents?workspace_root="+url.QueryEscape(ws), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out struct {
		Agents []map[string]any `json:"agents"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("list: %s", rec.Body.String())
	}
	return out.Agents
}

// spec has the architect spec a phase with the given title.
func spec(t *testing.T, h http.Handler, ws, name, arch string, phase int, title string) {
	t.Helper()
	code, out := doPost(t, h, "/v1/blueprints/spec", map[string]any{
		"workspace_root": ws, "name": name, "caller": arch, "phase": phase, "title": title, "objective": "o",
		"requirements": []any{map[string]any{"id": "r1", "text": "req"}},
		"acceptance":   []any{map[string]any{"id": "a1", "text": "acc"}},
	})
	if code != 200 {
		t.Fatalf("spec: %d %v", code, out)
	}
}

func TestSeriesPathEscape(t *testing.T) {
	ws := seriesRepo(t)
	h := newTestServer(t).Handler()
	arch := register(t, h, ws, ws, "architect")
	initSeries(t, h, ws, "s1", arch)
	sh(t, ws, "branch", "breakroom/s1")
	out := proposeApprove(t, h, ws, "s1", arch, "", "one")
	if e, _ := out["series_error"].(string); e == "" || blueprintField(t, h, ws, "s1", "worktree_path") != "" {
		t.Fatalf("want series_error and no worktree: %v", out)
	}

	// Above the workspace's parent: t.TempDir() would be a sibling, which is allowed.
	escapePath := filepath.Join(filepath.Dir(filepath.Dir(ws)), "escape-wt")
	code, out := doPost(t, h, "/v1/blueprints/series", map[string]any{
		"workspace_root": ws, "caller": arch, "name": "s1", "branch": "breakroom/s1-2", "path": escapePath,
	})
	wantErr(t, code, out, "worktree_escape")

	code, out = doPost(t, h, "/v1/blueprints/series", map[string]any{
		"workspace_root": ws, "caller": arch, "name": "s1", "branch": "breakroom/s1-2", "path": "../../escape-wt",
	})
	wantErr(t, code, out, "worktree_escape")

	code, out = doPost(t, h, "/v1/blueprints/series", map[string]any{
		"workspace_root": ws, "caller": arch, "name": "s1", "branch": "breakroom/s1-2", "path": ws,
	})
	wantErr(t, code, out, "worktree_escape")
}

func TestSeriesDiffIncludesCommittedWork(t *testing.T) {
	ws := seriesRepo(t)
	h := newTestServer(t).Handler()
	arch := register(t, h, ws, ws, "architect")
	build := register(t, h, ws, ws, "builder")
	initSeries(t, h, ws, "s1", arch)
	join(t, h, ws, build, "s1")
	proposeApprove(t, h, ws, "s1", arch, "", "one")
	wt := filepath.Join(ws, ".workspaces", "s1")
	writeFile(t, wt, "committed.txt", "phase work\n")
	sh(t, wt, "add", "-A")
	sh(t, wt, "commit", "-m", "phase 1")
	code, out := doPost(t, h, "/v1/git/diff", map[string]any{"workspace_root": ws, "agent_id": build})
	if code != 200 || !strings.Contains(fmt.Sprint(out["diff"]), "committed.txt") {
		t.Fatalf("builder's committed work missing from diff: %d %v", code, out)
	}
}
