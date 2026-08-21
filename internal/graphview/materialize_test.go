package graphview

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// The control plane every routed test addresses.
const (
	testGraphID    = "graph-1"
	testCheckoutID = "wt-1"
	testFamilyID   = "fam-1"
)

// seedStackControlPlane installs the family, checkout and dedicated
// graph a route hangs off. The route itself is left to each test, since
// what a route points at is what most of them are about.
func seedStackControlPlane(t *testing.T, store *store_sqlite.Store) {
	t.Helper()
	ctx := context.Background()
	catalog := store.Catalog()
	if err := catalog.UpsertRepositoryFamily(ctx, store_sqlite.RepositoryFamily{
		FamilyID:          testFamilyID,
		CommonDirIdentity: "identity/" + testFamilyID,
		DisplayRemote:     "git@example.invalid:" + testFamilyID + ".git",
		State:             "family_ready",
		CreatedAt:         100,
		LastSeen:          100,
	}); err != nil {
		t.Fatalf("UpsertRepositoryFamily: %v", err)
	}
	if err := catalog.UpsertCheckout(ctx, store_sqlite.Checkout{
		CheckoutID:    testCheckoutID,
		Incarnation:   "inc-1",
		FamilyID:      testFamilyID,
		RootPath:      "/tmp/" + testCheckoutID,
		GitDir:        "/tmp/" + testCheckoutID + "/.git",
		AdminName:     testCheckoutID,
		State:         store_sqlite.CheckoutStateReady,
		DesiredMode:   store_sqlite.CheckoutModeDedicated,
		EffectiveMode: store_sqlite.CheckoutModeDedicated,
		HeadRef:       "refs/heads/main",
		HeadCommit:    "c0ffee",
		HeadTree:      "7ee7",
		LastSeen:      101,
	}); err != nil {
		t.Fatalf("UpsertCheckout: %v", err)
	}
	if err := catalog.UpsertDedicatedGraph(ctx, store_sqlite.DedicatedGraph{
		GraphID:         testGraphID,
		OwnerCheckoutID: testCheckoutID,
		RepoPrefix:      stackRepo,
		FamilyID:        testFamilyID,
		State:           "graph_ready",
	}); err != nil {
		t.Fatalf("UpsertDedicatedGraph: %v", err)
	}
}

// routeStack points the checkout's route at a pair of generation slots.
// A slot of 0 is an unset pointer, which is how a checkout with no
// working-tree generation is routed.
func routeStack(t *testing.T, store *store_sqlite.Store, commit, dirty int64, state store_sqlite.RouteState) {
	t.Helper()
	if err := store.Catalog().UpsertCheckoutRoute(context.Background(), store_sqlite.CheckoutRoute{
		CheckoutID:         testCheckoutID,
		GraphID:            testGraphID,
		CommitGenerationID: commit,
		DirtyGenerationID:  dirty,
		State:              state,
	}); err != nil {
		t.Fatalf("UpsertCheckoutRoute: %v", err)
	}
}

func newTestMaterializer(store *store_sqlite.Store) *Materializer {
	return &Materializer{Store: store, Catalog: store.Catalog(), Leases: NewLeaseManager()}
}

// seedRoutedStack builds the whole fixture a routed view needs: the
// corpus, both generations, the control plane, and the active route.
func seedRoutedStack(t *testing.T, store *store_sqlite.Store) (commit, dirty int64) {
	t.Helper()
	seedStackCorpus(t, store)
	commit = writeStackCommitGeneration(t, store)
	dirty = writeStackDirtyGeneration(t, store, commit)
	seedStackControlPlane(t, store)
	routeStack(t, store, commit, dirty, store_sqlite.RouteActive)
	return commit, dirty
}

