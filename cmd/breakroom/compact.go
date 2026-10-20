package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/search-3d/breakroom/internal/paths"
	"github.com/search-3d/breakroom/internal/proxy"
)

// compactCommand is `breakroom compact`: a digest of recent workspace mail.
func compactCommand(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("compact", flag.ContinueOnError)
	limit := fs.Int("limit", 50, "condense at most N recent messages")
	format := fs.String("format", "text", "output format: text or json")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	return runCompact(ctx, *limit, *format)
}

// runCompact asks the daemon for the digest and prints it as text or JSON.
func runCompact(ctx context.Context, limit int, format string) int {
	ws, err := paths.CurrentWorkspace()
	if err != nil {
		return cliErr(err)
	}
	res, err := proxy.NewClient().Compact(ctx, ws, limit)
	if err != nil {
		return cliErr(err)
	}
	if format == "json" {
		printJSON(map[string]any{"digest": res.Digest, "message_count": res.MessageCount, "fallback": res.Fallback})
		return 0
	}
	if res.Digest == "" {
		fmt.Println("(no messages)")
		return 0
	}
	if res.Fallback {
		fmt.Fprintln(os.Stderr, "breakroom: no LLM configured; printing the raw thread")
	}
	fmt.Println(res.Digest)
	return 0
}
