package mcp

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

func localizationBodyMentionTestNode(file, name string, line int) *graph.Node {
	return &graph.Node{
		ID:        file + "::" + name,
		Name:      name,
		Kind:      graph.KindFunction,
		FilePath:  file,
		StartLine: line,
	}
}

func TestPromoteLocalizationBodyMentionsUsesRealPageDeclarations(t *testing.T) {
	sibling := localizationBodyMentionTestNode("src/a.go", "sibling", 22)
	fragment := localizationBodyMentionTestNode("src/a.go", "Foo", 30)
	external := localizationBodyMentionTestNode("src/b.go", "External", 7)
	reader := &localizationDeclarationSpyReader{
		files: map[string][]*graph.Node{
			"src/a.go": {sibling, fragment},
			"src/b.go": {external},
		},
		fileCalls: make(map[string]int),
	}
	envelope := localizationExploreEnvelope{
		Completion: newLocalizationCompletion(true, ""),
		Files:      []string{"src/a.go", "src/b.go"},
		Symbols:    []string{"src/a.go::owner", "src/b.go::page"},
		Evidence: []localizationEvidence{
			{
				Rank: 1, ID: "src/a.go::owner", Name: "owner", Kind: string(graph.KindFunction),
				File: "src/a.go", Line: 1, Source: "func owner() { sibling(); External(); foobar() }",
				Callees: []string{external.ID},
			},
			{Rank: 2, ID: "src/b.go::page", Name: "page", Kind: string(graph.KindFunction), File: "src/b.go", Line: 1},
		},
	}
	digest := newLocalizationEvidenceDigestForTask("fix sibling and External", envelope)

	packed, packedDigest := promoteLocalizationBodyMentions(
		"fix sibling and External", envelope, newLocalizationDeclarationTestCache(reader), 1<<20, digest,
	)

	require.Equal(t, []string{sibling.ID, external.ID}, []string{packed.Evidence[2].ID, packed.Evidence[3].ID})
	require.Equal(t, localizationProvenanceTaskMention, packed.Evidence[2].Provenance)
	require.Equal(t, localizationProvenanceTaskMention, packed.Evidence[3].Provenance)
	require.Empty(t, packed.Evidence[2].Source)
	require.NotContains(t, packed.Symbols, fragment.ID, "identifier fragments must not promote declarations")
	require.Equal(t, map[string]int{"src/a.go": 1, "src/b.go": 1}, reader.fileCalls)
	require.Len(t, packed.Files, len(packed.Evidence))
	require.Len(t, packed.Symbols, len(packed.Evidence))
	require.True(t, localizationBodyMentionDigestContains(packedDigest, sibling.ID))
	require.True(t, localizationBodyMentionDigestContains(packedDigest, external.ID))

	seatedMentions := 0
	for _, row := range localizationFinalResponseRows("fix sibling and External", nil, packedDigest.Evidence) {
		if row.row.Provenance == localizationProvenanceTaskMention {
			require.True(t, row.primary, "a body-derived declaration the task names seats as primary")
			seatedMentions++
		}
	}
	require.Equal(t, 2, seatedMentions)
}

func TestPromoteLocalizationBodyMentionsIsCappedAndCached(t *testing.T) {
	declarations := make([]*graph.Node, 0, localizationBodyMentionCap+4)
	var source strings.Builder
	for index := 0; index < localizationBodyMentionCap+4; index++ {
		name := fmt.Sprintf("Call%d", index)
		declarations = append(declarations, localizationBodyMentionTestNode("src/a.go", name, index+10))
		fmt.Fprintf(&source, "%s(); ", name)
	}
	reader := &localizationDeclarationSpyReader{
		files:     map[string][]*graph.Node{"src/a.go": declarations},
		fileCalls: make(map[string]int),
	}
	envelope := localizationExploreEnvelope{
		Completion: newLocalizationCompletion(true, ""),
		Files:      []string{"src/a.go"},
		Symbols:    []string{"src/a.go::owner"},
		Evidence: []localizationEvidence{{
			Rank: 1, ID: "src/a.go::owner", Name: "owner", Kind: string(graph.KindFunction),
			File: "src/a.go", Line: 1, Source: source.String(),
		}},
	}

	packed, _ := promoteLocalizationBodyMentions(
		"trace the calls", envelope, newLocalizationDeclarationTestCache(reader), 1<<20,
		newLocalizationEvidenceDigestForTask("trace the calls", envelope),
	)

	require.Len(t, packed.Evidence, 1+localizationBodyMentionCap)
	require.Equal(t, 1, reader.fileCalls["src/a.go"])
	for index, row := range packed.Evidence {
		require.Equal(t, index+1, row.Rank)
	}
}

