package graphview

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/pathkey"
)

// CheckoutForPath finds the registered checkout a filesystem path sits in.
//
// The catalog indexes checkouts by family, so the caller passes the families
// its corpus reaches rather than the table being scanned whole. The longest
// root wins: worktrees nest inside other checkouts, and the innermost one is
// the working copy a path actually belongs to.
//
// A path that lies in no registered checkout is not an error — it is the
// ordinary case for every directory nobody has tracked.
func CheckoutForPath(
	ctx context.Context,
	catalog *store_sqlite.Catalog,
	familyIDs []string,
	path string,
) (store_sqlite.Checkout, bool, error) {
	if catalog == nil || path == "" {
		return store_sqlite.Checkout{}, false, nil
	}
	cleaned := filepath.Clean(path)
	var (
		best  store_sqlite.Checkout
		found bool
	)
	for _, familyID := range familyIDs {
		if familyID == "" {
			continue
		}
		checkouts, err := catalog.ListCheckouts(ctx, familyID)
		if err != nil {
			return store_sqlite.Checkout{}, false, WrapViewError(CodeCheckoutInaccessible,
				fmt.Sprintf("list the checkouts of family %q", familyID), err)
		}
		for _, checkout := range checkouts {
			if checkout.RootPath == "" {
				continue
			}
			root := filepath.Clean(checkout.RootPath)
			if !pathkey.HasPathPrefix(cleaned, root) {
				continue
			}
			if !found || len(root) > len(filepath.Clean(best.RootPath)) {
				best, found = checkout, true
			}
		}
	}
	return best, found, nil
}

// ServesAutomaticView reports whether a checkout is one the shared lane
// answers for: it is live, and it is served from the family's automatic
// graph rather than from a dedicated one. A dedicated checkout and the
// primary are read from the indexed corpus directly, so neither has a
// composed view to route to.
func ServesAutomaticView(checkout store_sqlite.Checkout) bool {
	return checkout.State == store_sqlite.CheckoutStateReady &&
		checkout.EffectiveMode == store_sqlite.CheckoutModeAutomatic
}

// RouteReady reports whether a route can serve a composed view: it must be
// active and name both generation slots. A route missing the working-tree
// slot is mid-build — serving the commit generation alone would answer out
// of a state of the world the checkout is not in.
func RouteReady(route store_sqlite.CheckoutRoute) bool {
	return route.State == store_sqlite.RouteActive &&
		route.CommitGenerationID > 0 && route.DirtyGenerationID > 0
}
