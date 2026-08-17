package graph

import (
	"fmt"
	"strings"
	"sync"
	"testing"
)

func invalidStructuralEdge(from, to string, kind EdgeKind, origin string) *Edge {
	return &Edge{From: from, To: to, Kind: kind, Origin: origin, FilePath: "repo/file.go", Line: 7}
}

func TestFilterStructuralEdgeViolationsPurePartition(t *testing.T) {
	validCall := invalidStructuralEdge("src", "dst#param:x", EdgeCalls, "call")
	invalidParam := invalidStructuralEdge("src", "dst#param:x", EdgeImplements, "lsp")
	invalidLocal := invalidStructuralEdge("src", "dst#local:y", EdgeOverrides, "ast")
	validStructural := invalidStructuralEdge("src", "dst", EdgeExtends, "ast")
	input := []*Edge{nil, validCall, invalidParam, validStructural, invalidLocal}

	kept, rejected := FilterStructuralEdgeViolations(input)
	if len(input) != 5 || input[2] != invalidParam || input[4] != invalidLocal {
		t.Fatalf("filter mutated input: %#v", input)
	}
	if len(kept) != 3 || kept[0] != nil || kept[1] != validCall || kept[2] != validStructural {
		t.Fatalf("unexpected kept partition: %#v", kept)
	}
	if len(rejected) != 2 || rejected[0] != invalidParam || rejected[1] != invalidLocal {
		t.Fatalf("unexpected rejected partition: %#v", rejected)
	}
}

func TestGraphStructuralIntegrityExactOnceAndRepoAttribution(t *testing.T) {
	g := New()
	g.AddNode(&Node{ID: "repo/a.go::Source", Kind: KindFunction, RepoPrefix: "repo-a"})
	g.AddEdge(invalidStructuralEdge("repo/a.go::Source", "repo/a.go::Target#param:x", EdgeImplements, "LSP_DISPATCH"))
	g.AddBatch(
		[]*Node{{ID: "repo/b.go::Source", Kind: KindFunction, RepoPrefix: "repo-b"}},
		[]*Edge{
			invalidStructuralEdge("repo/b.go::Source", "repo/b.go::Target#local:y", EdgeOverrides, "AST"),
			invalidStructuralEdge("repo/b.go::Source", "repo/b.go::Target#param:ok", EdgeCalls, "AST"),
		},
	)

	snapshot := g.StructuralIntegritySnapshot(StructuralIntegritySnapshotOptions{IncludeAttribution: true, IncludeSamples: true})
	if snapshot.Totals.WriteRejected != 2 || snapshot.Totals.ReadSuppressed != 0 {
		t.Fatalf("unexpected totals: %+v", snapshot.Totals)
	}
	if len(snapshot.Attribution) != 2 {
		t.Fatalf("want two attributed dimensions, got %+v", snapshot.Attribution)
	}
	if snapshot.Attribution[0].Repo != "repo-a" || snapshot.Attribution[0].Origin != "lsp_dispatch" {
		t.Fatalf("existing source attribution mismatch: %+v", snapshot.Attribution[0])
	}
	if snapshot.Attribution[1].Repo != "repo-b" || snapshot.Attribution[1].Origin != "ast" {
		t.Fatalf("batch input attribution mismatch: %+v", snapshot.Attribution[1])
	}
	if got := len(g.AllEdges()); got != 1 || g.AllEdges()[0].Kind != EdgeCalls {
		t.Fatalf("valid call-to-param edge must survive; edges=%+v", g.AllEdges())
	}

	repoA := g.StructuralIntegritySnapshot(StructuralIntegritySnapshotOptions{
		RepoPrefixes: []string{"repo-a"}, RepoScopeActive: true,
		IncludeAttribution: true, IncludeSamples: true,
	})
	if repoA.Totals.WriteRejected != 1 || len(repoA.Attribution) != 1 || len(repoA.Samples) != 1 {
		t.Fatalf("scoped snapshot leaked or undercounted: %+v", repoA)
	}
	if repoA.Attribution[0].Repo != "repo-a" || repoA.Samples[0].Repo != "repo-a" {
		t.Fatalf("scoped snapshot leaked another repository: %+v", repoA)
	}
	emptyScope := g.StructuralIntegritySnapshot(StructuralIntegritySnapshotOptions{RepoScopeActive: true, IncludeSamples: true})
	if !emptyScope.Totals.Empty() || len(emptyScope.Samples) != 0 {
		t.Fatalf("active empty scope must match nothing: %+v", emptyScope)
	}
}

