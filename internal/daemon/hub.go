package daemon

import "sync"

// hub wakes wait_for_message holders when mail arrives. SQLite holds the
// messages; the hub carries only wakeups.
type hub struct {
	mu      sync.Mutex
	waiters map[string]map[chan struct{}]struct{}
}

// newHub returns an empty hub.
func newHub() *hub {
	return &hub{waiters: map[string]map[chan struct{}]struct{}{}}
}

// hubKey identifies one agent's inbox in one workspace.
func hubKey(workspace, recipient string) string {
	return workspace + "\x00" + recipient
}

// wait subscribes to wakeups for key. The channel (buffer 1, bursts
// coalesce) fires on the next signal; call cancel to unsubscribe.
func (h *hub) wait(key string) (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	h.mu.Lock()
	set := h.waiters[key]
	if set == nil {
		set = map[chan struct{}]struct{}{}
		h.waiters[key] = set
	}
	set[ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		delete(h.waiters[key], ch)
		if len(h.waiters[key]) == 0 {
			delete(h.waiters, key)
		}
	}
}

// signal wakes all waiters on key.
func (h *hub) signal(key string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.waiters[key] {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}
