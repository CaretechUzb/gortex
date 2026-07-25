package mcp

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestAnalysisRetryHintWithoutAPriorPass(t *testing.T) {
	var state analysisRunState
	require.Equal(t, analysisRetryUnknown, state.retryHint(),
		"with no completed pass to extrapolate from the hint must be the unknown default")
}

func TestAnalysisRetryHintCountsDownThePass(t *testing.T) {
	var state analysisRunState
	state.lastTook.Store(int64(40 * time.Second))
	state.startedAt.Store(time.Now().Add(-10 * time.Second).UnixNano())

	hint := state.retryHint()
	require.Less(t, hint, 40*time.Second, "the hint must shrink as the pass progresses")
	require.Greater(t, hint, analysisRetryFloor, "30s of a 40s pass remains, well above the floor")
}

func TestAnalysisRetryHintIsClamped(t *testing.T) {
	// A pass that already overran its previous runtime should land soon.
	var overrun analysisRunState
	overrun.lastTook.Store(int64(40 * time.Second))
	overrun.startedAt.Store(time.Now().Add(-2 * time.Minute).UnixNano())
	require.Equal(t, analysisRetryFloor, overrun.retryHint())

	// A pass far longer than any tool budget still yields one actionable
	// wait; the caller simply comes back more than once.
	var long analysisRunState
	long.lastTook.Store(int64(6 * time.Minute))
	require.Equal(t, analysisRetryCeiling, long.retryHint(),
		"a hint must never exceed a plausible tool budget")
}

func TestAnalysisPendingPayloadKeepsResponseShape(t *testing.T) {
	payload := analysisPendingPayload(
		AnalysisAvailability{Running: true, RetryAfter: 25 * time.Second}, "communities")

	// A caller that just iterates the collection must not hit a decode
	// error or a missing key while the pass runs.
	collection, ok := payload["communities"].([]any)
	require.True(t, ok, "the collection key must be present and correctly typed")
	require.Empty(t, collection)

	require.Equal(t, "analysis_pending", payload["status"])
	require.Equal(t, 25, payload["retry_after_seconds"])
	require.Contains(t, payload["message"], "retry this call in ~25s")
	require.NotContains(t, payload["message"], "index_repository",
		"a populated graph must not be told to re-index")
}

func TestAnalysisAvailabilityMessageIsEmptyWhenReady(t *testing.T) {
	require.Empty(t, AnalysisAvailability{Ready: true}.Message())
}
