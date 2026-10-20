package render

import (
	"strings"
	"testing"

	"github.com/search-3d/breakroom/internal/protocol"
)

func TestFormatMessage(t *testing.T) {
	m1 := protocol.Message{
		From:      "arch",
		To:        "builder",
		Type:      "chat",
		CreatedAt: "2026-10-17T12:00:00Z",
		Content:   "Hello builder",
	}
	got1 := FormatMessage(m1)
	want1 := "[arch -> builder] (chat, 2026-10-17T12:00:00Z)\nHello builder\n\n"
	if got1 != want1 {
		t.Fatalf("FormatMessage() = %q, want %q", got1, want1)
	}

	m2 := protocol.Message{
		From:           "builder",
		To:             "arch",
		Type:           "review_req",
		CreatedAt:      "2026-10-17T12:01:00Z",
		Content:        "Deliverable ready",
		DiffAttachment: "+func Foo() {}\n",
	}
	got2 := FormatMessage(m2)
	want2 := "[builder -> arch] (review_req, 2026-10-17T12:01:00Z)\nDeliverable ready\n\n#### Diff Attachment\n```diff\n+func Foo() {}\n```\n\n"
	if got2 != want2 {
		t.Fatalf("FormatMessage(with diff) = %q, want %q", got2, want2)
	}
}

func TestFormatMailWakeup(t *testing.T) {
	msgs := []protocol.Message{
		{From: "a1", To: "a2", Type: "chat", CreatedAt: "2026-10-17T12:00:00Z", Content: "hi"},
	}
	got := FormatMailWakeup("a2", msgs)
	if !strings.Contains(got, "Breakroom: 1 new message(s) for a2.") {
		t.Fatalf("unexpected mail header: %s", got)
	}
	if !strings.Contains(got, "[a1 -> a2] (chat, 2026-10-17T12:00:00Z)\nhi\n\n") {
		t.Fatalf("unexpected mail body: %s", got)
	}
}

func TestFormatRoadmap(t *testing.T) {
	rm := protocol.Roadmap{
		Version: 1,
		Phases: []protocol.RoadmapPhase{
			{Phase: 1, Title: "Setup", Delivers: "skeleton"},
			{Phase: 2, Title: "Core"},
		},
		OutOfScope: "GUI frontend",
	}
	got := FormatRoadmap(rm)
	want := "Roadmap v1:\n  1. Setup: skeleton\n  2. Core\nOut of scope: GUI frontend\n"
	if got != want {
		t.Fatalf("FormatRoadmap() = %q, want %q", got, want)
	}
}

func TestRenderSpec(t *testing.T) {
	reqs := []protocol.BlueprintRequirement{
		{ID: "REQ-1", Text: "Must do X"},
	}
	acc := []protocol.BlueprintRequirement{
		{ID: "ACC-1", Text: "Test Y passes"},
	}
	out := RenderSpec("widget", 2, "Core logic", "Build core", reqs, acc, "Use Go 1.22+")
	if !strings.Contains(out, "### [BLUEPRINT] widget - Phase Spec: Phase 2 - Core logic") {
		t.Fatalf("spec header missing: %s", out)
	}
	if !strings.Contains(out, "Phase 1 signed off.") {
		t.Fatalf("prior phase signoff notice missing: %s", out)
	}
	if !strings.Contains(out, "- [ ] REQ-1: Must do X") {
		t.Fatalf("requirement missing: %s", out)
	}
	if !strings.Contains(out, "- ACC-1: Test Y passes") {
		t.Fatalf("acceptance missing: %s", out)
	}
	if !strings.Contains(out, "#### Context & Constraints\nUse Go 1.22+") {
		t.Fatalf("constraints missing: %s", out)
	}
}

func TestRenderDeliverable(t *testing.T) {
	items := []protocol.BlueprintItem{
		{RequirementID: "REQ-1", Summary: "Implemented X"},
	}
	out := RenderDeliverable("widget", 1, items, "all 5 tests passed", "edge case handled", 0)
	if !strings.Contains(out, "### [BLUEPRINT] widget - Deliverable: Phase 1") {
		t.Fatalf("deliverable header missing: %s", out)
	}
	if !strings.Contains(out, "- [x] REQ-1: Implemented X") {
		t.Fatalf("item summary missing: %s", out)
	}
	if !strings.Contains(out, "all 5 tests passed") {
		t.Fatalf("proof missing: %s", out)
	}
	if !strings.Contains(out, "edge case handled") {
		t.Fatalf("notes missing: %s", out)
	}
}

func TestRenderInspection(t *testing.T) {
	signoff := RenderInspection("widget", 1, "signoff", []string{"ACC-1"}, nil, false)
	if !strings.Contains(signoff, "SIGN-OFF GRANTED") || !strings.Contains(signoff, "Phase 1: COMPLETED") {
		t.Fatalf("bad signoff render: %s", signoff)
	}

	punchlist := RenderInspection("widget", 1, "punchlist", nil, []protocol.BlueprintFinding{
		{RequirementID: "REQ-1", Issue: "Missing check", Fix: "Add check"},
	}, true)
	if !strings.Contains(punchlist, "Punch List Issued") || !strings.Contains(punchlist, "Round cap exhausted - blueprint BLOCKED") {
		t.Fatalf("bad punchlist render: %s", punchlist)
	}
}

func TestCutRunesAndTrimMiddle(t *testing.T) {
	s := "hello world"
	if got := CutRunes(s, 5); got != "hello" {
		t.Fatalf("CutRunes = %q, want 'hello'", got)
	}

	longProof := strings.Repeat("line of test output\n", 50)
	marker := func(omitted int) string {
		return "[omitted]"
	}
	trimmed := TrimMiddle(longProof, 100, marker)
	if len(trimmed) > 260 {
		t.Fatalf("TrimMiddle produced %d bytes, expected within budget bounds", len(trimmed))
	}
	if !strings.Contains(trimmed, "[omitted]") {
		t.Fatalf("TrimMiddle did not include marker")
	}
}
