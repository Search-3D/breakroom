package protocol_test

import (
	"encoding/json"
	"testing"

	"github.com/search-3d/breakroom/internal/protocol"
)

func TestProtocolJSONSerialization(t *testing.T) {
	gateReq := protocol.GateStopRequest{
		WorkspaceRoot:  "/ws",
		WorktreePath:   "/ws/wt-a",
		Host:           "antigravity",
		Status:         "completed",
		Ancestors:      []int64{101, 102},
		StopHookActive: true,
		HoldSec:        540,
	}
	data, err := json.Marshal(gateReq)
	if err != nil {
		t.Fatalf("marshal GateStopRequest: %v", err)
	}

	var parsedReq protocol.GateStopRequest
	if err := json.Unmarshal(data, &parsedReq); err != nil {
		t.Fatalf("unmarshal GateStopRequest: %v", err)
	}
	if parsedReq.WorkspaceRoot != gateReq.WorkspaceRoot ||
		parsedReq.WorktreePath != gateReq.WorktreePath ||
		parsedReq.Host != gateReq.Host ||
		parsedReq.Status != gateReq.Status ||
		parsedReq.StopHookActive != gateReq.StopHookActive ||
		parsedReq.HoldSec != gateReq.HoldSec {
		t.Errorf("GateStopRequest roundtrip mismatch: got %+v, want %+v", parsedReq, gateReq)
	}

	gateDec := protocol.GateDecision{
		Action:      "block",
		Kind:        "mail",
		Reason:      "unread mail",
		Agent:       "agent-1",
		MessageIDs:  []int64{1, 2},
		SuppressAck: true,
		HostPayload: map[string]string{"decision": "continue", "reason": "unread mail"},
	}
	decData, err := json.Marshal(gateDec)
	if err != nil {
		t.Fatalf("marshal GateDecision: %v", err)
	}
	var parsedDec protocol.GateDecision
	if err := json.Unmarshal(decData, &parsedDec); err != nil {
		t.Fatalf("unmarshal GateDecision: %v", err)
	}
	if parsedDec.Action != gateDec.Action || parsedDec.Kind != gateDec.Kind || parsedDec.SuppressAck != gateDec.SuppressAck {
		t.Errorf("GateDecision roundtrip mismatch: got %+v, want %+v", parsedDec, gateDec)
	}

	promptReq := protocol.PromptRecordRequest{
		WorkspaceRoot: "/ws",
		WorktreePath:  "/ws/wt-a",
		Host:          "claude",
		Text:          "hello",
		Ancestors:     []int64{10},
	}
	pData, err := json.Marshal(promptReq)
	if err != nil {
		t.Fatalf("marshal PromptRecordRequest: %v", err)
	}
	var parsedPrompt protocol.PromptRecordRequest
	if err := json.Unmarshal(pData, &parsedPrompt); err != nil {
		t.Fatalf("unmarshal PromptRecordRequest: %v", err)
	}
	if parsedPrompt.Text != promptReq.Text || parsedPrompt.Host != promptReq.Host {
		t.Errorf("PromptRecordRequest roundtrip mismatch")
	}
}

func TestJSONWireTagNaming(t *testing.T) {
	// Verify that serialized JSON keys are strictly snake_case or match expected wire protocols.
	tests := []struct {
		name     string
		v        any
		wantKeys []string
	}{
		{
			name: "GateStopRequest",
			v: protocol.GateStopRequest{
				WorkspaceRoot:  "/ws",
				WorktreePath:   "/wt",
				Host:           "claude",
				Status:         "completed",
				Ancestors:      []int64{1},
				StopHookActive: true,
				HoldSec:        540,
			},
			wantKeys: []string{"workspace_root", "worktree_path", "host", "status", "ancestors", "stop_hook_active", "hold_sec"},
		},
		{
			name: "GateDecision",
			v: protocol.GateDecision{
				Action:      "block",
				Kind:        "mail",
				Reason:      "unread",
				Agent:       "a1",
				MessageIDs:  []int64{1},
				SuppressAck: true,
				HostPayload: map[string]string{"k": "v"},
			},
			wantKeys: []string{"action", "kind", "reason", "agent", "message_ids", "suppress_ack", "host_payload"},
		},
		{
			name: "Message",
			v: protocol.Message{
				ID:             1,
				From:           "arch",
				To:             "builder",
				Type:           "chat",
				Content:        "hello",
				DiffAttachment: "+test",
				CreatedAt:      "2026-10-17T00:00:00Z",
			},
			wantKeys: []string{"id", "from", "to", "type", "content", "diff_attachment", "created_at"},
		},
		{
			name: "Roadmap",
			v: protocol.Roadmap{
				Version:       1,
				Phases:        []protocol.RoadmapPhase{{Phase: 1, Title: "T", Delivers: "D"}},
				OutOfScope:    "none",
				Base:          "main",
				ProposedAt:    "now",
				ApprovedVia:   "cli",
				ApprovalQuote: "ok",
			},
			wantKeys: []string{"version", "phases", "out_of_scope", "base", "proposed_at", "approved_via", "approval_quote"},
		},
		{
			name: "BlueprintSubmitRequest",
			v: protocol.BlueprintSubmitRequest{
				WorkspaceRoot: "/ws",
				Name:          "bp",
				Phase:         1,
				Caller:        "builder",
				Items:         []protocol.BlueprintItem{{RequirementID: "r1", Summary: "done"}},
				Proof:         "ok",
				Notes:         "note",
			},
			wantKeys: []string{"workspace_root", "name", "phase", "caller", "items", "proof", "notes"},
		},
		{
			name: "BlueprintInspectRequest",
			v: protocol.BlueprintInspectRequest{
				WorkspaceRoot: "/ws",
				Name:          "bp",
				Phase:         1,
				Caller:        "arch",
				Verdict:       "punchlist",
				AcceptanceIDs: []string{"a1"},
				Findings:      []protocol.BlueprintFinding{{RequirementID: "r1", Issue: "iss", Fix: "fix"}},
			},
			wantKeys: []string{"workspace_root", "name", "phase", "caller", "verdict", "acceptance_ids", "findings"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			data, err := json.Marshal(tc.v)
			if err != nil {
				t.Fatalf("Marshal(%s) failed: %v", tc.name, err)
			}
			var m map[string]any
			if err := json.Unmarshal(data, &m); err != nil {
				t.Fatalf("Unmarshal(%s) failed: %v", tc.name, err)
			}
			for _, k := range tc.wantKeys {
				if _, ok := m[k]; !ok {
					t.Errorf("%s JSON missing expected wire key %q; got map: %v", tc.name, k, m)
				}
			}
		})
	}
}
