package render

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/search-3d/breakroom/internal/protocol"
)

// DefaultMailMax caps blueprint notifications to fit model context.
const DefaultMailMax = 8 << 10

// CutRunes returns s[:n] backed off to a rune boundary (n < len(s)).
func CutRunes(s string, n int) string {
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// TrimMiddle keeps s's first quarter and last three quarters of budget,
// split on line boundaries: test logs end with the totals and failures.
func TrimMiddle(s string, budget int, marker func(omitted int) string) string {
	const markerMax = 160
	if len(s) <= budget {
		return s
	}
	keep := max(budget-markerMax, 0)
	head := CutRunes(s, keep/4)
	if i := strings.LastIndexByte(head, '\n'); i >= 0 {
		head = head[:i+1]
	}
	start := len(s) - (keep - keep/4)
	for start < len(s) && !utf8.RuneStart(s[start]) {
		start++
	}
	tail := s[start:]
	if i := strings.IndexByte(tail, '\n'); i >= 0 && i < len(tail)-1 {
		tail = tail[i+1:]
	}
	omitted := len(s) - len(head) - len(tail)
	return head + marker(omitted) + tail
}

// FormatRoadmap formats a roadmap for terminal or notification display.
func FormatRoadmap(rm protocol.Roadmap) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Roadmap v%d:\n", rm.Version)
	for _, p := range rm.Phases {
		line := "  " + strconv.Itoa(p.Phase) + ". " + p.Title
		if p.Delivers != "" {
			line += ": " + p.Delivers
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	if rm.OutOfScope != "" {
		fmt.Fprintf(&b, "Out of scope: %s\n", rm.OutOfScope)
	}
	return b.String()
}

// RenderSpec renders a phase specification in markdown.
func RenderSpec(name string, phase int, title, objective string, reqs, acc []protocol.BlueprintRequirement, constraints string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "### [BLUEPRINT] %s - Phase Spec: Phase %d - %s\n\n", name, phase, title)
	if phase > 1 {
		fmt.Fprintf(&b, "Phase %d signed off.\n\n", phase-1)
	}
	fmt.Fprintf(&b, "#### Objective\n%s\n\n#### Scope & Requirements\n", objective)
	for _, r := range reqs {
		fmt.Fprintf(&b, "- [ ] %s: %s\n", r.ID, r.Text)
	}
	fmt.Fprintf(&b, "\n#### Verification & Acceptance Criteria\n")
	for _, a := range acc {
		fmt.Fprintf(&b, "- %s: %s\n", a.ID, a.Text)
	}
	if strings.TrimSpace(constraints) != "" {
		fmt.Fprintf(&b, "\n#### Context & Constraints\n%s\n", constraints)
	}
	return b.String()
}

// RenderDeliverable renders a phase deliverable in markdown, trimming only
// proof when it exceeds maxLen.
func RenderDeliverable(name string, phase int, items []protocol.BlueprintItem, proof, notes string, maxLen int) string {
	if maxLen <= 0 {
		maxLen = DefaultMailMax
	}
	var head, foot strings.Builder
	fmt.Fprintf(&head, "### [BLUEPRINT] %s - Deliverable: Phase %d\n\n#### Completed Items\n", name, phase)
	for _, it := range items {
		fmt.Fprintf(&head, "- [x] %s: %s\n", it.RequirementID, it.Summary)
	}
	head.WriteString("\n#### Verification Proof\n```\n")
	foot.WriteString("\n```\n")
	if strings.TrimSpace(notes) != "" {
		fmt.Fprintf(&foot, "\n#### Notes / Edge Cases Handled\n%s\n", notes)
	}
	foot.WriteString("\n*Ready for inspection.*\n")
	budget := maxLen - head.Len() - foot.Len()
	return head.String() + TrimMiddle(proof, budget, func(omitted int) string {
		return fmt.Sprintf("[… %d bytes omitted - blueprint_status name=%s phase=%d has the full proof]\n", omitted, name, phase)
	}) + foot.String()
}

// RenderInspection renders an inspection verdict (sign-off or punch list) in markdown.
func RenderInspection(name string, phase int, verdict string, accIDs []string, findings []protocol.BlueprintFinding, blocked bool) string {
	var b strings.Builder
	if verdict == "signoff" {
		fmt.Fprintf(&b, "### [BLUEPRINT] %s - Inspection: Phase %d - SIGN-OFF GRANTED\n\n#### Verdict\nAll acceptance criteria verified and passed: %s.\n\n#### Ledger Update\n- Phase %d: COMPLETED\n", name, phase, strings.Join(accIDs, ", "), phase)
		return b.String()
	}
	fmt.Fprintf(&b, "### [BLUEPRINT] %s - Inspection: Phase %d - Punch List Issued\n\n#### Findings\nThe phase deliverable does not yet meet acceptance criteria:\n", name, phase)
	for i, f := range findings {
		fmt.Fprintf(&b, "%d. **%s:** %s\n   - *Required Fix:* %s\n", i+1, f.RequirementID, f.Issue, f.Fix)
	}
	if blocked {
		fmt.Fprintf(&b, "\n**Round cap exhausted - blueprint BLOCKED, awaiting human triage.**\n")
	} else {
		fmt.Fprintf(&b, "\n*Builder: Address these punch list items and re-submit for inspection.*\n")
	}
	return b.String()
}
