package indexer

import (
	"context"
	"os"
	"os/exec"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/reach"
)

const incrementalDerivedLegacyChildEnv = "GORTEX_TEST_INCREMENTAL_DERIVED_LEGACY_CHILD"

type incrementalDerivedProbeStore struct {
	graph.Store
	checkpoints atomic.Int64
}

func (s *incrementalDerivedProbeStore) CheckpointWAL() error {
	s.checkpoints.Add(1)
	return nil
}

func newIncrementalDerivedTestMultiIndexer(store graph.Store) *MultiIndexer {
	return NewMultiIndexer(store, parser.NewRegistry(), nil, nil, zap.NewNop())
}

func incrementalDerivedLegacyPlan() map[string]DerivedInvalidationPlan {
	return map[string]DerivedInvalidationPlan{
		"repo": {
			Files:          []string{"repo/main.go"},
			LegacyFallback: true,
		},
	}
}

func waitForIncrementalDerivedBatchWriter(t *testing.T, mi *MultiIndexer) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if !mi.batchMutationGate.TryRLock() {
			return
		}
		mi.batchMutationGate.RUnlock()
		if time.Now().After(deadline) {
			t.Fatal("incremental derived wrapper did not queue for the batch write gate")
		}
		runtime.Gosched()
	}
}

func TestRunIncrementalDerivedPassesLegacyFallbackDoesNotReenterTopology(t *testing.T) {
	if os.Getenv(incrementalDerivedLegacyChildEnv) == "1" {
		store := &incrementalDerivedProbeStore{Store: graph.New()}
		mi := newIncrementalDerivedTestMultiIndexer(store)
		before := reach.BuildCounter()

		report := mi.RunIncrementalDerivedPasses(context.Background(), incrementalDerivedLegacyPlan())

		require.True(t, report.LegacyFallback)
		require.Equal(t, 1, report.Repos)
		require.Equal(t, 1, report.Files)
		require.Positive(t, store.checkpoints.Load(), "legacy fallback did not enter global passes")
		require.Greater(t, reach.BuildCounter(), before)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0],
		"-test.run=^TestRunIncrementalDerivedPassesLegacyFallbackDoesNotReenterTopology$",
		"-test.count=1",
	)
	cmd.Env = append(os.Environ(), incrementalDerivedLegacyChildEnv+"=1")
	output, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		t.Fatalf("legacy fallback deadlocked while re-entering the topology gate:\n%s", output)
	}
	require.NoErrorf(t, err, "legacy fallback subprocess failed:\n%s", output)
}

func TestRunIncrementalDerivedPassesTakesBatchWriteGate(t *testing.T) {
	mi := newIncrementalDerivedTestMultiIndexer(graph.New())
	plan := map[string]DerivedInvalidationPlan{
		"repo": {Files: []string{"repo/main.go"}},
	}

	mi.batchMutationGate.RLock()
	gateHeld := true
	defer func() {
		if gateHeld {
			mi.batchMutationGate.RUnlock()
		}
	}()

	before := reach.BuildCounter()
	done := make(chan IncrementalDerivedReport, 1)
	go func() {
		done <- mi.RunIncrementalDerivedPasses(context.Background(), plan)
	}()
	waitForIncrementalDerivedBatchWriter(t, mi)
	select {
	case <-done:
		t.Fatal("incremental derived wrapper bypassed the batch write gate")
	default:
	}

	mi.batchMutationGate.RUnlock()
	gateHeld = false
	select {
	case report := <-done:
		require.Equal(t, 1, report.Repos)
		require.Equal(t, 1, report.Files)
		require.Greater(t, reach.BuildCounter(), before)
	case <-time.After(5 * time.Second):
		t.Fatal("incremental derived wrapper did not resume after the batch gate was released")
	}
}

func TestRunIncrementalDerivedPassesCanceledWhileQueuedSkipsTopologyAndGlobalWork(t *testing.T) {
	store := &incrementalDerivedProbeStore{Store: graph.New()}
	mi := newIncrementalDerivedTestMultiIndexer(store)

	mi.batchMutationGate.RLock()
	gateHeld := true
	defer func() {
		if gateHeld {
			mi.batchMutationGate.RUnlock()
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	before := reach.BuildCounter()
	done := make(chan IncrementalDerivedReport, 1)
	go func() {
		done <- mi.RunIncrementalDerivedPasses(ctx, incrementalDerivedLegacyPlan())
	}()
	waitForIncrementalDerivedBatchWriter(t, mi)
	cancel()

	mi.batchMutationGate.RUnlock()
	gateHeld = false
	select {
	case report := <-done:
		require.Equal(t, IncrementalDerivedReport{}, report)
	case <-time.After(5 * time.Second):
		t.Fatal("canceled incremental derived wrapper did not return after the batch gate was released")
	}
	require.Equal(t, before, reach.BuildCounter(), "canceled queued work entered the topology transaction")
	require.Zero(t, store.checkpoints.Load(), "canceled queued work entered global passes")
}
