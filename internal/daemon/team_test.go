package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// initSeries has the architect start a series (no builder named).
func initSeries(t *testing.T, h http.Handler, ws, name, arch string) {
	t.Helper()
	code, out := doPost(t, h, "/v1/blueprints/init", map[string]any{
		"workspace_root": ws, "name": name, "architect": arch, "goal": "g",
	})
	if code != 200 {
		t.Fatalf("init %s: %d %v", name, code, out)
	}
}

// join has a builder join a series by name.
func join(t *testing.T, h http.Handler, ws, caller, name string) map[string]any {
	t.Helper()
	code, out := doPost(t, h, "/v1/blueprints/join", map[string]any{
		"workspace_root": ws, "caller": caller, "name": name,
	})
	if code != 200 {
		t.Fatalf("join: %d %v", code, out)
	}
	return out
}

// builderOf returns a blueprint's builder.
func builderOf(t *testing.T, h http.Handler, ws, name string) string {
	t.Helper()
	_, out := doPost(t, h, "/v1/blueprints/status", map[string]any{"workspace_root": ws, "name": name})
	return out["blueprint"].(map[string]any)["builder"].(string)
}

func TestJoinRoute(t *testing.T) {
	srv := newTestServer(t)
	h := srv.Handler()
	a1 := register(t, h, "/ws", "/ws/a1", "architect")
	initSeries(t, h, "/ws", "s1", a1)
	b := register(t, h, "/ws", "/ws/b", "builder")
	if out := join(t, h, "/ws", b, ""); out["joined"] != true || out["name"] != "s1" || out["architect"] != a1 {
		t.Fatalf("sole open series: %v", out)
	}
	a2 := register(t, h, "/ws", "/ws/a2", "architect")
	a3 := register(t, h, "/ws", "/ws/a3", "architect")
	initSeries(t, h, "/ws", "s3", a3)
	initSeries(t, h, "/ws", "s2", a2)
	c := register(t, h, "/ws", "/ws/c", "builder")
	out := join(t, h, "/ws", c, "")
	if out["joined"] != false || !reflect.DeepEqual(out["open"], []any{"s2", "s3"}) {
		t.Fatalf("two open: %v", out)
	}
	join(t, h, "/ws", c, "s2")
	d := register(t, h, "/ws", "/ws/d", "builder")
	join(t, h, "/ws", d, "s3")
	e := register(t, h, "/ws", "/ws/e", "builder")
	if out := join(t, h, "/ws", e, ""); out["joined"] != false || out["waiting"] != true {
		t.Fatalf("none open: %v", out)
	}
	a4 := register(t, h, "/ws", "/ws/a4", "architect")
	initSeries(t, h, "/ws", "s4", a4)
	if got := builderOf(t, h, "/ws", "s4"); got != e {
		t.Fatalf("lone waiter auto-join builder = %q, want %s", got, e)
	}
	if w, _ := srv.store.ListSeriesWaiters(context.Background(), "/ws"); len(w) != 0 {
		t.Fatalf("waiters = %v, want cleared", w)
	}
	f1 := register(t, h, "/ws", "/ws/f1", "builder")
	f2 := register(t, h, "/ws", "/ws/f2", "builder")
	join(t, h, "/ws", f1, "")
	join(t, h, "/ws", f2, "")
	a5 := register(t, h, "/ws", "/ws/a5", "architect")
	initSeries(t, h, "/ws", "s5", a5)
	for _, w := range []string{f1, f2} {
		msgs := takeMail(t, h, "/ws", w)
		if len(msgs) != 1 || msgs[0]["type"] != "blueprint_series" || !strings.Contains(msgs[0]["content"].(string), "s5") {
			t.Fatalf("waiter %s mail = %v", w, msgs)
		}
	}
}

