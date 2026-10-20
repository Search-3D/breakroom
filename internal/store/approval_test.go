package store

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// proposedBlueprint inits "bp" (architect agent-1, builder agent-2) with a
// pending one-phase roadmap v1.
func proposedBlueprint(t *testing.T) (*Store, string) {
	t.Helper()
	ctx := context.Background()
	st := openBlueprintStore(t)
	ws := "/ws"
	if err := st.InitBlueprint(ctx, ws, "bp", "agent-1", "agent-2", "goal"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ProposeRoadmap(ctx, ws, "bp", []RoadmapPhase{{Phase: 1, Title: "phase"}}, "", ""); err != nil {
		t.Fatal(err)
	}
	return st, ws
}

// chat builds a chat approval of "bp" quoting quote.
func chat(ws string, version int, caller, quote string) ApproveParams {
	return ApproveParams{WorkspaceRoot: ws, Name: "bp", Version: version, Via: "chat", Caller: caller, Quote: quote}
}

// record records a prompt and reports whether it was stored.
func record(t *testing.T, st *Store, ws, agent, text string) bool {
	t.Helper()
	ok, err := st.RecordPrompt(context.Background(), ws, agent, text)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

func TestApproveChatHappyPath(t *testing.T) {
	st, ws := proposedBlueprint(t)
	if !record(t, st, ws, "agent-1", "Looks great — approved") {
		t.Fatal("prompt not recorded")
	}
	rm, err := st.ApproveRoadmap(context.Background(), chat(ws, 1, "agent-1", "approved"))
	if err != nil {
		t.Fatal(err)
	}
	if rm.ApprovedVia != "chat" || rm.ApprovalQuote != "approved" || rm.ApprovedAt == nil {
		t.Fatalf("roadmap = %+v", rm)
	}
	if err := st.SpecPhase(context.Background(), testSpec(ws, "bp", 1)); err != nil {
		t.Fatalf("spec after chat approval: %v", err)
	}
}

func TestApproveRejects(t *testing.T) {
	ctx := context.Background()

	st, ws := proposedBlueprint(t)
	if _, err := st.ApproveRoadmap(ctx, chat(ws, 1, "agent-1", "approved")); !errors.Is(err, ErrHumanApprovalRequired) || !strings.Contains(err.Error(), "no prompt recorded for agent-1") {
		t.Fatalf("no prompt = %v", err)
	}

	// A prompt typed before the proposal does not count.
	st2 := openBlueprintStore(t)
	if err := st2.InitBlueprint(ctx, ws, "bp", "agent-1", "agent-2", ""); err != nil {
		t.Fatal(err)
	}
	record(t, st2, ws, "agent-1", "draft it, approved in advance")
	if _, err := st2.ProposeRoadmap(ctx, ws, "bp", []RoadmapPhase{{Phase: 1, Title: "phase"}}, "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st2.ApproveRoadmap(ctx, chat(ws, 1, "agent-1", "approved")); !errors.Is(err, ErrHumanApprovalRequired) || !strings.Contains(err.Error(), "since the request was proposed") {
		t.Fatalf("prompt before proposal = %v", err)
	}

	record(t, st, ws, "agent-1", "approved")
	for _, q := range []string{"ship it", "a", "  "} {
		t.Run("quote "+q, func(t *testing.T) {
			if _, err := st.ApproveRoadmap(ctx, chat(ws, 1, "agent-1", q)); !errors.Is(err, ErrHumanApprovalRequired) {
				t.Fatalf("quote %q = %v", q, err)
			}
		})
	}
	if _, err := st.ApproveRoadmap(ctx, chat(ws, 1, "agent-1", "approved")); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ApproveRoadmap(ctx, chat(ws, 1, "agent-1", "approved")); !errors.Is(err, ErrBadTransition) {
		t.Fatalf("approve twice = %v", err)
	}
	if _, err := st.ProposeRoadmap(ctx, ws, "bp", []RoadmapPhase{{Phase: 1, Title: "phase"}, {Phase: 2, Title: "b"}}, "", ""); err != nil {
		t.Fatal(err)
	}
	record(t, st, ws, "agent-1", "approved")
	if _, err := st.ApproveRoadmap(ctx, chat(ws, 1, "agent-1", "approved")); !errors.Is(err, ErrBadTransition) {
		t.Fatalf("approve superseded = %v", err)
	}
}

func TestApprovePromptMatching(t *testing.T) {
	for _, tc := range []struct {
		name        string
		agent, text string
		quote       string
		wantErr     error
	}{
		{"other session rejected", "agent-2", "approved", "approved", ErrHumanApprovalRequired},
		{"quote normalized", "agent-1", "Approved!   looks GOOD", "approved! looks good", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, ws := proposedBlueprint(t)
			record(t, st, ws, tc.agent, tc.text)
			_, err := st.ApproveRoadmap(context.Background(), chat(ws, 1, "agent-1", tc.quote))
			if tc.wantErr == nil && err != nil {
				t.Fatal(err)
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestApproveCLI(t *testing.T) {
	ctx := context.Background()
	st, ws := proposedBlueprint(t)
	rm, err := st.ApproveRoadmap(ctx, ApproveParams{WorkspaceRoot: ws, Name: "bp", Via: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	if rm.Version != 1 || rm.ApprovedVia != "cli" || rm.ApprovalQuote != "" {
		t.Fatalf("roadmap = %+v", rm)
	}
	if _, err := st.ApproveRoadmap(ctx, ApproveParams{WorkspaceRoot: ws, Name: "bp", Via: "cli"}); !errors.Is(err, ErrBadTransition) {
		t.Fatalf("nothing pending = %v", err)
	}
}

func TestRecordPromptSelfEcho(t *testing.T) {
	ctx := context.Background()
	st, ws := proposedBlueprint(t)
	if err := st.RecordEmission(ctx, ws, "agent-1", "  Breakroom: 1 new message, approved "); err != nil {
		t.Fatal(err)
	}
	if record(t, st, ws, "agent-1", "Breakroom: 1 new message, approved") {
		t.Fatal("echoed continuation was recorded as a human prompt")
	}
	// A host that rewraps the text or changes line endings still echoes.
	if record(t, st, ws, "agent-1", "Breakroom: 1 new\r\nmessage,  approved\n") {
		t.Fatal("re-wrapped continuation was recorded as a human prompt")
	}
	if _, err := st.ApproveRoadmap(ctx, chat(ws, 1, "agent-1", "approved")); !errors.Is(err, ErrHumanApprovalRequired) {
		t.Fatalf("approval from echo = %v", err)
	}
}

func TestRecordPromptPrunes(t *testing.T) {
	st, ws := proposedBlueprint(t)
	for i := range 55 {
		record(t, st, ws, "agent-1", strings.Repeat("x", i+1))
	}
	var n, minLen int
	if err := st.db.QueryRow(`SELECT COUNT(*), MIN(length(text)) FROM human_prompts WHERE agent='agent-1'`).Scan(&n, &minLen); err != nil {
		t.Fatal(err)
	}
	if n != 50 || minLen != 6 {
		t.Fatalf("kept %d prompts, shortest %d; want 50, 6", n, minLen)
	}
}

func TestRecordPromptTruncates(t *testing.T) {
	st, ws := proposedBlueprint(t)
	if !record(t, st, ws, "agent-1", strings.Repeat("y", 200000)) {
		t.Fatal("big prompt not recorded")
	}
	var n int
	if err := st.db.QueryRow(`SELECT length(text) FROM human_prompts`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n > 65536 {
		t.Fatalf("stored %d bytes, want <= 65536", n)
	}
}

// A long paste with the approval at either end still backs the quote.
func TestApproveQuoteAroundLongPaste(t *testing.T) {
	filler := strings.Repeat("log line\n", 25000)
	for _, tc := range []struct{ name, prompt string }{
		{"approval after paste", filler + "roadmap approved"},
		{"approval before paste", "roadmap approved\n" + filler},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, ws := proposedBlueprint(t)
			record(t, st, ws, "agent-1", tc.prompt)
			if _, err := st.ApproveRoadmap(context.Background(), chat(ws, 1, "agent-1", "roadmap approved")); err != nil {
				t.Fatalf("approval at %d-byte paste edge: %v", len(tc.prompt), err)
			}
		})
	}
}

// Quotes match whole words outside negated clauses; a one-character quote
// must be its whole clause (the landing choice "1").
func TestQuoteBacked(t *testing.T) {
	for _, c := range []struct {
		prompt, quote string
		want          bool
	}{
		{"no, don't accept this yet, the tests fail", "accept", false},
		{"this is not ok", "ok", false},
		{"I don't think we should accept", "accept", false},
		{"looks good but don't merge", "looks good", false},
		{"it looks okay to me", "ok", false},
		{"good, go", "go", true},
		{"yes, accept it", "accept", true},
		{"Looks great — approved", "approved", true},
		{"accept, no changes needed", "accept", true},
		{"1", "1", true},
		{"1, and push it", "1", true},
		{"phase 1 looks wrong", "1", false},
		{"👍", "👍", true},
		{"Approved!   looks GOOD", "approved! looks good", true},
	} {
		t.Run(c.quote, func(t *testing.T) {
			if got := quoteBacked(c.prompt, splitWords(c.quote)); got != c.want {
				t.Errorf("quoteBacked(%q, %q) = %v, want %v", c.prompt, c.quote, got, c.want)
			}
		})
	}
}