func TestPromoteLocalizationBodyMentionsUsesPackedSourceWindow(t *testing.T) {
	sibling := localizationBodyMentionTestNode("src/a.go", "sibling", 22)
	reader := &localizationDeclarationSpyReader{
		files:     map[string][]*graph.Node{"src/a.go": {sibling}},
		fileCalls: make(map[string]int),
	}
	envelope := localizationExploreEnvelope{
		Completion: newLocalizationCompletion(true, ""),
		Files:      []string{"src/a.go"},
		Symbols:    []string{"src/a.go::owner"},
		Evidence: []localizationEvidence{{
			Rank: 1, ID: "src/a.go::owner", Name: "owner", Kind: string(graph.KindFunction),
			File: "src/a.go", Line: 1,
		}},
		SourceWindow: &localizationSourceWindow{
			Path: "src/a.go", StartLine: 20, EndLine: 24, MatchLine: 21,
			Content: "func call() { sibling() }", AnchorSymbol: "src/a.go::owner",
		},
	}

	packed, packedDigest := promoteLocalizationBodyMentions(
		"find sibling", envelope, newLocalizationDeclarationTestCache(reader), 1<<20,
		newLocalizationEvidenceDigestForTask("find sibling", envelope),
	)

	require.Contains(t, packed.Symbols, sibling.ID)
	require.True(t, localizationBodyMentionDigestContains(packedDigest, sibling.ID))
	for _, row := range localizationFinalResponseRows("find sibling", nil, packedDigest.Evidence) {
		if row.row.ID == sibling.ID {
			require.True(t, row.primary, "a window-derived row the task names seats as primary")
		}
	}
}

func TestPromoteLocalizationBodyMentionsBoundsDeclarationsPerFile(t *testing.T) {
	inside := localizationBodyMentionTestNode("src/generated.go", "Inside", 1)
	outside := localizationBodyMentionTestNode("src/generated.go", "Outside", localizationBodyMentionDeclarationCap+1)
	declarations := make([]*graph.Node, 0, localizationBodyMentionDeclarationCap+1)
	declarations = append(declarations, inside)
	for index := 1; index < localizationBodyMentionDeclarationCap; index++ {
		declarations = append(declarations, localizationBodyMentionTestNode("src/generated.go", fmt.Sprintf("Filler%d", index), index+1))
	}
	declarations = append(declarations, outside)
	reader := &localizationDeclarationSpyReader{
		files:     map[string][]*graph.Node{"src/generated.go": declarations},
		fileCalls: make(map[string]int),
	}
	envelope := localizationExploreEnvelope{
		Completion: newLocalizationCompletion(true, ""),
		Files:      []string{"src/generated.go"},
		Symbols:    []string{"src/generated.go::owner"},
		Evidence: []localizationEvidence{{
			Rank: 1, ID: "src/generated.go::owner", Name: "owner", Kind: string(graph.KindFunction),
			File: "src/generated.go", Line: 1, Source: "Inside(); Outside()",
		}},
	}

	packed, _ := promoteLocalizationBodyMentions(
		"find Inside and Outside", envelope, newLocalizationDeclarationTestCache(reader), 1<<20,
		newLocalizationEvidenceDigestForTask("find Inside and Outside", envelope),
	)

	require.Contains(t, packed.Symbols, inside.ID)
	require.NotContains(t, packed.Symbols, outside.ID)
	require.Equal(t, 1, reader.fileCalls["src/generated.go"])
	require.Len(t, reader.calls, 1)
	require.Equal(t, localizationBodyMentionDeclarationCap, reader.calls[0].limit)
}

func TestPromoteLocalizationBodyMentionsHonorsEnvelopeBudget(t *testing.T) {
	sibling := localizationBodyMentionTestNode("src/a.go", "sibling", 22)
	reader := &localizationDeclarationSpyReader{
		files:     map[string][]*graph.Node{"src/a.go": {sibling}},
		fileCalls: make(map[string]int),
	}
	envelope := localizationExploreEnvelope{
		Completion: newLocalizationCompletion(true, ""),
		Files:      []string{"src/a.go"},
		Symbols:    []string{"src/a.go::owner"},
		Evidence: []localizationEvidence{{
			Rank: 1, ID: "src/a.go::owner", Name: "owner", Kind: string(graph.KindFunction),
			File: "src/a.go", Line: 1, Source: "func owner() { sibling() }",
		}},
	}
	body, err := json.Marshal(envelope)
	require.NoError(t, err)
	digest := newLocalizationEvidenceDigestForTask("find sibling", envelope)

	packed, packedDigest := promoteLocalizationBodyMentions(
		"find sibling", envelope, newLocalizationDeclarationTestCache(reader), len(body), digest,
	)

	require.Equal(t, envelope, packed)
	require.Same(t, digest, packedDigest)
}

