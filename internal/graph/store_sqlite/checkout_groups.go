package store_sqlite

import "github.com/zzet/gortex/internal/graph"

// Checkout-group publication for the SQLite store. The map is process
// state, not table state: see graph/checkout_groups.go for why a
// worktree relationship must not survive into a daemon that no longer
// tracks the worktree.

// The coreless() guards below are required, not defensive noise. The map moved
// onto storeCore in the 2026-09-04 upstream merge, so every accessor now
// dereferences the embedded pointer. A nil *Store or a zero Store value used to
// read the zero map and group nothing; without these it panics instead.

// SetCheckoutGroups publishes the repo-prefix → checkout-group map.
func (s *Store) SetCheckoutGroups(byPrefix map[string]string) {
	if s.coreless() {
		return
	}
	s.checkoutGroups.SetCheckoutGroups(byPrefix)
}

// CheckoutGroup implements graph.CheckoutGrouped.
func (s *Store) CheckoutGroup(repoPrefix string) string {
	if s.coreless() {
		return ""
	}
	return s.checkoutGroups.CheckoutGroup(repoPrefix)
}

// HasCheckoutGroups implements graph.CheckoutGrouped.
func (s *Store) HasCheckoutGroups() bool {
	return !s.coreless() && s.checkoutGroups.HasCheckoutGroups()
}

var _ graph.CheckoutGrouped = (*Store)(nil)