func TestStructuralIntegrityShadowForwardsAttemptOnce(t *testing.T) {
	owner := New()
	shadow := NewStructuralIntegrityShadow(owner, "repo-shadow", StructuralPathShadowCold)
	shadow.AddBatch(nil, []*Edge{
		invalidStructuralEdge("src", "dst#param:x", EdgeImplements, "LSP"),
		invalidStructuralEdge("src", "dst#param:x", EdgeCalls, "LSP"),
	})

	ownerSnapshot := owner.StructuralIntegritySnapshot(StructuralIntegritySnapshotOptions{IncludeAttribution: true})
	if ownerSnapshot.Totals.WriteRejected != 1 || len(ownerSnapshot.Attribution) != 1 {
		t.Fatalf("shadow rejection was not forwarded exactly once: %+v", ownerSnapshot)
	}
	got := ownerSnapshot.Attribution[0]
	if got.Repo != "repo-shadow" || got.Path != StructuralPathShadowCold || got.Count != 1 {
		t.Fatalf("shadow attribution mismatch: %+v", got)
	}
	if shadow.StructuralIntegritySnapshot(StructuralIntegritySnapshotOptions{}).Totals.WriteRejected != 0 {
		t.Fatal("forwarding shadow must not retain a duplicate local event")
	}
	if gotEdges := shadow.AllEdges(); len(gotEdges) != 1 || gotEdges[0].Kind != EdgeCalls {
		t.Fatalf("valid edge missing from shadow: %+v", gotEdges)
	}
}

func TestStructuralIntegrityMeterBoundsDimensionsAndSignalsScopedLowerBound(t *testing.T) {
	var meter StructuralIntegrityMeter
	longOrigin := "  " + strings.Repeat("Ab", 40) + "  "
	for i := 0; i < structuralIntegrityMaxRepos+1; i++ {
		origin := fmt.Sprintf("origin-%03d", i)
		if i == 0 {
			origin = longOrigin
		}
		meter.Record(StructuralIntegrityEvent{
			Direction: StructuralDropWrite,
			Path:      StructuralPathGraphAddEdge,
			Repo:      fmt.Sprintf("repo-%03d", i),
			Kind:      EdgeImplements,
			Reason:    StructuralReasonParameterTarget,
			Origin:    origin,
			From:      fmt.Sprintf("from-%03d", i),
			To:        fmt.Sprintf("to-%03d#param:x", i),
			Line:      i,
		})
	}

	all := meter.Snapshot(StructuralIntegritySnapshotOptions{IncludeAttribution: true, IncludeSamples: true})
	if all.Totals.WriteRejected != structuralIntegrityMaxRepos+1 {
		t.Fatalf("global atomics must remain exact: %+v", all.Totals)
	}
	if len(all.Attribution) != structuralIntegrityMaxDimensions || all.AttributionOverflow != 1 {
		t.Fatalf("dimension bound mismatch: len=%d overflow=%d", len(all.Attribution), all.AttributionOverflow)
	}
	if len(all.Samples) != structuralIntegrityMaxSamples || all.SampleOverflow != structuralIntegrityMaxRepos+1-structuralIntegrityMaxSamples {
		t.Fatalf("sample bound mismatch: len=%d overflow=%d", len(all.Samples), all.SampleOverflow)
	}
	if all.RepoTotalsOverflow != 1 {
		t.Fatalf("repo total overflow not surfaced: %+v", all)
	}
	wantOrigin := strings.ToLower(strings.TrimSpace(longOrigin))[:structuralIntegrityOriginLimit]
	if all.Attribution[0].Origin != wantOrigin {
		t.Fatalf("origin must be normalized and bounded: got %q want %q", all.Attribution[0].Origin, wantOrigin)
	}

	overflowedRepo := meter.Snapshot(StructuralIntegritySnapshotOptions{
		RepoPrefixes:    []string{fmt.Sprintf("repo-%03d", structuralIntegrityMaxRepos)},
		RepoScopeActive: true,
	})
	if overflowedRepo.Totals.WriteRejected != 0 || !overflowedRepo.TotalsLowerBound || overflowedRepo.RepoTotalsOverflow != 1 {
		t.Fatalf("scoped undercount must be explicit: %+v", overflowedRepo)
	}
}

func TestStructuralIntegrityMeterConcurrentRecordAndSnapshot(t *testing.T) {
	var meter StructuralIntegrityMeter
	const goroutines = 16
	const perGoroutine = 200
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(worker int) {
			defer wg.Done()
			for j := 0; j < perGoroutine; j++ {
				meter.Record(StructuralIntegrityEvent{
					Direction: StructuralDropRead,
					Path:      StructuralPathSQLiteFullRead,
					Repo:      fmt.Sprintf("repo-%d", worker%4),
					Kind:      EdgeExtends,
					Reason:    StructuralReasonLocalTarget,
					Origin:    "legacy",
					From:      fmt.Sprintf("from-%d", worker),
					To:        fmt.Sprintf("to-%d#local:x", j),
				})
				if j%17 == 0 {
					_ = meter.Snapshot(StructuralIntegritySnapshotOptions{IncludeAttribution: true, IncludeSamples: true})
				}
			}
		}(i)
	}
	wg.Wait()
	got := meter.Snapshot(StructuralIntegritySnapshotOptions{})
	if got.Totals.ReadSuppressed != goroutines*perGoroutine {
		t.Fatalf("lost concurrent events: got %d want %d", got.Totals.ReadSuppressed, goroutines*perGoroutine)
	}
}
