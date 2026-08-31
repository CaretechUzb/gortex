package graph

// EdgeRowsInsertedReporter is an optional capability: report how many edge rows
// this process has actually WRITTEN.
//
// It exists because a derived pass's own edge count cannot answer whether the
// pass contributed anything. Every edge ingest is `INSERT OR IGNORE`, and a
// pass's total deliberately includes already-persisted idempotent results, so a
// pass that re-derives 67,136 edges the store already holds and a pass that
// discovers 67,136 new ones report the same number.
//
// The distinction is load-bearing for the worktree-copy path. A copy installs a
// sibling's subgraph including its derived edges, and the post-track derive then
// re-runs the same pass families over the whole repository. Whether that second
// run is doing anything is exactly the question this answers.
//
// Backends without durable row accounting simply do not implement it; callers
// type-assert and skip the measurement, as with every other optional capability
// in this package.
type EdgeRowsInsertedReporter interface {
	EdgeRowsInserted() int64
}

// EdgeRowsInserted reports the process-wide count of edge rows written, and
// whether the backend could answer. Monotonic — read it as a delta across the
// window you care about.
//
// Only meaningful as a delta over a window where nothing else writes edges. The
// global derived passes hold the batch-mutation gate for their duration, which
// is what makes the window around them well-defined.
func EdgeRowsInserted(s Store) (int64, bool) {
	reporter, ok := s.(EdgeRowsInsertedReporter)
	if !ok {
		return 0, false
	}
	return reporter.EdgeRowsInserted(), true
}
