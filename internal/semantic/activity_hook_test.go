package semantic

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// The hook is level-triggered: every call carries the complete set of repos
// with a pass in flight, so a dropped notification is repaired by the next one
// rather than leaving a reader permanently wrong about which repos are busy.
func TestEnrichmentActivityHookReportsTheWholeRunningSet(t *testing.T) {
	m := NewManager(Config{}, zap.NewNop())
	var got [][]string
	m.SetActivityHook(func(repos []string) { got = append(got, repos) })

	m.setEnrichStatus("repoA", "go-types", "go", EnrichStateRunning, 0, nil, "")
	m.setEnrichStatus("repoB", "python-types", "python", EnrichStateRunning, 0, nil, "")
	m.setEnrichStatus("repoA", "go-types", "go", EnrichStateCompleted, 0, nil, "")

	require.Equal(t, [][]string{{"repoA"}, {"repoA", "repoB"}, {"repoB"}}, got)
}

// A repo with several providers is busy until the last one finishes. Reporting
// it idle after the first provider completes would clear "enriching…" while
// the repo is still, in fact, enriching.
func TestEnrichmentActivityHookKeepsARepoBusyUntilItsLastProviderFinishes(t *testing.T) {
	m := NewManager(Config{}, zap.NewNop())
	var got [][]string
	m.SetActivityHook(func(repos []string) { got = append(got, repos) })

	m.setEnrichStatus("repoA", "go-types", "go", EnrichStateRunning, 0, nil, "")
	m.setEnrichStatus("repoA", "python-types", "python", EnrichStateRunning, 0, nil, "")
	m.setEnrichStatus("repoA", "go-types", "go", EnrichStateCompleted, 0, nil, "")
	require.Equal(t, []string{"repoA"}, got[len(got)-1])

	m.setEnrichStatus("repoA", "python-types", "python", EnrichStateCompleted, 0, nil, "")
	require.Empty(t, got[len(got)-1])
}

// The daemon's hook writes a file, so it must not run while m.mu is held. A
// hook that reads the manager back would then DEADLOCK rather than race —
// which -race cannot see, because nothing races. This test hangs the call in a
// goroutine so the failure is a timeout with a message instead of a stuck
// suite.
func TestEnrichmentActivityHookRunsOutsideTheManagerLock(t *testing.T) {
	m := NewManager(Config{}, zap.NewNop())
	active := make(chan bool, 1)
	m.SetActivityHook(func([]string) { active <- m.EnrichmentActive() })

	done := make(chan struct{})
	go func() {
		defer close(done)
		m.setEnrichStatus("repoA", "go-types", "go", EnrichStateRunning, 0, nil, "")
	}()

	select {
	case <-done:
		require.True(t, <-active, "the hook must see the state that triggered it")
	case <-time.After(10 * time.Second):
		t.Fatal("activity hook deadlocked against the manager lock")
	}
}

// A manager with no hook installed is the common case — every caller outside
// the daemon — and must not pay for the snapshot or trip over a nil call.
func TestSetEnrichStatusIsInertWithoutAHook(t *testing.T) {
	m := NewManager(Config{}, zap.NewNop())
	require.NotPanics(t, func() {
		m.setEnrichStatus("repoA", "go-types", "go", EnrichStateRunning, 0, nil, "")
	})
	require.True(t, m.EnrichmentActive())
}