// TestMaterializeCheckoutReadsTheRoutedStack is the end-to-end: the
// route names two generations, and what the checkout reads through them
// is the tree those generations describe — the same graph a flat index
// of that tree produces.
func TestMaterializeCheckoutReadsTheRoutedStack(t *testing.T) {
	store := openStackStore(t, "routed")
	commit, dirty := seedRoutedStack(t, store)

	flat := openStackStore(t, "flat")
	seedStackFlatCorpus(t, flat)

	view, err := newTestMaterializer(store).MaterializeCheckout(context.Background(), testCheckoutID)
	if err != nil {
		t.Fatalf("MaterializeCheckout: %v", err)
	}
	defer view.Close()

	if got, want := view.Generations(), []int64{commit, dirty}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("Generations() = %v, want %v", got, want)
	}
	if view.ID.BaseGeneration != commit {
		t.Errorf("identity base generation = %d, want the commit generation %d", view.ID.BaseGeneration, commit)
	}
	if view.ID.RepoPrefix != stackRepo || view.ID.BaseGraphID != testGraphID {
		t.Errorf("identity names %q/%q, want %q/%q", view.ID.RepoPrefix, view.ID.BaseGraphID, stackRepo, testGraphID)
	}
	if len(view.ID.Layers) != 1 {
		t.Fatalf("identity names %d layers, want the working tree alone", len(view.ID.Layers))
	}
	want := LayerRef{Kind: LayerDirty, LayerID: stackDirtyLayerID, Generation: dirty}
	if !view.ID.Layers[0].Equal(want) {
		t.Errorf("layer = %+v, want %+v", view.ID.Layers[0], want)
	}

	assertReadersAgree(t, view.Reader, flat)
}

// TestMaterializeCheckoutCompletenessRunsBottomUp pins the union: the
// corpus underneath contributes completeness for everything, and the one
// capability the working-tree generation declares narrowed is the only
// one the view reports narrowed.
func TestMaterializeCheckoutCompletenessRunsBottomUp(t *testing.T) {
	store := openStackStore(t, "completeness")
	seedRoutedStack(t, store)

	view, err := newTestMaterializer(store).MaterializeCheckout(context.Background(), testCheckoutID)
	if err != nil {
		t.Fatalf("MaterializeCheckout: %v", err)
	}
	defer view.Close()

	if got := view.Completeness.State(CapResolutionCrossRepo); got != StateIncomplete {
		t.Errorf("%s = %q, want %q", CapResolutionCrossRepo, got, StateIncomplete)
	}
	for _, id := range KnownCapabilities() {
		if id == CapResolutionCrossRepo {
			continue
		}
		if got := view.Completeness.State(id); got != StateComplete {
			t.Errorf("%s = %q, want %q", id, got, StateComplete)
		}
	}
	if err := view.Completeness.Evaluate([]CapabilityID{CapSyntaxGraph}, nil); err != nil {
		t.Errorf("a capability nothing narrowed did not evaluate: %v", err)
	}
	if err := view.Completeness.Evaluate([]CapabilityID{CapResolutionCrossRepo}, nil); err == nil {
		t.Error("the narrowed capability evaluated as servable")
	}
}

