package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/search-3d/breakroom/internal/hosts"
	"github.com/search-3d/breakroom/internal/paths"
	"github.com/search-3d/breakroom/internal/proxy"
	"github.com/search-3d/breakroom/internal/render"
)

// hookInput is the slice of host hook stdin the gate reads. Claude Code,
// Codex, VS Code, Muse Code, and Copilot all send these snake_case fields;
// Antigravity sends camelCase workspacePaths and transcriptPath instead of cwd,
// Cursor sends workspace_roots, and Cursor's stop hook adds a status.
type hookInput struct {
	Cwd                 string   `json:"cwd"`
	Prompt              string   `json:"prompt"`
	StopHookActive      bool     `json:"stop_hook_active"`
	ExecutionNum        int      `json:"executionNum"`
	Status              string   `json:"status"`
	WorkspacePaths      []string `json:"workspacePaths"`
	WorkspaceRoots      []string `json:"workspace_roots"`
	WorkspaceRootsCamel []string `json:"workspaceRoots"`
	TranscriptPath      string   `json:"transcriptPath"`
}

// extractTranscriptPrompt extracts the most recent human prompt from an Antigravity transcript JSONL file.
func extractTranscriptPrompt(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	var lastContent string
	scanner := bufio.NewScanner(f)
	const maxLine = 1024 * 1024
	buf := make([]byte, maxLine)
	scanner.Buffer(buf, maxLine)

	for scanner.Scan() {
		line := scanner.Bytes()
		if !bytes.Contains(line, []byte(`"USER_INPUT"`)) {
			continue
		}
		var entry struct {
			Type    string `json:"type"`
			Content string `json:"content"`
		}
		if err := json.Unmarshal(line, &entry); err == nil && entry.Type == "USER_INPUT" {
			lastContent = entry.Content
		}
	}
	if scanner.Err() != nil {
		return ""
	}
	return cleanTranscriptPrompt(lastContent)
}

// cleanTranscriptPrompt strips Antigravity wrapper tags (<USER_REQUEST>, <ADDITIONAL_METADATA>) from prompt content.
func cleanTranscriptPrompt(content string) string {
	content = strings.TrimSpace(content)
	if content == "" {
		return ""
	}
	if start := strings.Index(content, "<USER_REQUEST>"); start != -1 {
		content = content[start+len("<USER_REQUEST>"):]
	}
	if end := strings.Index(content, "</USER_REQUEST>"); end != -1 {
		content = content[:end]
	}
	if meta := strings.Index(content, "<ADDITIONAL_METADATA>"); meta != -1 {
		content = content[:meta]
	}
	return strings.TrimSpace(content)
}