func TestTeamPairing(t *testing.T) {
	h := newTestServer(t).Handler()
	a := register(t, h, "/ws", "/ws/a", "architect")
	b := register(t, h, "/ws", "/ws/b", "builder")
	initSeries(t, h, "/ws", "s1", a)
	join(t, h, "/ws", b, "s1")
	if code, out := doPost(t, h, "/v1/blueprints/close", map[string]any{"workspace_root": "/ws", "name": "s1", "caller": a}); code != 200 {
		t.Fatalf("close: %d %v", code, out)
	}
	initSeries(t, h, "/ws", "s2", a)
	if got := builderOf(t, h, "/ws", "s2"); got != b {
		t.Fatalf("next series builder = %q, want team builder %s", got, b)
	}
	doPost(t, h, "/v1/agents/release", map[string]any{"workspace_root": "/ws", "agent_id": b})
	doPost(t, h, "/v1/blueprints/close", map[string]any{"workspace_root": "/ws", "name": "s2", "caller": a})
	initSeries(t, h, "/ws", "s3", a)
	if got := builderOf(t, h, "/ws", "s3"); got != "" {
		t.Fatalf("offline team builder assigned: %q", got)
	}
}

func TestVisiblePeers(t *testing.T) {
	srv := newTestServer(t)
	h := srv.Handler()
	a1 := register(t, h, "/ws", "/ws/a1", "architect")
	a2 := register(t, h, "/ws", "/ws/a2", "architect")
	b1 := register(t, h, "/ws", "/ws/b1", "builder")
	u := register(t, h, "/ws", "/ws/u", "reviewer")
	initSeries(t, h, "/ws", "s1", a1)
	join(t, h, "/ws", b1, "s1")
	initSeries(t, h, "/ws", "s2", a2)
	ctx := context.Background()
	all, err := srv.store.ListAgents(ctx, "/ws", false)
	if err != nil {
		t.Fatal(err)
	}
	ids := func(me string) []string {
		out := []string{}
		for _, a := range srv.visiblePeers(ctx, "/ws", me, all) {
			out = append(out, a.AgentID)
		}
		sort.Strings(out)
		return out
	}
	sorted := func(v ...string) []string { sort.Strings(v); return v }
	for _, tc := range []struct {
		me   string
		want []string
	}{
		{b1, sorted(a1)},
		{a1, sorted(a2, b1, u)},
		{a2, sorted(a1, u)},
		{u, sorted(a1, a2, b1)},
	} {
		t.Run(tc.me, func(t *testing.T) {
			if got := ids(tc.me); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("visiblePeers(%s) = %v, want %v", tc.me, got, tc.want)
			}
		})
	}
	// list_agents with a caller and broadcasts follow the same scoping.
	req := httptest.NewRequest(http.MethodGet, "/v1/agents?workspace_root=/ws&caller="+b1, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var listed struct {
		Agents []struct {
			AgentID string `json:"agent_id"`
		} `json:"agents"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil || len(listed.Agents) != 2 || listed.Agents[0].AgentID != b1 || listed.Agents[1].AgentID != a1 {
		t.Fatalf("list as builder = %s", rec.Body.String())
	}
	for _, id := range []string{a1, a2, u} {
		takeMail(t, h, "/ws", id)
	}
	doPost(t, h, "/v1/messages/send", map[string]any{"workspace_root": "/ws", "from": b1, "to": "*", "content": "hello team"})
	if got := takeMail(t, h, "/ws", a1); len(got) != 1 {
		t.Fatalf("own architect got %d broadcasts", len(got))
	}
	for _, id := range []string{a2, u} {
		if got := takeMail(t, h, "/ws", id); len(got) != 0 {
			t.Fatalf("%s outside the team got the broadcast: %v", id, got)
		}
	}
}

func TestSeriesWaiterHolds(t *testing.T) {
	h := newTestServer(t).Handler()
	e := register(t, h, "/ws", "/ws/e", "builder")
	join(t, h, "/ws", e, "")
	stop := map[string]any{"workspace_root": "/ws", "worktree_path": "/ws/e", "hold_sec": 1}
	// Alone: nobody can open a series, so waiting would never end.
	if code, out := doPost(t, h, "/v1/gate/stop", stop); code != 200 || out["action"] != "allow" {
		t.Fatalf("lone waiter gate = %d %v, want allow", code, out)
	}
	register(t, h, "/ws", "/ws/a", "architect")
	if code, out := doPost(t, h, "/v1/gate/stop", stop); code != 200 || out["kind"] != "hold_expired" {
		t.Fatalf("waiter gate = %d %v, want hold_expired", code, out)
	}
}

// register_agent's series reaches the store: a builder that went offline
// resumes its identity from another folder by naming its series.
func TestRegisterResumesBySeries(t *testing.T) {
	srv := newTestServer(t)
	h := srv.Handler()
	a := register(t, h, "/ws", "/ws/a", "architect")
	initSeries(t, h, "/ws", "s1", a)
	b := register(t, h, "/ws", "/ws/b", "builder")
	join(t, h, "/ws", b, "s1")
	doPost(t, h, "/v1/agents/release", map[string]any{"workspace_root": "/ws", "agent_id": b})
	code, out := doPost(t, h, "/v1/agents/register", map[string]any{
		"workspace_root": "/ws", "worktree_path": "/ws/elsewhere", "role": "builder", "series": "s1",
	})
	if code != 200 || out["agent_id"] != b {
		t.Fatalf("register with series = %d %v, want %s back", code, out, b)
	}
}

// A phase specced before the builder joined reaches the builder on join.
func TestJoinResendsActiveSpec(t *testing.T) {
	h := newTestServer(t).Handler()
	a := register(t, h, "/ws", "/ws/a", "architect")
	initSeries(t, h, "/ws", "s1", a)
	mustPost(t, h, "/v1/blueprints/roadmap", map[string]any{
		"workspace_root": "/ws", "name": "s1", "caller": a,
		"phases": []any{map[string]any{"phase": 1, "title": "t"}},
	})
	mustPost(t, h, "/v1/blueprints/approve", map[string]any{"workspace_root": "/ws", "name": "s1", "via": "cli"})
	mustPost(t, h, "/v1/blueprints/spec", map[string]any{
		"workspace_root": "/ws", "name": "s1", "caller": a, "phase": 1, "title": "t", "objective": "o",
		"requirements": []any{map[string]any{"id": "r1", "text": "req"}},
		"acceptance":   []any{map[string]any{"id": "a1", "text": "acc"}},
	})
	b := register(t, h, "/ws", "/ws/b", "builder")
	join(t, h, "/ws", b, "s1")
	for _, m := range takeMail(t, h, "/ws", b) {
		if m["type"] == "blueprint_spec" && strings.Contains(m["content"].(string), "r1: req") {
			return
		}
	}
	t.Fatal("joined builder never got the phase 1 spec")
}

func TestTeamBuilderRemovesWaiterAndDoesNotHold(t *testing.T) {
	srv := newTestServer(t)
	h := srv.Handler()
	a := register(t, h, "/ws", "/ws/a", "architect")
	b := register(t, h, "/ws", "/ws/b", "builder")

	// Series 1: a and b work together
	initSeries(t, h, "/ws", "s1", a)
	join(t, h, "/ws", b, "s1")
	code, out := doPost(t, h, "/v1/blueprints/close", map[string]any{"workspace_root": "/ws", "name": "s1", "caller": a})
	if code != 200 {
		t.Fatalf("close s1: %d %v", code, out)
	}

	// b calls join before architect initializes next series -> waiter
	out = join(t, h, "/ws", b, "")
	if out["joined"] != false || out["waiting"] != true {
		t.Fatalf("expected waiting=true, got %v", out)
	}

	// a initializes s2: teamBuilder auto-attaches b
	code, initOut := doPost(t, h, "/v1/blueprints/init", map[string]any{
		"workspace_root": "/ws", "name": "s2", "architect": a, "goal": "g",
	})
	if code != 200 || initOut["builder"] != b {
		t.Fatalf("init s2: %d %v, want builder %s", code, initOut, b)
	}

	// Verify series waiters in store is empty
	waiters, err := srv.store.ListSeriesWaiters(context.Background(), "/ws")
	if err != nil || len(waiters) != 0 {
		t.Fatalf("waiters = %v, want empty", waiters)
	}

	// Drain messages so unread mail doesn't block stop
	takeMail(t, h, "/ws", b)

	// Stop gate should not hold b as a series waiter! (It parks on s2's
	// roadmap instead.)
	stop := map[string]any{"workspace_root": "/ws", "worktree_path": "/ws/b", "hold_sec": 1}
	if code, out := doPost(t, h, "/v1/gate/stop", stop); code != 200 || strings.Contains(fmt.Sprint(out["reason"]), "series to join") {
		t.Fatalf("builder after auto-attach gate = %d %v, want no series-waiter hold", code, out)
	}
}
