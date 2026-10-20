package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// blueprint_approve always speaks for the human via chat, as this session's
// agent; the daemon decides whether a recorded prompt backs it.
func TestRoadmapTools(t *testing.T) {
	mux := http.NewServeMux()
	var proposed, approved map[string]any
	mux.HandleFunc("POST /v1/blueprints/roadmap", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&proposed)
		_, _ = w.Write([]byte(`{"version":1,"status":"pending"}`))
	})
	mux.HandleFunc("POST /v1/blueprints/approve", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&approved)
		if approved["quote"] != "approved" {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":"human_approval_required: no prompt"}`))
			return
		}
		_, _ = w.Write([]byte(`{"version":1,"approved_via":"chat"}`))
	})
	stubDaemon(t, mux)
	s := &session{client: NewClient(), workspace: "/ws", agents: map[string]string{"owl-1": "/ws"}}
	ctx := context.Background()

	_, env, _ := s.handleBlueprintRoadmap(ctx, nil, bpRoadmapArgs{Name: "bp",
		Phases: []RoadmapPhase{{Phase: 1, Title: "cart"}}, OutOfScope: "docs"})
	if !env.OK || env.Data.(map[string]any)["version"] != 1 || proposed["caller"] != "owl-1" || proposed["out_of_scope"] != "docs" {
		t.Fatalf("roadmap = %+v, sent %v", env, proposed)
	}

	_, env, _ = s.handleBlueprintApprove(ctx, nil, bpApproveArgs{Name: "bp", Version: 1, Quote: "nope"})
	if env.OK || !strings.HasPrefix(env.Err, "human_approval_required:") {
		t.Fatalf("approve without prompt = %+v", env)
	}
	_, env, _ = s.handleBlueprintApprove(ctx, nil, bpApproveArgs{Name: "bp", Version: 1, Quote: "approved"})
	if !env.OK || env.Data.(map[string]any)["approved_via"] != "chat" {
		t.Fatalf("approve = %+v", env)
	}
	if approved["via"] != "chat" || approved["caller"] != "owl-1" || approved["version"] != float64(1) {
		t.Fatalf("approve sent %v", approved)
	}
}
