package indexer

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
)

// TestOnDegradedReplaysAnEarlierDegradation pins the ordering the daemon
// actually has: it can only register OnDegraded after MultiWatcher.Start
// returns, while every degradation raised during startup — an exhausted
// inotify budget, a slow mount, an incomplete startup barrier — has already
// fired by then. Without a replay on registration those degradations are
// announced to nobody and the index freezes while health stays green.
func TestOnDegradedReplaysAnEarlierDegradation(t *testing.T) {
	w, err := NewWatcher(&Indexer{rootPath: t.TempDir()}, config.WatchConfig{}, zap.NewNop())
	require.NoError(t, err)

	w.markDegraded("watching is frozen", "log line", errors.New("cause"))
	require.NotEmpty(t, w.DegradedReason())

	var got string
	w.OnDegraded(func(reason string) { got = reason })
	require.Equal(t, "watching is frozen", got,
		"a callback registered after the fact must still learn the watcher is degraded")
}

// TestOnDegradedStaysQuietWhenHealthy guards the other direction: registration
// must not manufacture a degradation notice for a healthy watcher.
func TestOnDegradedStaysQuietWhenHealthy(t *testing.T) {
	w, err := NewWatcher(&Indexer{rootPath: t.TempDir()}, config.WatchConfig{}, zap.NewNop())
	require.NoError(t, err)

	called := false
	w.OnDegraded(func(string) { called = true })
	require.False(t, called)
	require.Empty(t, w.DegradedReason())
}

// TestMarkDegradedFiresOnceAndKeepsTheLatestReason pins the funnel every
// degradation now shares: the operator warning is logged once, but the reason
// readers see tracks the most recent cause.
func TestMarkDegradedFiresOnceAndKeepsTheLatestReason(t *testing.T) {
	w, err := NewWatcher(&Indexer{rootPath: t.TempDir()}, config.WatchConfig{}, zap.NewNop())
	require.NoError(t, err)

	notices := 0
	w.OnDegraded(func(string) { notices++ })

	require.True(t, w.markDegraded("first", "first log"))
	require.False(t, w.markDegraded("second", "second log"))
	require.Equal(t, 1, notices, "the operator notice is warn-once")
	require.Equal(t, "second", w.DegradedReason())
}
