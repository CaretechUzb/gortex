package store_sqlite

import "time"

// SetMemEstBudgetForTest shrinks the per-recompute budget that
// AllRepoMemoryEstimates gives its COUNT scans, so a test can force the
// truncated path deterministically instead of waiting for a slow store.
// It returns a restore func.
func SetMemEstBudgetForTest(d time.Duration) func() {
	prev := memEstBudget
	memEstBudget = d
	return func() { memEstBudget = prev }
}