// TestMaterializeCheckoutRefusesPartialStacks pins the rule that a route
// whose slots cannot all be served yields a typed failure rather than a
// thinner stack. Every case here would otherwise answer out of the wrong
// state of the world while looking like a success.
func TestMaterializeCheckoutRefusesPartialStacks(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name  string
		setup func(t *testing.T, store *store_sqlite.Store, commit, dirty int64)
		code  string
	}{
		{
			name:  "checkout is not routed",
			setup: func(t *testing.T, store *store_sqlite.Store, _, _ int64) { deleteRoute(t, store) },
			code:  CodeCheckoutInaccessible,
		},
		{
			name: "route has retired",
			setup: func(t *testing.T, store *store_sqlite.Store, commit, dirty int64) {
				routeStack(t, store, commit, dirty, store_sqlite.RouteRetired)
			},
			code: CodeCheckoutInaccessible,
		},
		{
			name: "no commit generation",
			setup: func(t *testing.T, store *store_sqlite.Store, _, dirty int64) {
				routeStack(t, store, 0, dirty, store_sqlite.RouteActive)
			},
			code: CodeViewBuilding,
		},
		{
			name: "working-tree generation is still building",
			setup: func(t *testing.T, store *store_sqlite.Store, _, dirty int64) {
				setGenerationState(t, store, dirty, store_sqlite.ViewGenerationBuilding, store_sqlite.ViewGenerationReady)
			},
			code: CodeViewBuilding,
		},
		{
			name: "working-tree generation is retiring",
			setup: func(t *testing.T, store *store_sqlite.Store, _, dirty int64) {
				setGenerationState(t, store, dirty, store_sqlite.ViewGenerationRetiring, store_sqlite.ViewGenerationReady)
			},
			code: CodeCheckoutInaccessible,
		},
		{
			name: "commit generation is retiring",
			setup: func(t *testing.T, store *store_sqlite.Store, commit, _ int64) {
				setGenerationState(t, store, commit, store_sqlite.ViewGenerationRetiring, store_sqlite.ViewGenerationReady)
			},
			code: CodeCheckoutInaccessible,
		},
		{
			name:  "graph has no catalog row",
			setup: func(t *testing.T, store *store_sqlite.Store, _, _ int64) { deleteDedicatedGraph(t, store) },
			code:  CodeCheckoutInaccessible,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := openStackStore(t, "partial")
			commit, dirty := seedRoutedStack(t, store)
			tc.setup(t, store, commit, dirty)

			materializer := newTestMaterializer(store)
			view, err := materializer.MaterializeCheckout(ctx, testCheckoutID)
			if err == nil {
				view.Close()
				t.Fatalf("MaterializeCheckout succeeded, want %s", tc.code)
			}
			if code := CodeOf(err); code != tc.code {
				t.Fatalf("error code = %q, want %q (%v)", code, tc.code, err)
			}
			for _, id := range []int64{commit, dirty} {
				if materializer.Leases.InUse(id) {
					t.Fatalf("generation %d stayed leased after a refused materialization", id)
				}
			}
		})
	}
}

// TestMaterializeCheckoutValidatesItsInputs pins the guards that turn a
// missing dependency into a typed error here instead of a nil
// dereference inside a read.
func TestMaterializeCheckoutValidatesItsInputs(t *testing.T) {
	store := openStackStore(t, "inputs")
	full := newTestMaterializer(store)

	cases := map[string]struct {
		materializer *Materializer
		ctx          context.Context
		checkoutID   string
	}{
		"nil materializer": {nil, context.Background(), testCheckoutID},
		"no store":         {&Materializer{Catalog: store.Catalog(), Leases: NewLeaseManager()}, context.Background(), testCheckoutID},
		"no catalog":       {&Materializer{Store: store, Leases: NewLeaseManager()}, context.Background(), testCheckoutID},
		"no lease manager": {&Materializer{Store: store, Catalog: store.Catalog()}, context.Background(), testCheckoutID},
		"no context":       {full, nil, testCheckoutID},
		"no checkout id":   {full, context.Background(), ""},
		"unknown checkout": {full, context.Background(), "nobody"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			view, err := tc.materializer.MaterializeCheckout(tc.ctx, tc.checkoutID)
			if err == nil {
				view.Close()
				t.Fatal("MaterializeCheckout succeeded, want an error")
			}
			if CodeOf(err) == "" {
				t.Fatalf("error carries no view code: %v", err)
			}
		})
	}
}

