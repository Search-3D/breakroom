package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/search-3d/breakroom/internal/paths"
	"github.com/search-3d/breakroom/internal/proxy"
)

// stringList is a repeatable --path flag.
type stringList []string

// String joins the collected values, for the flag package.
func (s *stringList) String() string { return strings.Join(*s, ",") }

// Set adds one value each time the flag is given.
func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

// diffCommand is `breakroom diff`: show an agent's uncommitted diff.
func diffCommand(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("diff", flag.ContinueOnError)
	agent := fs.String("agent", "", "target agent id (defaults to the sole active agent)")
	var filter stringList
	fs.Var(&filter, "path", "only this file or directory (repeatable)")
	untracked := fs.Bool("untracked", true, "include untracked files (as MCP get_diff does; --untracked=false to skip)")
	summarize := fs.Bool("summarize", false, "summarize the diff via the configured LLM")
	format := fs.String("format", "text", "output format: text or json")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	return runDiff(ctx, *agent, filter, *untracked, *summarize, *format)
}

// runDiff fetches the diff (and optional summary) from the daemon and prints it.
// With no agent given, it uses the only active agent.
func runDiff(ctx context.Context, agent string, filter []string, untracked, summarize bool, format string) int {
	ws, err := paths.CurrentWorkspace()
	if err != nil {
		return cliErr(err)
	}
	client := proxy.NewClient()
	if agent == "" {
		agents, err := client.ListAgents(ctx, ws, "", false)
		if err != nil {
			return cliErr(err)
		}
		if len(agents) != 1 {
			fmt.Fprintf(os.Stderr, "breakroom: pass --agent (%d agents active)\n", len(agents))
			return 1
		}
		agent = agents[0].AgentID
	}
	res, err := client.Diff(ctx, ws, agent, filter, untracked, summarize)
	if err != nil {
		return cliErr(err)
	}
	if format == "json" {
		printJSON(map[string]any{
			"diff": res.Diff, "truncated": res.Truncated,
			"files": res.Files, "summary": res.Summary, "fallback": res.Fallback, "agent": agent,
		})
		return 0
	}
	if summarize && res.Fallback {
		fmt.Fprintln(os.Stderr, "breakroom: no LLM configured; printing the raw diff")
	} else if summarize && res.Summary != "" {
		fmt.Println(res.Summary)
		return 0
	}
	if res.Diff == "" {
		fmt.Println("(clean)")
		return 0
	}
	fmt.Print(res.Diff)
	if !strings.HasSuffix(res.Diff, "\n") {
		fmt.Println()
	}
	return 0
}
