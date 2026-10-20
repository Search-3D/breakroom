package daemon

// This file renders handoffs: what the architect relays, verbatim, to the
// human at each checkpoint. Every handoff ends with one clear next step.

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/search-3d/breakroom/internal/store"
)

// relPath shows p relative to the main checkout when that is shorter.
func relPath(mainCheckout, p string) string {
	rel, err := filepath.Rel(mainCheckout, p)
	if err != nil || len(rel) >= len(p) {
		return p
	}
	return rel
}

// short abbreviates a commit hash to 7 characters.
func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// handoffRoadmap asks the human to approve or change a roadmap.
func handoffRoadmap(name string, version int) string {
	return fmt.Sprintf("Please review the roadmap for `%s` (v%d) above.\n"+
		"Reply approved to start, or tell me what to change.", name, version)
}

// handoffSeries tells the human where the new series worktree and branch are.
// initialFiles is how many files Breakroom's own initial commit took (0 when
// the repo already had commits).
func handoffSeries(bp *store.Blueprint, mainCheckout string, mainDirty bool, initialFiles int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Series `%s` is set up.\n\n", bp.Name)
	fmt.Fprintf(&b, "- Worktree: `%s`\n", relPath(mainCheckout, bp.WorktreePath))
	fmt.Fprintf(&b, "- Branch: `%s` (from %s)\n", bp.Branch, short(bp.BaseRef))
	if initialFiles > 0 {
		fmt.Fprintf(&b, "\nNote: the folder had no commits, so Breakroom made an initial commit of %d file(s) (dependency folders and likely secrets such as .env left out). Check it with `git show --stat`.\n", initialFiles)
	}
	if mainDirty {
		b.WriteString("\nNote: your main checkout has uncommitted changes; they are not part of this series.\n")
	}
	if bp.Builder == "" {
		fmt.Fprintf(&b, "\nNext step: paste this into your builder's session:\n\n    Join Breakroom as the builder for `%s`.\n", bp.Name)
	} else {
		fmt.Fprintf(&b, "\nYour builder (%s) will be told to switch into the worktree with the phase 1 spec. Phase 1 starts next; nothing is needed from you until the work is ready for review.\n", bp.Builder)
	}
	return b.String()
}

// handoffAcceptance asks the human to review finished work and says where it is.
func handoffAcceptance(bp *store.Blueprint, mainCheckout string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "`%s` is finished and ready for your review (%d phase(s) signed off).\n\n", bp.Name, bp.CurrentPhase)
	if bp.WorktreePath != "" {
		rel := relPath(mainCheckout, bp.WorktreePath)
		fmt.Fprintf(&b, "- Worktree: `%s`\n- Branch: `%s`\n- All changes: `git -C %s diff %s`\n\n", rel, bp.Branch, rel, short(bp.BaseRef))
		fmt.Fprintf(&b, "Next step: please review the code in `%s` - read the diff, or run it in your dev environment.\n", rel)
	} else {
		fmt.Fprintf(&b, "Next step: please review the builder's changes (%s's worktree) - read the diff, or run it in your dev environment.\n", bp.Builder)
	}
	b.WriteString("Reply accept, or describe what needs fixing.")
	return b.String()
}

// handoffLandingChoices asks the human how to land accepted work.
func handoffLandingChoices(bp *store.Blueprint) string {
	branch := bp.Branch
	if branch == "" {
		branch = "the builder's branch"
	} else {
		branch = "`" + branch + "`"
	}
	return fmt.Sprintf("Accepted. How would you like to land `%s`?\n\n"+
		"1. Merge into your main checkout - merge %s into the branch checked out there.\n"+
		"2. Commit and push as a branch - keep %s, pushed to the remote or local only.\n"+
		"3. Open a pull request - commit, push, and open a PR for review.\n"+
		"4. Let you handle landing - close this blueprint out and disconnect the agents.\n\n"+
		"Reply 1, 2, 3 or 4 (for 2, say whether to push).", bp.Name, branch, branch)
}

