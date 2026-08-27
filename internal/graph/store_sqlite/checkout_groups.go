package store_sqlite

import "github.com/zzet/gortex/internal/graph"

// Checkout-group publication for the SQLite store. The map is process
// state, not table state: see graph/checkout_groups.go for why a
// worktree relationship must not survive into a daemon that no longer
// tracks the worktree.

// SetCheckoutGroups publishes the repo-prefix → checkout-group map.
func (s *Store) SetCheckoutGroups(byPrefix map[string]string) {
	s.checkoutGroups.SetCheckoutGroups(byPrefix)
}

// CheckoutGroup implements graph.CheckoutGrouped.
func (s *Store) CheckoutGroup(repoPrefix string) string {
	return s.checkoutGroups.CheckoutGroup(repoPrefix)
}

// HasCheckoutGroups implements graph.CheckoutGrouped.
func (s *Store) HasCheckoutGroups() bool { return s.checkoutGroups.HasCheckoutGroups() }

var _ graph.CheckoutGrouped = (*Store)(nil)
