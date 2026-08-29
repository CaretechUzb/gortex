package resolver

import (
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// odooPhaseFixture seeds a store with exactly n Odoo external-ID references,
// so the pass's own `edges_collected` phase counts back to the store it ran
// over and to no other.
func odooPhaseFixture(n int) *graph.Graph {
	g := graph.New()
	for i := range n {
		odooRepoRecord(g, "odoo", fmt.Sprintf("sale.view_order_%d", i))
	}
	return g
}

// odooPhases returns the Odoo pass's reported phase breakdown from a report.
func odooPhases(t *testing.T, rep FrameworkSynthReport) map[string]int64 {
	t.Helper()
	for _, c := range rep.Per {
		if c.Name == SynthOdoo {
			return c.PhaseMillis
		}
	}
	t.Fatalf("no %s row in the report", SynthOdoo)
	return nil
}

// Concurrent runs must each report their own phase timings.
//
// The timings used to travel through a package-level variable, on the argument
// that the synthesizer loop is serial by construction. It is — within one run.
// Both production callers hold only their own MultiIndexer's topology lock, so
// two indexers in one process run two loops through one variable, and `go test
// -race ./internal/indexer/` reported that four times over: four blocks, every
// one of them on the same address, the read and the clear inside
// takeSynthPhases racing. Because -race blames whichever test happens to be
// running, the four blocks landed on four unrelated tests and read as suite
// flakiness rather than as one defect. This test reproduces that shape
// directly, so the failure names its cause.
//
// Measured against mutants, it catches exactly two things:
//
//   - the unguarded shared variable, under -race;
//   - a report the slot never reaches — cutting PhaseMillis out of SynthCount
//     fails the precondition below, so the equality is not asserting on nil.
//
// It does NOT catch a mutex-guarded shared slot: that mutant passes here even
// under -race -count=3, because the window between a pass writing its phases
// and the loop reading them is too short to lose the race reliably. Nothing in
// a concurrency test can be trusted to close it. The structural assertion in
// TestDefaultFrameworkSynthesizers_PhaseSlotIsPerRegistryInstance is what
// makes "add a lock and keep one slot" fail, and it fails deterministically.
func TestRunFrameworkSynthesizers_PhaseTimingsAreOwnedByTheirRun(t *testing.T) {
	// Distinct sizes are what make a stolen breakdown visible; equal ones
	// would agree no matter which run wrote them.
	sizes := []int{1, 2, 3, 4, 5, 6}

	reports := make([]FrameworkSynthReport, len(sizes))
	var wg sync.WaitGroup
	for i, n := range sizes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			reports[i] = RunFrameworkSynthesizers(odooPhaseFixture(n))
		}()
	}
	wg.Wait()

	for i, n := range sizes {
		phases := odooPhases(t, reports[i])
		require.NotEmpty(t, phases,
			"precondition: the Odoo pass must report its phases, or this test asserts nothing")
		assert.Equal(t, int64(n), phases["edges_collected"],
			"run %d collected %d Odoo references and must report its own count", i, n)
	}
}

// The registry hands out a fresh phase slot per call.
//
// This is the assertion that holds the fix in place, because it fails on ANY
// shared slot — guarded or not. The obvious repair for the race this replaced
// is a mutex on the one variable, and a mutex silences the detector while
// leaving two runs able to report each other's breakdown; that mutant is
// invisible to the concurrency test above and fails deterministically here.
// A future change that caches defaultFrameworkSynthesizers' slice fails here
// too, naming the cause, rather than surfacing as an intermittent wrong number.
func TestDefaultFrameworkSynthesizers_PhaseSlotIsPerRegistryInstance(t *testing.T) {
	slot := func() *synthPhases {
		for _, s := range defaultFrameworkSynthesizers() {
			if sf, ok := s.(synthFunc); ok && sf.name == SynthOdoo {
				return sf.phases
			}
		}
		t.Fatalf("no %s entry in the registry", SynthOdoo)
		return nil
	}
	first, second := slot(), slot()
	require.NotNil(t, first)
	assert.NotSame(t, first, second,
		"two registries must not share one phase slot: that is the package-level sink again")
}