// gateCommand runs as a host hook. Configuration mistakes (unknown event or
// host) exit 2 so the human sees them; every runtime failure exits 0 with
// empty stdout so the host never breaks.
func gateCommand(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("gate", flag.ContinueOnError)
	event := fs.String("event", "", "hook event: stop or prompt")
	host := fs.String("host", "claude", "hook output format: "+hosts.SupportedHostsString())
	hold := fs.Int("hold", 540, "stop: max seconds to hold while waiting on a blueprint peer (keep under the host's hook timeout)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	target := hosts.Find(*host)
	if (*event != "stop" && *event != "prompt") || target == nil {
		fmt.Fprintf(os.Stderr, "breakroom gate: --event must be stop or prompt; --host must be %s\n", hosts.SupportedHostsString())
		return 2
	}
	*host = target.ID
	var in hookInput
	if data, err := io.ReadAll(os.Stdin); err == nil {
		_ = json.Unmarshal(data, &in) // garbage stdin: zero input, still fail-open
	}
	if in.Cwd == "" && len(in.WorkspacePaths) > 0 {
		in.Cwd = in.WorkspacePaths[0]
	}
	if in.Cwd == "" && len(in.WorkspaceRoots) > 0 {
		in.Cwd = in.WorkspaceRoots[0]
	}
	if in.Cwd == "" && len(in.WorkspaceRootsCamel) > 0 {
		in.Cwd = in.WorkspaceRootsCamel[0]
	}
	if in.Cwd != "" {
		if err := os.Chdir(in.Cwd); err != nil {
			return 0
		}
	}
	cwd, err := os.Getwd()
	if err != nil {
		return 0
	}
	ws := paths.ResolveWorkspace(cwd)
	c := proxy.NewClient()
	if *event == "prompt" {
		if in.Prompt == "" && in.TranscriptPath != "" {
			in.Prompt = extractTranscriptPrompt(in.TranscriptPath)
		}
		if in.Prompt == "" {
			if *host == "antigravity" {
				fmt.Println("{}")
			}
			if *host == "cursor" {
				fmt.Println(`{"continue":true}`)
			}
			return 0
		}
		// agent is "" when the daemon could only guess whose prompt this was:
		// mail is then never delivered here.
		agent, _, err := c.RecordPrompt(ctx, ws, cwd, *host, in.Prompt, hostAncestors(ctx))
		if *host == "cursor" {
			fmt.Println(`{"continue":true}`)
			return 0
		}
		if *host == "antigravity" {
			if err == nil && agent != "" {
				if msgs, resolved, peekErr := c.Peek(ctx, ws, agent, cwd, hostAncestors(ctx)); peekErr == nil && len(msgs) > 0 {
					ids := make([]int64, 0, len(msgs))
					var b strings.Builder
					fmt.Fprintf(&b, "Breakroom: %d new message(s) for %s.\n\n", len(msgs), resolved)
					for _, m := range msgs {
						ids = append(ids, m.ID)
						b.WriteString(render.FormatMessage(m))
					}
					out := map[string]any{
						"injectSteps": []map[string]any{
							{"ephemeralMessage": b.String()},
						},
					}
					if json.NewEncoder(os.Stdout).Encode(out) == nil {
						_ = c.Ack(ctx, ws, resolved, ids)
					}
					return 0
				}
			}
			fmt.Println("{}")
			return 0
		}
		if err != nil || agent == "" || target.PromptContext != hosts.PromptContextText {
			return 0
		}
		return runCheck(ctx, true, "UserPromptSubmit", "text", agent)
	}
	stopActive := in.StopHookActive
	if *host == "antigravity" && in.ExecutionNum > 1 {
		stopActive = true
	}
	d, err := c.GateStop(ctx, ws, cwd, *host, in.Status, hostAncestors(ctx), stopActive, *hold)
	if err != nil || d.Action != "block" {
		// Antigravity requires a decision; anything but "continue" allows the stop.
		if *host == "antigravity" {
			fmt.Println(`{"decision":"stop"}`)
		}
		return 0
	}
	out := d.HostPayload
	if out == nil {
		out = map[string]string{"decision": "block", "reason": d.Reason}
	}
	if err := json.NewEncoder(os.Stdout).Encode(out); err != nil {
		return 0
	}
	// Ack only after the reason is out: a hook killed before this line
	// redelivers the mail instead of losing it. Muse leaves mail unread for
	// the next MCP tool call.
	if d.Kind == "mail" && !d.SuppressAck {
		_ = c.Ack(ctx, ws, d.Agent, d.MessageIDs)
	}
	return 0
}

// blueprintCommand is the human's side of blueprints, from their own
// terminal: approve a roadmap, accept finished work or ask for changes, and
// record how it landed.
func blueprintCommand(ctx context.Context, args []string) int {
	const usage = "usage: breakroom blueprint approve|accept|reject|land <name> [flags]"
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, usage)
		return 2
	}
	switch args[0] {
	case "approve":
		return blueprintApprove(ctx, args[1:])
	case "accept", "reject":
		return blueprintVerdict(ctx, args[0], args[1:])
	case "land":
		return blueprintLand(ctx, args[1:])
	}
	fmt.Fprintln(os.Stderr, usage)
	return 2
}

// parseNamed parses "<name> [flags]" with flags before or after the name.
func parseNamed(fs *flag.FlagSet, args []string) (string, bool) {
	if err := fs.Parse(args); err != nil {
		return "", false
	}
	name := fs.Arg(0)
	if err := fs.Parse(fs.Args()[min(1, fs.NArg()):]); err != nil {
		return "", false
	}
	return name, name != "" && fs.NArg() == 0
}

