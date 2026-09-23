package agent

import (
	"os"
	"testing"

	"github.com/tegarthegreat/agentium/internal/sandbox"
)

// TestMain lets this test binary act as the sandbox helper when bash
// commands re-execute it.
func TestMain(m *testing.M) {
	sandbox.MaybeRunHelper()
	os.Exit(m.Run())
}