func TestPromoteLocalizationBodyMentionsBoundsTheTenFileProjection(t *testing.T) {
	reader := &localizationDeclarationSpyReader{files: make(map[string][]*graph.Node)}
	envelope := localizationExploreEnvelope{Completion: newLocalizationCompletion(true, "")}
	for index := 0; index < localizationBodyMentionFileCap; index++ {
		file := fmt.Sprintf("src/file-%02d.go", index)
		ownerID := file + "::owner"
		reader.files[file] = []*graph.Node{
			localizationBodyMentionTestNode(file, "Mentioned", index+2),
		}
		envelope.Files = append(envelope.Files, file)
		envelope.Symbols = append(envelope.Symbols, ownerID)
		envelope.Evidence = append(envelope.Evidence, localizationEvidence{
			Rank: index + 1, ID: ownerID, Name: "owner", Kind: string(graph.KindFunction),
			File: file, Line: 1, Source: "func owner() { Mentioned() }",
		})
	}

	promoteLocalizationBodyMentions(
		"find Mentioned", envelope, newLocalizationDeclarationTestCache(reader), 1<<20,
		newLocalizationEvidenceDigestForTask("find Mentioned", envelope),
	)

	require.Len(t, reader.calls, localizationBodyMentionFileCap)
	consumed := 0
	for _, call := range reader.calls {
		require.LessOrEqual(t, call.limit, localizationBodyMentionDeclarationCap)
		consumed += call.limit
	}
	require.LessOrEqual(t, consumed, localizationFileRequestLimit)
}

func TestLocalizationDigestReservesBodyMentionTail(t *testing.T) {
	evidence := make([]localizationEvidence, 0, localizationReplayEvidenceLimit+1)
	for index := 0; index < localizationReplayEvidenceLimit; index++ {
		evidence = append(evidence, localizationEvidence{
			Rank: index + 1, ID: fmt.Sprintf("src/a.go::ordinary%d", index),
			Name: fmt.Sprintf("ordinary%d", index), Kind: string(graph.KindFunction), File: "src/a.go", Line: index + 1,
		})
	}
	bodyID := "src/a.go::mentioned"
	evidence = append(evidence, localizationEvidence{
		Rank: len(evidence) + 1, ID: bodyID, Name: "mentioned", Kind: string(graph.KindFunction),
		File: "src/a.go", Line: 99, Provenance: localizationProvenanceBodyMention,
	})

	digest := newLocalizationEvidenceDigestForTask("find mentioned", localizationExploreEnvelope{Evidence: evidence})

	require.Len(t, digest.Evidence, localizationReplayEvidenceLimit)
	require.True(t, localizationBodyMentionDigestContains(digest, bodyID))
	require.NotContains(t, digest.Symbols, "src/a.go::ordinary15")
}

func TestShedLocalizationDigestSupportingOnlyBeforeOrdinaryRows(t *testing.T) {
	rows := []localizationDigestRow{
		{ID: "ordinary", Provenance: "ranked"},
		{ID: "mentioned", Provenance: localizationProvenanceBodyMention, supportingOnly: true},
		{ID: "adjacent", Provenance: localizationProvenanceDirectAdjacency, supportingOnly: true},
		{ID: "protected", Provenance: localizationProvenanceDirectAdjacency, supportingOnly: true, authorizationPriority: true},
		{ID: "tail", Provenance: "ranked"},
	}

	retained, removed := shedLocalizationDigestSupportingOnly(rows)
	require.True(t, removed)
	require.Equal(t, []string{"ordinary", "mentioned", "protected", "tail"}, []string{retained[0].ID, retained[1].ID, retained[2].ID, retained[3].ID})

	retained, removed = shedLocalizationDigestSupportingOnly(retained)
	require.True(t, removed)
	require.Equal(t, []string{"ordinary", "protected", "tail"}, []string{retained[0].ID, retained[1].ID, retained[2].ID})

	retained, removed = shedLocalizationDigestSupportingOnly(retained)
	require.False(t, removed)
	require.Equal(t, []string{"ordinary", "protected", "tail"}, []string{retained[0].ID, retained[1].ID, retained[2].ID})
}

func TestPromoteLocalizationTaskNamedDeclarationWithoutBodyMention(t *testing.T) {
	target := localizationBodyMentionTestNode("src/a.go", "RotateJournal", 40)
	reader := &localizationDeclarationSpyReader{
		files:     map[string][]*graph.Node{"src/a.go": {target}},
		fileCalls: make(map[string]int),
	}
	envelope := localizationExploreEnvelope{
		Completion: newLocalizationCompletion(true, ""),
		Files:      []string{"src/a.go"},
		Symbols:    []string{"src/a.go::owner"},
		Evidence: []localizationEvidence{{
			Rank: 1, ID: "src/a.go::owner", Name: "owner", Kind: string(graph.KindFunction),
			File: "src/a.go", Line: 1, Source: "func owner() { unrelated() }",
		}},
	}
	task := "RotateJournal drops entries during shutdown"

	packed, packedDigest := promoteLocalizationBodyMentions(
		task, envelope, newLocalizationDeclarationTestCache(reader), 1<<20,
		newLocalizationEvidenceDigestForTask(task, envelope),
	)

	require.Contains(t, packed.Symbols, target.ID)
	var provenance string
	for _, row := range packed.Evidence {
		if row.ID == target.ID {
			provenance = row.Provenance
		}
	}
	require.Equal(t, localizationProvenanceTaskMention, provenance)
	seated := false
	for _, row := range localizationFinalResponseRows(task, nil, packedDigest.Evidence) {
		if row.row.ID == target.ID {
			seated = row.primary
		}
	}
	require.True(t, seated, "a task-named declaration seats as primary without a body mention")
}