// cliWorkspace resolves the workspace from the human's cwd.
func cliWorkspace() (string, bool) {
	ws, err := paths.CurrentWorkspace()
	if err != nil {
		cliErr(err)
		return "", false
	}
	return ws, true
}

// blueprintApprove is `breakroom blueprint approve`: the human approves a roadmap
// from their own terminal. It prints the roadmap before approving it.
func blueprintApprove(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("blueprint approve", flag.ContinueOnError)
	version := fs.Int("version", 0, "roadmap version to approve (default: the latest pending)")
	name, ok := parseNamed(fs, args)
	if !ok {
		fmt.Fprintln(os.Stderr, "usage: breakroom blueprint approve <name> [--version N]")
		return 2
	}
	ws, ok := cliWorkspace()
	if !ok {
		return 1
	}
	c := proxy.NewClient()
	bp, err := c.GetBlueprint(ctx, ws, name, 0)
	if err != nil {
		return cliErr(err)
	}
	// Print the roadmap being approved; the broker rejects anything else.
	if n := len(bp.Roadmaps); n > 0 && (*version == 0 || *version == n) {
		rm := bp.Roadmaps[n-1]
		fmt.Print(render.FormatRoadmap(rm))
	}
	rm, err := c.ApproveRoadmap(ctx, ws, name, "", *version, "cli", "")
	if err != nil {
		return cliErr(err)
	}
	fmt.Printf("approved v%d\n", rm.Version)
	if rm.Handoff != "" {
		fmt.Printf("\n%s\n", rm.Handoff)
	}
	if rm.SeriesError != "" {
		fmt.Fprintf(os.Stderr, "breakroom: series not set up: %s\n", rm.SeriesError)
	}
	return 0
}

// blueprintVerdict records "accept" or "reject" (changes, with --notes).
func blueprintVerdict(ctx context.Context, cmd string, args []string) int {
	fs := flag.NewFlagSet("blueprint "+cmd, flag.ContinueOnError)
	var notes *string
	if cmd == "reject" {
		notes = fs.String("notes", "", "what needs fixing (becomes the follow-up phase)")
	}
	name, ok := parseNamed(fs, args)
	if !ok || (cmd == "reject" && strings.TrimSpace(*notes) == "") {
		if cmd == "accept" {
			fmt.Fprintln(os.Stderr, "usage: breakroom blueprint accept <name>")
		} else {
			fmt.Fprintln(os.Stderr, "usage: breakroom blueprint reject <name> --notes TEXT")
		}
		return 2
	}
	ws, ok := cliWorkspace()
	if !ok {
		return 1
	}
	verdict := "accept"
	var notesVal string
	if cmd == "reject" {
		verdict = "changes"
		notesVal = *notes
	}
	res, err := proxy.NewClient().RecordVerdict(ctx, ws, name, "", verdict, "cli", "", notesVal, "")
	if err != nil {
		return cliErr(err)
	}
	fmt.Println(res.Handoff)
	return 0
}

// blueprintLand records how accepted work landed, from the human's terminal.
func blueprintLand(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("blueprint land", flag.ContinueOnError)
	method := fs.String("method", "", "merge, branch, pr, or manual")
	ref := fs.String("ref", "", "merge commit, branch name, or PR URL")
	name, ok := parseNamed(fs, args)
	validMethod := *method == "merge" || *method == "branch" || *method == "pr" || *method == "manual"
	if !ok || !validMethod {
		fmt.Fprintln(os.Stderr, "usage: breakroom blueprint land <name> --method merge|branch|pr|manual [--ref REF]")
		return 2
	}
	ws, ok := cliWorkspace()
	if !ok {
		return 1
	}
	handoff, err := proxy.NewClient().Land(ctx, ws, name, "", *method, *ref, "cli", "")
	if err != nil {
		return cliErr(err)
	}
	fmt.Println(handoff)
	return 0
}