// TestMaterializeCheckoutLeaseBlocksRetirement is the lease acceptance:
// retirement consults the same manager the materializer pins through, so
// a generation a live view reads cannot be swept out from under it, and
// the drain completes once the view closes.
func TestMaterializeCheckoutLeaseBlocksRetirement(t *testing.T) {
	ctx := context.Background()
	store := openStackStore(t, "leases")
	commit, dirty := seedRoutedStack(t, store)

	materializer := newTestMaterializer(store)
	view, err := materializer.MaterializeCheckout(ctx, testCheckoutID)
	if err != nil {
		t.Fatalf("MaterializeCheckout: %v", err)
	}
	for _, id := range []int64{commit, dirty} {
		if !materializer.Leases.InUse(id) {
			t.Fatalf("generation %d is not leased by the materialized view", id)
		}
	}

	// Un-route the working-tree generation so the catalog's own reference
	// guard passes: what must refuse the retire from here on is the lease
	// alone, not the route still pointing at it.
	unrouteDirty(t, store)
	if err := store.RetirePayloadGeneration(ctx, dirty, materializer.Leases.InUse); !errors.Is(err, store_sqlite.ErrPayloadGenerationInUse) {
		t.Fatalf("retire while the view is open = %v, want %v", err, store_sqlite.ErrPayloadGenerationInUse)
	}

	// The drain cannot finish while the view holds the lease, so a bounded
	// wait must expire rather than report the generation released.
	bounded, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if err := materializer.Leases.WaitDrain(bounded, view.Generations()...); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WaitDrain while the view is open = %v, want %v", err, context.DeadlineExceeded)
	}

	view.Close()
	view.Close() // idempotent: a second close must not drop another pin
	if err := materializer.Leases.WaitDrain(ctx, commit, dirty); err != nil {
		t.Fatalf("WaitDrain after Close: %v", err)
	}
	if materializer.Leases.InUse(dirty) {
		t.Fatal("the working-tree generation is still leased after Close")
	}
	if err := store.RetirePayloadGeneration(ctx, dirty, materializer.Leases.InUse); err != nil {
		t.Fatalf("retire after Close: %v", err)
	}
}

// deleteRoute withdraws the checkout's route row.
func deleteRoute(t *testing.T, store *store_sqlite.Store) {
	t.Helper()
	if err := store.Catalog().DeleteCheckoutRoute(context.Background(), testCheckoutID); err != nil {
		t.Fatalf("DeleteCheckoutRoute: %v", err)
	}
}

// unrouteDirty clears the working-tree slot through the route's own
// compare-and-set, which is how a reconciler retires a generation.
func unrouteDirty(t *testing.T, store *store_sqlite.Store) {
	t.Helper()
	ctx := context.Background()
	catalog := store.Catalog()
	route, found, err := catalog.GetCheckoutRoute(ctx, testCheckoutID)
	if err != nil || !found {
		t.Fatalf("GetCheckoutRoute: %v (found=%v)", err, found)
	}
	if err := catalog.FlipCheckoutRouteSlot(ctx, store_sqlite.FlipCheckoutRouteSlotRequest{
		CheckoutID:         testCheckoutID,
		Slot:               store_sqlite.RouteSlotDirty,
		GenerationID:       0,
		ExpectedRouteEpoch: route.RouteEpoch,
		State:              store_sqlite.RouteActive,
	}); err != nil {
		t.Fatalf("FlipCheckoutRouteSlot: %v", err)
	}
}

// deleteDedicatedGraph removes the graph row the view identity is named
// from, leaving the route pointing at a graph nothing describes.
func deleteDedicatedGraph(t *testing.T, store *store_sqlite.Store) {
	t.Helper()
	if err := store.Catalog().DeleteDedicatedGraph(context.Background(), testGraphID); err != nil {
		t.Fatalf("DeleteDedicatedGraph: %v", err)
	}
}

func setGenerationState(t *testing.T, store *store_sqlite.Store, generationID int64, next, expected store_sqlite.ViewGenerationState) {
	t.Helper()
	if err := store.Catalog().SetViewGenerationState(context.Background(), generationID, next, expected); err != nil {
		t.Fatalf("SetViewGenerationState(%d, %s): %v", generationID, next, err)
	}
}
