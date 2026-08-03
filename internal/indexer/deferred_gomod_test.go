package indexer

import "testing"

func TestRunDeferredGoModOnceIsIdempotentAndFallbackSafe(t *testing.T) {
	firstDone := false
	firstRuns := 0

	// Models the warmup predrain while the coordinated bulk load is open.
	if !runDeferredGoModOnce(true, &firstDone, func() { firstRuns++ }) {
		t.Fatal("warmup predrain did not run pending work")
	}
	if !firstDone || firstRuns != 1 {
		t.Fatalf("warmup state = (done %v, runs %d), want (true, 1)", firstDone, firstRuns)
	}

	// Models the retained RunPreEnrichResolve fallback. Already-drained work
	// is a no-op, while independently registered work still drains; the guard
	// is per repository rather than an unsafe process-wide once.
	if runDeferredGoModOnce(true, &firstDone, func() { firstRuns++ }) {
		t.Fatal("fallback reran already-drained work")
	}
	laterDone := false
	laterRuns := 0
	if !runDeferredGoModOnce(true, &laterDone, func() { laterRuns++ }) {
		t.Fatal("fallback skipped newly pending work")
	}
	if firstRuns != 1 || !laterDone || laterRuns != 1 {
		t.Fatalf("fallback state = (first %d, later done %v/runs %d), want (1, true/1)", firstRuns, laterDone, laterRuns)
	}
}

func TestRunDeferredGoModOnceSkipsAbsentWork(t *testing.T) {
	done := false
	runs := 0
	if runDeferredGoModOnce(false, &done, func() { runs++ }) {
		t.Fatal("absent work reported as drained")
	}
	if done || runs != 0 {
		t.Fatalf("absent work state = (done %v, runs %d), want (false, 0)", done, runs)
	}
}
