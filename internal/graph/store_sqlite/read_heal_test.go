package store_sqlite

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

// A store written BEFORE the write backstop existed can carry structurally
// impossible edges. Read paths must heal them — never materialize the junk
// into Go objects (pure GC pressure) — and the store-scoped integrity signal
// must move so operators know the on-disk store needs an audit/rebuild.
func TestReadPathsHealStructurallyInvalidRows(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "heal.sqlite"))
	require.NoError(t, err)
	defer s.Close()

	s.AddBatch([]*graph.Node{
		{ID: "a/t.go::T", Kind: graph.KindType, Name: "T", FilePath: "a/t.go"},
		{ID: "a/f.go::F#param:ctx", Kind: graph.KindParam, Name: "ctx", FilePath: "a/f.go"},
	}, []*graph.Edge{
		{From: "a/t.go::T", To: "a/t.go::I", Kind: graph.EdgeCalls, FilePath: "a/t.go", Line: 3},
	})

	// Simulate pre-backstop corruption: raw SQL bypasses every write funnel.
	_, err = s.writerDB.Exec(`INSERT INTO edges
  (from_id, to_id, kind, file_path, line, confidence, confidence_label, origin, tier, cross_repo, meta)
  VALUES ('a/t.go::T', 'a/f.go::F#param:ctx', 'implements', 'a/t.go', 1, 1.0, 'EXTRACTED', 'lsp_dispatch', '', 0, NULL)`)
	require.NoError(t, err)

	before := s.StructuralIntegritySnapshot(graph.StructuralIntegritySnapshotOptions{}).Totals.ReadSuppressed
	out := s.GetOutEdges("a/t.go::T")
	for _, e := range out {
		assert.NotEqual(t, graph.EdgeImplements, e.Kind, "junk implements row must be healed on read")
	}
	require.Len(t, out, 1, "the legitimate call edge must survive the heal")
	after := s.StructuralIntegritySnapshot(graph.StructuralIntegritySnapshotOptions{}).Totals.ReadSuppressed
	assert.Greater(t, after, before, "healing must move the store-scoped integrity signal")
}
