package graph

import "fmt"

// ResolveStateToken identifies one durable whole-graph resolution pass.
// Owned is true only for the caller that created the active generation;
// nested resolver coordinators observe the same generation but must not clear
// it before their outer pipeline finishes.
type ResolveStateToken struct {
	Generation int64
	Owned      bool
}

// ResolveStateStore is an optional durable crash-recovery capability. Stores
// that implement it persist one active generation before any independently
// durable resolver mutation and remove that exact generation only after the
// complete resolver pipeline succeeds.
type ResolveStateStore interface {
	BeginResolvePass() (ResolveStateToken, error)
	ResolvePassIncomplete() (ResolveStateToken, bool, error)
	CompleteResolvePass(ResolveStateToken) error
}

// BeginResolvePass starts or joins a durable resolution generation. Backends
// without durable state need no restart recovery and return a zero token.
func BeginResolvePass(store Store) (ResolveStateToken, error) {
	state, ok := store.(ResolveStateStore)
	if !ok {
		return ResolveStateToken{}, nil
	}
	return state.BeginResolvePass()
}

// IncompleteResolvePass returns the active durable generation, if any.
func IncompleteResolvePass(store Store) (ResolveStateToken, bool, error) {
	state, ok := store.(ResolveStateStore)
	if !ok {
		return ResolveStateToken{}, false, nil
	}
	return state.ResolvePassIncomplete()
}

// CompleteResolvePass removes the exact generation named by token. A zero
// token is the no-op value returned for stores without durable state.
func CompleteResolvePass(store Store, token ResolveStateToken) error {
	if token.Generation == 0 {
		return nil
	}
	state, ok := store.(ResolveStateStore)
	if !ok {
		return fmt.Errorf("graph store lost resolve-state capability for generation %d", token.Generation)
	}
	return state.CompleteResolvePass(token)
}

// CompleteOwnedResolvePass completes only a generation created by this caller.
// Nested coordinators join the outer generation with Owned=false and leave it
// for the outermost successful boundary to clear.
func CompleteOwnedResolvePass(store Store, token ResolveStateToken) error {
	if !token.Owned {
		return nil
	}
	return CompleteResolvePass(store, token)
}
