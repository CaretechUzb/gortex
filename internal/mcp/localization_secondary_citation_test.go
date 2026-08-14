package mcp

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func localizationSecondaryCitationTestTarget(id, name string) exploreTarget {
	return exploreTarget{node: &graph.Node{
		ID: id, Name: name, Kind: graph.KindFunction,
		FilePath: "src/citations.go", StartLine: 1,
	}}
}

func TestLocalizationSecondaryTaskCitedTargetConsumesRepresentedMention(t *testing.T) {
	format := localizationSecondaryCitationTestTarget("src/citations.go::format", "format")
	format.node.QualName = "fmt::format"
	formatOverload := localizationSecondaryCitationTestTarget("src/citations.go::format#2", "format")
	formatOverload.node.QualName = "other::format"
	macro := localizationSecondaryCitationTestTarget("src/citations.go::FMT_FORMAT_AS", "FMT_FORMAT_AS")
	later := localizationSecondaryCitationTestTarget("src/citations.go::LATER_MACRO", "LATER_MACRO")
	targets := []exploreTarget{formatOverload, later, macro, format}
	task := "The call fmt::format(value) should use FMT_FORMAT_AS(Type) before LATER_MACRO(Type)."

	got, ok := localizationSecondaryTaskCitedTarget(task, targets, []exploreTarget{format})
	if !ok {
		t.Fatal("expected an independent cited target")
	}
	if got.node == nil || got.node.ID != macro.node.ID {
		t.Fatalf("secondary target = %#v, want %q", got.node, macro.node.ID)
	}
	if targets[0].node.ID != formatOverload.node.ID || targets[1].node.ID != later.node.ID {
		t.Fatalf("helper mutated caller-owned order: %v", exploreLocalizationTargetSymbols(targets))
	}
}

func TestLocalizationSecondaryTaskCitedTargetRejectsLooseProse(t *testing.T) {
	target := localizationSecondaryCitationTestTarget("src/citations.go::target", "target")
	if got, ok := localizationSecondaryTaskCitedTarget(
		"Investigate the target behavior in src/citations.go.",
		[]exploreTarget{target}, nil,
	); ok {
		t.Fatalf("loose prose selected %#v", got.node)
	}
	perform := localizationSecondaryCitationTestTarget("src/citations.go::performHydrate", "performHydrate")
	if got, ok := localizationSecondaryTaskCitedTarget(
		"Investigate performHydrateLater during startup.",
		[]exploreTarget{perform}, nil,
	); ok {
		t.Fatalf("identifier suffix selected %#v", got.node)
	}
}

func TestLocalizationEvidenceTargetsFromDraftReservesOneIndependentCitation(t *testing.T) {
	format := localizationSecondaryCitationTestTarget("src/citations.go::format", "format")
	format.sourceRange = true
	formatOverload := localizationSecondaryCitationTestTarget("src/citations.go::format#2", "format")
	ordinaryA := localizationSecondaryCitationTestTarget("src/citations.go::ordinaryA", "ordinaryA")
	ordinaryB := localizationSecondaryCitationTestTarget("src/citations.go::ordinaryB", "ordinaryB")
	ordinaryC := localizationSecondaryCitationTestTarget("src/citations.go::ordinaryC", "ordinaryC")
	ordinaryD := localizationSecondaryCitationTestTarget("src/citations.go::ordinaryD", "ordinaryD")
	later := localizationSecondaryCitationTestTarget("src/citations.go::LATER_MACRO", "LATER_MACRO")
	macro := localizationSecondaryCitationTestTarget("src/citations.go::FMT_FORMAT_AS", "FMT_FORMAT_AS")
	targets := []exploreTarget{
		format, formatOverload, ordinaryA, ordinaryB, ordinaryC, ordinaryD, later, macro,
	}
	task := "The call fmt::format(value) should use FMT_FORMAT_AS(Type) before LATER_MACRO(Type)."

	ordered := localizationEvidenceTargetsFromDraft(task, "", targets, []exploreDraftEntry{})
	if len(ordered) != len(targets) {
		t.Fatalf("ordered target count = %d, want %d", len(ordered), len(targets))
	}
	got := exploreLocalizationTargetSymbols(ordered)
	wantPrefix := []string{format.node.ID, macro.node.ID, formatOverload.node.ID}
	for index, want := range wantPrefix {
		if got[index] != want {
			t.Fatalf("ordered IDs = %v, want prefix %v", got, wantPrefix)
		}
	}
	laterIndex := -1
	for index, id := range got {
		if id == later.node.ID {
			laterIndex = index
			break
		}
	}
	if laterIndex <= 1 {
		t.Fatalf("second independent citation was also reserved: %v", got)
	}

	envelope := localizationExploreEnvelope{Evidence: make([]localizationEvidence, 0, len(ordered))}
	for index, target := range ordered {
		node := target.node
		envelope.Evidence = append(envelope.Evidence, localizationEvidence{
			Rank: index + 1, ID: node.ID, Name: node.Name, QualName: node.QualName,
			Kind: string(node.Kind), File: nodeDisplayPath(node), Line: node.StartLine,
		})
	}
	digest := newLocalizationEvidenceDigestForTask(task, envelope)
	freezeLocalizationPrimaryCohort(task, &envelope, digest)
	if len(digest.primaryIDs) != localizationFinalResponsePrimaryLimit {
		t.Fatalf("PRIMARY count = %d, want %d: %v", len(digest.primaryIDs), localizationFinalResponsePrimaryLimit, digest.primaryIDs)
	}
	macroPrimary := false
	for _, id := range digest.primaryIDs {
		macroPrimary = macroPrimary || id == macro.node.ID
	}
	if !macroPrimary {
		t.Fatalf("secondary cited target not frozen as PRIMARY: %v", digest.primaryIDs)
	}
	for _, row := range envelope.Evidence {
		if row.ID == macro.node.ID && (row.Provenance != "" || row.taskCitedPrimaryEligible) {
			t.Fatalf("secondary citation gained authority metadata: %#v", row)
		}
	}
}
