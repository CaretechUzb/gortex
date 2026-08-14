package store_sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func openStructuralIntegrityTestStore(t *testing.T, name string) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), name+".sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	return store
}

func rawInsertStructuralViolation(t *testing.T, store *Store, from, to string, kind graph.EdgeKind, origin, file string, line int) {
	t.Helper()
	_, err := store.writerDB.Exec(`INSERT INTO edges
  (from_id, to_id, kind, file_path, line, confidence, confidence_label, origin, tier, cross_repo, meta)
  VALUES (?, ?, ?, ?, ?, 1.0, 'EXTRACTED', ?, '', 0, NULL)`, from, to, string(kind), file, line, origin)
	if err != nil {
		t.Fatal(err)
	}
}

func TestSQLiteStructuralWriteRejectionsExactOnceAndAttributed(t *testing.T) {
	store := openStructuralIntegrityTestStore(t, "writes")
	store.AddNode(&graph.Node{ID: "a/source", Kind: graph.KindFunction, RepoPrefix: "repo-a", FilePath: "a.go"})
	store.AddEdge(&graph.Edge{From: "a/source", To: "a/target#param:x", Kind: graph.EdgeImplements, Origin: "LSP_DISPATCH"})
	store.AddEdge(nil)
	store.AddBatch(
		[]*graph.Node{{ID: "b/source", Kind: graph.KindFunction, RepoPrefix: "repo-b", FilePath: "b.go"}},
		[]*graph.Edge{
			{From: "b/source", To: "b/target#local:y", Kind: graph.EdgeOverrides, Origin: "AST"},
			{From: "b/source", To: "b/target#param:ok", Kind: graph.EdgeCalls, Origin: "AST"},
			{From: "b/source", To: "b/target#local:ok", Kind: graph.EdgeReferences, Origin: "AST"},
		},
	)

	snapshot := store.StructuralIntegritySnapshot(graph.StructuralIntegritySnapshotOptions{IncludeAttribution: true, IncludeSamples: true})
	if snapshot.Totals.WriteRejected != 2 || snapshot.Totals.ReadSuppressed != 0 {
		t.Fatalf("unexpected write totals: %+v", snapshot.Totals)
	}
	if len(snapshot.Attribution) != 2 {
		t.Fatalf("want one AddEdge and one AddBatch dimension: %+v", snapshot.Attribution)
	}
	if snapshot.Attribution[0].Repo != "repo-a" || snapshot.Attribution[0].Path != graph.StructuralPathSQLiteAddEdge || snapshot.Attribution[0].Count != 1 {
		t.Fatalf("AddEdge attribution mismatch: %+v", snapshot.Attribution[0])
	}
	if snapshot.Attribution[1].Repo != "repo-b" || snapshot.Attribution[1].Path != graph.StructuralPathSQLiteAddBatch || snapshot.Attribution[1].Count != 1 {
		t.Fatalf("AddBatch attribution mismatch: %+v", snapshot.Attribution[1])
	}
	if !store.structuralWriteWarned.Load() {
		t.Fatal("first write rejection must trigger this store's rate-limited warning")
	}
	edges := store.AllEdges()
	if len(edges) != 2 || edges[0].Kind != graph.EdgeCalls || edges[1].Kind != graph.EdgeReferences {
		t.Fatalf("valid call/reference edges to params/locals must survive: %+v", edges)
	}

	other := openStructuralIntegrityTestStore(t, "isolated")
	if got := other.StructuralIntegritySnapshot(graph.StructuralIntegritySnapshotOptions{}).Totals; !got.Empty() {
		t.Fatalf("store telemetry leaked across instances: %+v", got)
	}
	if other.structuralWriteWarned.Load() || other.structuralReadWarned.Load() {
		t.Fatal("warning rate limits must be store-scoped")
	}
}

