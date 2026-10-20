package render

import (
	"fmt"
	"strings"

	"github.com/search-3d/breakroom/internal/protocol"
)

// FormatMessage formats a single message for terminal or wake-up display.
func FormatMessage(m protocol.Message) string {
	if m.DiffAttachment != "" {
		return fmt.Sprintf("[%s -> %s] (%s, %s)\n%s\n\n#### Diff Attachment\n```diff\n%s```\n\n", m.From, m.To, m.Type, m.CreatedAt, m.Content, m.DiffAttachment)
	}
	return fmt.Sprintf("[%s -> %s] (%s, %s)\n%s\n\n", m.From, m.To, m.Type, m.CreatedAt, m.Content)
}

// FormatMailWakeup formats a list of unread messages into a gate mail wake-up reason.
func FormatMailWakeup(agent string, msgs []protocol.Message) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Breakroom: %d new message(s) for %s.\n\n", len(msgs), agent)
	for _, m := range msgs {
		b.WriteString(FormatMessage(m))
	}
	return b.String()
}
