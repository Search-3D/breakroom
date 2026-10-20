package daemon

import (
	"os"
	"testing"
)

// TestMain pins BREAKROOM_HOME to a scratch dir so handler tests (which
// resolve config per request) never read the developer's real home.
// Tests that need their own home use t.Setenv to override.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "br-daemon-test-")
	if err != nil {
		os.Exit(1)
	}
	_ = os.Setenv("BREAKROOM_HOME", dir)
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