func TestSQLiteStructuralReadSuppressionAndExactAudit(t *testing.T) {
	store := openStructuralIntegrityTestStore(t, "read-audit")
	store.AddNode(&graph.Node{ID: "a/source", Kind: graph.KindFunction, RepoPrefix: "repo-a", FilePath: "a.go"})
	rawInsertStructuralViolation(t, store, "a/source", "a/target#param:x", graph.EdgeImplements, " LSP_DISPATCH ", "a.go", 11)

	if got := store.AllEdges(); len(got) != 0 {
		t.Fatalf("full read materialized structurally invalid row: %+v", got)
	}
	if got := store.AllEdges(); len(got) != 0 {
		t.Fatalf("repeated full read materialized structurally invalid row: %+v", got)
	}
	if got := store.AllEdgesLight(); len(got) != 0 {
		t.Fatalf("light read materialized structurally invalid row: %+v", got)
	}

	snapshot := store.StructuralIntegritySnapshot(graph.StructuralIntegritySnapshotOptions{IncludeAttribution: true, IncludeSamples: true})
	if snapshot.Totals.ReadSuppressed != 3 || snapshot.Totals.WriteRejected != 0 {
		t.Fatalf("read observations must count every suppressed observation: %+v", snapshot.Totals)
	}
	if len(snapshot.Attribution) != 2 || snapshot.Attribution[0].Count != 2 || snapshot.Attribution[1].Count != 1 {
		t.Fatalf("full/light read attribution mismatch: %+v", snapshot.Attribution)
	}
	for _, dimension := range snapshot.Attribution {
		if dimension.Repo != "unknown" {
			t.Fatalf("routine scanner must not guess repository ownership: %+v", dimension)
		}
	}
	if !store.structuralReadWarned.Load() {
		t.Fatal("first read suppression must trigger this store's rate-limited warning")
	}

	audit, err := store.AuditStructuralIntegrity(context.Background(), graph.StructuralIntegrityAuditOptions{SampleLimit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if audit.Status != graph.StructuralAuditSupported || audit.TotalRows != 1 || len(audit.Groups) != 1 || len(audit.Samples) != 1 {
		t.Fatalf("unexpected exact audit: %+v", audit)
	}
	group := audit.Groups[0]
	if group.Repo != "repo-a" || group.Kind != graph.EdgeImplements || group.Reason != graph.StructuralReasonParameterTarget || group.Origin != "lsp_dispatch" || group.Count != 1 {
		t.Fatalf("exact persisted attribution mismatch: %+v", group)
	}
}

func TestSQLiteStructuralExactAuditScopeCapAndCancellation(t *testing.T) {
	store := openStructuralIntegrityTestStore(t, "scope")
	store.AddBatch([]*graph.Node{
		{ID: "a/source", Kind: graph.KindFunction, RepoPrefix: "repo-a"},
		{ID: "b/source", Kind: graph.KindFunction, RepoPrefix: "repo-b"},
	}, nil)
	rawInsertStructuralViolation(t, store, "a/source", "a/z#param:x", graph.EdgeImplements, "BETA", "z.go", 3)
	rawInsertStructuralViolation(t, store, "a/source", "a/a#local:y", graph.EdgeExtends, "ALPHA", "a.go", 1)
	rawInsertStructuralViolation(t, store, "a/source", "a/m#param:z", graph.EdgeOverrides, "GAMMA", "m.go", 2)
	rawInsertStructuralViolation(t, store, "b/source", "b/a#param:q", graph.EdgeMemberOf, "OTHER", "b.go", 1)

	audit, err := store.AuditStructuralIntegrity(context.Background(), graph.StructuralIntegrityAuditOptions{
		RepoPrefixes: []string{"repo-a"}, RepoScopeActive: true, SampleLimit: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if audit.TotalRows != 3 || len(audit.Samples) != 2 || !audit.Truncated {
		t.Fatalf("scope/cap mismatch: %+v", audit)
	}
	for _, group := range audit.Groups {
		if group.Repo != "repo-a" {
			t.Fatalf("group leaked out-of-scope repository: %+v", group)
		}
	}
	for _, sample := range audit.Samples {
		if sample.Repo != "repo-a" {
			t.Fatalf("sample leaked out-of-scope repository: %+v", sample)
		}
	}
	if audit.Samples[0].Origin > audit.Samples[1].Origin {
		t.Fatalf("samples are not deterministically ordered: %+v", audit.Samples)
	}

	empty, err := store.AuditStructuralIntegrity(context.Background(), graph.StructuralIntegrityAuditOptions{RepoScopeActive: true})
	if err != nil {
		t.Fatal(err)
	}
	if empty.TotalRows != 0 || len(empty.Groups) != 0 || len(empty.Samples) != 0 {
		t.Fatalf("active empty scope must match nothing: %+v", empty)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = store.AuditStructuralIntegrity(ctx, graph.StructuralIntegrityAuditOptions{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("audit must honor cancellation, got %v", err)
	}
}
