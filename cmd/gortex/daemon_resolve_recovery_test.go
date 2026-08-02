package main

import (
	"errors"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

type warmupResolveStateStore struct {
	graph.Store
	token graph.ResolveStateToken
	err   error
}

func (store *warmupResolveStateStore) BeginResolvePass() (graph.ResolveStateToken, error) {
	return store.token, store.err
}

func (store *warmupResolveStateStore) ResolvePassIncomplete() (graph.ResolveStateToken, bool, error) {
	if store.err != nil {
		return graph.ResolveStateToken{}, false, store.err
	}
	return store.token, store.token.Generation != 0, nil
}

func (store *warmupResolveStateStore) CompleteResolvePass(graph.ResolveStateToken) error {
	return store.err
}

func TestWarmupResolveRecoveryPolicyForcesFullReconstruction(t *testing.T) {
	recovery := warmupResolveRecovery{
		token:    graph.ResolveStateToken{Generation: 73},
		required: true,
	}
	prior := map[string]int64{"repo/a.go": 1}
	scope := map[string]struct{}{"repo": {}}

	if got := recovery.priorMtimes(prior); got != nil {
		t.Fatalf("prior mtimes = %v, want nil full-track sentinel", got)
	}
	if !recovery.anyChanged(0) {
		t.Fatal("recovery did not force the global resolve path")
	}
	if recovery.exactDelta(true) {
		t.Fatal("recovery retained the exact warm-delta path")
	}
	if got := recovery.resolveScope(scope); got != nil {
		t.Fatalf("resolve scope = %v, want nil whole-graph scope", got)
	}
}

func TestProbeWarmupResolveRecoveryFailsClosed(t *testing.T) {
	probeErr := errors.New("resolve_state unavailable")
	store := &warmupResolveStateStore{Store: graph.New(), err: probeErr}
	if _, err := probeWarmupResolveRecovery(store); !errors.Is(err, probeErr) {
		t.Fatalf("probe error = %v, want %v", err, probeErr)
	}
}

func TestProbeWarmupResolveRecoveryPreservesObservedGeneration(t *testing.T) {
	store := &warmupResolveStateStore{
		Store: graph.New(),
		token: graph.ResolveStateToken{Generation: 91},
	}
	recovery, err := probeWarmupResolveRecovery(store)
	if err != nil {
		t.Fatal(err)
	}
	if !recovery.required || recovery.token.Generation != 91 || recovery.token.Owned {
		t.Fatalf("recovery = %+v, want required observed generation 91", recovery)
	}
}
