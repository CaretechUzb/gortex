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
