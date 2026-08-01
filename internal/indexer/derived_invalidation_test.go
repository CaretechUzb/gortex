package indexer

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestDerivedPlanForBodyOnlyDelta(t *testing.T) {
	fingerprints := derivedFingerprints{
		declarations: "decl",
		imports:      "imports",
		runtime:      "runtime",
		artifacts:    "artifacts",
	}

	plan := derivedPlanForDelta(fingerprints, fingerprints, true, "gortex/internal/indexer/example.go", nil, nil)

	if plan.Flags != 0 {
		t.Fatalf("body-only delta flags = %v, want 0", plan.Flags)
	}
	if plan.BodyOnlyFiles != 1 {
		t.Fatalf("body-only files = %d, want 1", plan.BodyOnlyFiles)
	}
	if len(plan.Files) != 1 || plan.Files[0] != "gortex/internal/indexer/example.go" {
		t.Fatalf("files = %v, want exact changed file", plan.Files)
	}
	if plan.LegacyFallback {
		t.Fatal("body-only delta must not request legacy fallback")
	}
}

func TestDerivedPlanForDeltaDoesNotTreatCreateAsLegacy(t *testing.T) {
	fresh := completeDerivedFingerprints("fresh")
	plan := derivedPlanForDelta(
		derivedFingerprints{}, fresh, true, "repo/new.go", nil,
		[]*graph.Node{{ID: "repo/new.go", Kind: graph.KindFile}},
	)

	if plan.LegacyFallback {
		t.Fatal("new file with no prior fingerprint side must not require legacy fallback")
	}
	assertAllDerivedFamiliesInvalidated(t, plan)
}

func TestDerivedPlanForDeltaDoesNotTreatDeleteAsLegacy(t *testing.T) {
	prior := completeDerivedFingerprints("prior")
	plan := derivedPlanForDelta(
		prior, derivedFingerprints{}, true, "repo/deleted.go",
		[]*graph.Node{{ID: "repo/deleted.go", Kind: graph.KindFile}}, nil,
	)

	if plan.LegacyFallback {
		t.Fatal("deleted file with no fresh fingerprint side must not require legacy fallback")
	}
	assertAllDerivedFamiliesInvalidated(t, plan)
}

func TestDerivedPlanForDeltaTreatsPresentUnfingerprintedSideAsLegacy(t *testing.T) {
	fresh := completeDerivedFingerprints("fresh")
	plan := derivedPlanForDelta(
		derivedFingerprints{}, fresh, true, "repo/upgraded.go",
		[]*graph.Node{{ID: "repo/upgraded.go", Kind: graph.KindFile}},
		[]*graph.Node{{ID: "repo/upgraded.go", Kind: graph.KindFile}},
	)

	if !plan.LegacyFallback {
		t.Fatal("present prior file without fingerprints must require legacy fallback")
	}
	assertAllDerivedFamiliesInvalidated(t, plan)
}

func TestDerivedPlanForDeltaMarksCSharpHierarchyChanges(t *testing.T) {
	prior := completeDerivedFingerprints("prior")
	fresh := completeDerivedFingerprints("fresh")
	plan := derivedPlanForDelta(
		prior, fresh, true, "repo/model.cs",
		hierarchyTestNodes("repo/model.cs", "csharp", graph.KindType),
		hierarchyTestNodes("repo/model.cs", "csharp", graph.KindType),
	)
	if !plan.CSharpHierarchyChanged {
		t.Fatal("C# declaration change must mark the hierarchy frontier")
	}
}

func TestDerivedPlanForDeltaMarksDeletedCSharpHierarchy(t *testing.T) {
	plan := derivedPlanForDelta(
		completeDerivedFingerprints("prior"), derivedFingerprints{}, true, "repo/model.cs",
		hierarchyTestNodes("repo/model.cs", "csharp", graph.KindInterface), nil,
	)
	if !plan.CSharpHierarchyChanged {
		t.Fatal("deleted C# type/interface must mark the hierarchy frontier from prior nodes")
	}
}

func TestDerivedPlanForDeltaDoesNotMarkNonCSharpHierarchy(t *testing.T) {
	plan := derivedPlanForDelta(
		derivedFingerprints{}, completeDerivedFingerprints("fresh"), true, "repo/model.go", nil,
		hierarchyTestNodes("repo/model.go", "go", graph.KindType),
	)
	if plan.CSharpHierarchyChanged {
		t.Fatal("non-C# type changes must not widen the C# demotion frontier")
	}
}

func TestDerivedInvalidationPlanMergePreservesCSharpHierarchyChange(t *testing.T) {
	var merged DerivedInvalidationPlan
	merged.Merge(DerivedInvalidationPlan{CSharpHierarchyChanged: true})
	merged.Merge(DerivedInvalidationPlan{})
	if !merged.CSharpHierarchyChanged {
		t.Fatal("merged plan lost C# hierarchy invalidation")
	}
}

func hierarchyTestNodes(path, language string, kind graph.NodeKind) []*graph.Node {
	return []*graph.Node{
		{ID: path, Kind: graph.KindFile, FilePath: path, Language: language},
		{ID: path + "::declaration", Kind: kind, FilePath: path, Language: language},
	}
}

func completeDerivedFingerprints(prefix string) derivedFingerprints {
	return derivedFingerprints{
		declarations: prefix + "-declarations",
		imports:      prefix + "-imports",
		runtime:      prefix + "-runtime",
		artifacts:    prefix + "-artifacts",
	}
}

func assertAllDerivedFamiliesInvalidated(t *testing.T, plan DerivedInvalidationPlan) {
	t.Helper()
	want := DerivedInvalidatesDeclarations | DerivedInvalidatesImports | DerivedInvalidatesRuntime | DerivedInvalidatesArtifacts
	if plan.Flags&want != want {
		t.Fatalf("derived invalidation flags = %b, want all base families %b", plan.Flags, want)
	}
}
