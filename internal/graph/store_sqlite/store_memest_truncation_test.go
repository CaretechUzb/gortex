package store_sqlite_test

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// A COUNT scan that runs out of budget must not be cached. The estimate is
// advisory, but a *truncated* count is not stale — it is wrong, and the TTL
// would then serve that wrong number as if it were a measurement.
func TestAllRepoMemoryEstimates_TruncatedScanIsNotCached(t *testing.T) {
	path := filepath.Join(t.TempDir(), "g.sqlite")
	s, err := store_sqlite.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	const want = 20000
	batch := make([]*graph.Node, 0, want)
	for i := 0; i < want; i++ {
		id := fmt.Sprintf("a.go::F%d", i)
		batch = append(batch, &graph.Node{
			ID: id, Kind: graph.KindFunction, Name: fmt.Sprintf("F%d", i),
			FilePath: "a.go", RepoPrefix: fmt.Sprintf("r%d", i%4),
		})
	}
	s.AddBatch(batch, nil)

	totalNodes := func(m map[string]graph.RepoMemoryEstimate) int {
		n := 0
		for _, e := range m {
			n += e.NodeCount
		}
		return n
	}

	// Force the scan to run out of budget mid-flight.
	restore := store_sqlite.SetMemEstBudgetForTest(time.Millisecond)
	truncated := s.AllRepoMemoryEstimates()
	restore()

	if totalNodes(truncated) == want {
		t.Skip("scan completed inside the shrunken budget; nothing to assert")
	}

	// With a normal budget the very next call must report the true count.
	// Before the fix the truncated map was cached and served here for the
	// whole TTL, so this reads a plausible-looking wrong number.
	full := s.AllRepoMemoryEstimates()
	require.Equal(t, want, totalNodes(full),
		"a truncated COUNT was cached and served as if it were a real measurement")
}
