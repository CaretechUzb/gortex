package graphview

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// seedBindingCheckout writes one checkout row under the shared test family.
func seedBindingCheckout(t *testing.T, store *store_sqlite.Store, id, root string, mode store_sqlite.CheckoutMode, state store_sqlite.CheckoutState) {
	t.Helper()
	if err := store.Catalog().UpsertCheckout(context.Background(), store_sqlite.Checkout{
		CheckoutID:    id,
		Incarnation:   "inc-" + id,
		FamilyID:      testFamilyID,
		RootPath:      root,
		GitDir:        filepath.Join(root, ".git"),
		AdminName:     id,
		State:         state,
		DesiredMode:   mode,
		EffectiveMode: mode,
		LastSeen:      101,
	}); err != nil {
		t.Fatalf("UpsertCheckout(%s): %v", id, err)
	}
}

// TestCheckoutForPathPicksTheInnermostCheckout pins the rule that makes a
// nested worktree reachable: a path inside two registered roots belongs to
// the deeper one, not to whichever the catalog listed first.
func TestCheckoutForPathPicksTheInnermostCheckout(t *testing.T) {
	store := openStackStore(t, "binding")
	seedStackControlPlane(t, store)
	base := t.TempDir()
	outer := filepath.Join(base, "repo")
	inner := filepath.Join(outer, "worktrees", "feature")
	seedBindingCheckout(t, store, "co-outer", outer, store_sqlite.CheckoutModeDedicated, store_sqlite.CheckoutStateReady)
	seedBindingCheckout(t, store, "co-inner", inner, store_sqlite.CheckoutModeAutomatic, store_sqlite.CheckoutStateReady)

	ctx := context.Background()
	families := []string{testFamilyID}
	for _, tc := range []struct {
		path string
		want string
	}{
		{outer, "co-outer"},
		{filepath.Join(outer, "internal", "x.go"), "co-outer"},
		{inner, "co-inner"},
		{filepath.Join(inner, "internal", "x.go"), "co-inner"},
	} {
		checkout, found, err := CheckoutForPath(ctx, store.Catalog(), families, tc.path)
		if err != nil {
			t.Fatalf("CheckoutForPath(%s): %v", tc.path, err)
		}
		if !found || checkout.CheckoutID != tc.want {
			t.Errorf("CheckoutForPath(%s) = %q (found %v), want %q", tc.path, checkout.CheckoutID, found, tc.want)
		}
	}

	outside := filepath.Join(base, "elsewhere")
	if _, found, err := CheckoutForPath(ctx, store.Catalog(), families, outside); err != nil || found {
		t.Errorf("CheckoutForPath(%s) = found %v, err %v; want no checkout and no error", outside, found, err)
	}
	// A sibling whose name merely starts with a registered root is not inside
	// it: the match respects path components.
	sibling := outer + "-fork"
	if _, found, _ := CheckoutForPath(ctx, store.Catalog(), families, sibling); found {
		t.Errorf("CheckoutForPath(%s) matched a root it only shares a prefix with", sibling)
	}
}

func TestServesAutomaticViewRequiresLiveAndAutomatic(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state store_sqlite.CheckoutState
		mode  store_sqlite.CheckoutMode
		want  bool
	}{
		{"live automatic", store_sqlite.CheckoutStateReady, store_sqlite.CheckoutModeAutomatic, true},
		{"live dedicated", store_sqlite.CheckoutStateReady, store_sqlite.CheckoutModeDedicated, false},
		{"unavailable automatic", store_sqlite.CheckoutStateUnavailable, store_sqlite.CheckoutModeAutomatic, false},
		{"reconciling automatic", store_sqlite.CheckoutStateReconciling, store_sqlite.CheckoutModeAutomatic, false},
	} {
		got := ServesAutomaticView(store_sqlite.Checkout{State: tc.state, EffectiveMode: tc.mode})
		if got != tc.want {
			t.Errorf("%s: ServesAutomaticView = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestRouteReadyRequiresBothSlots(t *testing.T) {
	for _, tc := range []struct {
		name  string
		route store_sqlite.CheckoutRoute
		want  bool
	}{
		{"active with both slots", store_sqlite.CheckoutRoute{State: store_sqlite.RouteActive, CommitGenerationID: 1, DirtyGenerationID: 2}, true},
		{"active without the working tree", store_sqlite.CheckoutRoute{State: store_sqlite.RouteActive, CommitGenerationID: 1}, false},
		{"active without a commit", store_sqlite.CheckoutRoute{State: store_sqlite.RouteActive, DirtyGenerationID: 2}, false},
		{"pending with both slots", store_sqlite.CheckoutRoute{State: store_sqlite.RoutePending, CommitGenerationID: 1, DirtyGenerationID: 2}, false},
		{"retired with both slots", store_sqlite.CheckoutRoute{State: store_sqlite.RouteRetired, CommitGenerationID: 1, DirtyGenerationID: 2}, false},
	} {
		if got := RouteReady(tc.route); got != tc.want {
			t.Errorf("%s: RouteReady = %v, want %v", tc.name, got, tc.want)
		}
	}
}