// handoffFollowUp tells the human their requested changes became a new phase.
func handoffFollowUp(name string, phase int) string {
	return fmt.Sprintf("Got it. I've added phase %d to `%s` to cover your notes. The builder will work on it, I'll inspect it, "+
		"and I'll bring the work back to you for review. Nothing else is needed from you until then.", phase, name)
}

// handoffLanded reports the landing to the human, who is asked what's next;
// removeErr is a failed worktree removal.
func handoffLanded(bp *store.Blueprint, mainCheckout string, removeErr error) string {
	if bp.LandingMethod == "manual" {
		return landedFacts(bp, mainCheckout, removeErr) +
			fmt.Sprintf("\n`%s` is closed and the agents have been disconnected.", bp.Name)
	}
	return landedFacts(bp, mainCheckout, removeErr) +
		fmt.Sprintf("\n`%s` is closed. Want to start the next series? Tell me what to build next.", bp.Name)
}

// landedFacts is the landing without the question, for the builder.
func landedFacts(bp *store.Blueprint, mainCheckout string, removeErr error) string {
	if bp.LandingMethod == "manual" {
		if bp.WorktreePath != "" {
			return fmt.Sprintf("Closed `%s` without landing it. ", bp.Name) + seriesLeftovers(bp, mainCheckout) + "\n"
		}
		return fmt.Sprintf("Closed `%s` without landing it.\n", bp.Name)
	}
	var b strings.Builder
	switch bp.LandingMethod {
	case "merge":
		fmt.Fprintf(&b, "Landed: `%s` is merged into your main checkout", bp.Branch)
	case "branch":
		ref := bp.LandingRef
		if ref == "" {
			ref = bp.Branch
		}
		fmt.Fprintf(&b, "Landed: the work is on branch `%s`", ref)
	case "pr":
		b.WriteString("Landed: the pull request is open")
	}
	if bp.LandingRef != "" && bp.LandingMethod != "branch" {
		fmt.Fprintf(&b, " (%s)", bp.LandingRef)
	}
	b.WriteString(".\n")
	if removeErr != nil {
		rel := relPath(mainCheckout, bp.WorktreePath)
		fmt.Fprintf(&b, "The series worktree `%s` could not be removed (%v); remove it with `git worktree remove %s`. Branch `%s` is kept.\n",
			rel, removeErr, rel, bp.Branch)
	} else if bp.WorktreePath != "" {
		fmt.Fprintf(&b, "The series worktree has been removed; branch `%s` is kept.\n", bp.Branch)
	}
	return b.String()
}

// seriesLeftovers tells the human where unlanded series work still is and how
// to discard it.
func seriesLeftovers(bp *store.Blueprint, mainCheckout string) string {
	rel := relPath(mainCheckout, bp.WorktreePath)
	return fmt.Sprintf("The work is still there:\n\n- Worktree: `%s`\n- Branch: `%s`\n\n"+
		"Next step: review or keep it, or discard it with `git worktree remove %s` (add `--force` if it has changes) and `git branch -D %s`.",
		rel, bp.Branch, rel, bp.Branch)
}

// handoffAbandoned tells the human a blueprint closed without landing.
func handoffAbandoned(bp *store.Blueprint, mainCheckout string) string {
	return fmt.Sprintf("Closed `%s` without landing it. ", bp.Name) + seriesLeftovers(bp, mainCheckout)
}

// handoffBlocked tells the human a blueprint closed after running out of
// punch-list rounds.
func handoffBlocked(bp *store.Blueprint, mainCheckout string) string {
	return fmt.Sprintf("Closed `%s` while blocked at phase %d: the builder and I couldn't get it through inspection within the round limit. ",
		bp.Name, bp.CurrentPhase) + seriesLeftovers(bp, mainCheckout)
}
