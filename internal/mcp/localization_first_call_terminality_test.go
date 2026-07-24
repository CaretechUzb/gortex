package mcp

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
)

func firstCallEvidence(id, name, file, source string) localizationEvidence {
	return localizationEvidence{
		Rank: 1, ID: id, Name: name, QualName: name, Kind: "method",
		File: file, Line: 12, Signature: "func " + name + "(path string) ([]byte, error)",
		Source: source,
	}
}

func TestLocalizationEvidencePolicyKeepsRankedReadinessTerminal(t *testing.T) {
	node := &graph.Node{
		ID: "repo/storage/disk.go::DiskStorage.Load", Name: "DiskStorage.Load",
		Kind: graph.KindMethod, FilePath: "storage/disk.go",
	}
	envelope := localizationExploreEnvelope{
		Completion: newLocalizationCompletion(true, ""),
		Symbols:    []string{node.ID},
		Evidence: []localizationEvidence{
			firstCallEvidence(node.ID, "DiskStorage.Load", "storage/disk.go", ""),
		},
	}
	finalized := localizationFinalizeCompletionEvidence(
		"DiskStorage.Load truncates large payloads",
		envelope.Completion, []exploreTarget{{node: node}}, envelope,
	)
	// Without one of the hard provenance shapes the page keeps its ranked
	// verdict and only forfeits enforcement — it never bills another call.
	require.Equal(t, localizationStateAnswerReady, finalized.State)
	require.Equal(t, "respond", finalized.RequiredAction)
	require.Equal(t, 0, finalized.AllowedToolCalls)
	require.False(t, finalized.Enforceable)
	require.True(t, localizationContractFor(finalized).Terminal)
}

func TestLocalizationExactReadSatisfiedOnlyByPackedAlignedBody(t *testing.T) {
	const symbol = "repo/storage/disk.go::DiskStorage.Load"
	task := `DiskStorage.Load returns a truncated payload for large files`
	body := "func (s *DiskStorage) Load(path string) ([]byte, error) { return s.read(path) }"

	packed := localizationExploreEnvelope{
		Completion: newLocalizationExactReadCompletion(symbol, false),
		Evidence: []localizationEvidence{
			firstCallEvidence(symbol, "DiskStorage.Load", "storage/disk.go", body),
		},
	}
	require.True(t, localizationExactReadSatisfiedByEnvelope(task, packed),
		"a packed, task-aligned body is exactly what the prescribed read would return")

	withoutBody := packed
	withoutBody.Evidence = []localizationEvidence{
		firstCallEvidence(symbol, "DiskStorage.Load", "storage/disk.go", ""),
	}
	require.False(t, localizationExactReadSatisfiedByEnvelope(task, withoutBody),
		"a signature-only row still owes the caller the body")

	absent := packed
	absent.Evidence = []localizationEvidence{
		firstCallEvidence("repo/storage/cloud.go::CloudStorage.Load", "CloudStorage.Load",
			"storage/cloud.go", body),
	}
	require.False(t, localizationExactReadSatisfiedByEnvelope(task, absent),
		"another symbol's body cannot satisfy the authorized read")

	unset := packed
	unset.Completion = newLocalizationCompletion(true, "")
	require.False(t, localizationExactReadSatisfiedByEnvelope(task, unset))
}

func TestExploreLocalizationTestLaneNodeExemptsTestIntentAndExplicitAnchor(t *testing.T) {
	pathOnlyTest := &graph.Node{
		ID: "repo/Storage.Tests.Shared/LoadCases.cs::LoadCases.Truncated",
		Name: "LoadCases.Truncated", Kind: graph.KindMethod,
		FilePath: "Storage.Tests.Shared/LoadCases.cs",
	}
	production := &graph.Node{
		ID: "repo/storage/disk.go::DiskStorage.Load", Name: "DiskStorage.Load",
		Kind: graph.KindMethod, FilePath: "storage/disk.go",
	}

	// A path-only test marker the indexer never stamped still leaves the
	// production head slots to production code.
	require.True(t, exploreLocalizationTestLaneNode("truncated payload on load", pathOnlyTest))
	require.False(t, exploreLocalizationTestLaneNode("truncated payload on load", production))

	// …unless the request is about test code, or names the candidate itself.
	require.False(t, exploreLocalizationTestLaneNode("which test covers truncated loads", pathOnlyTest))
	require.False(t, exploreLocalizationTestLaneNode(
		"repo/Storage.Tests.Shared/LoadCases.cs::LoadCases.Truncated fails", pathOnlyTest))

	stamped := &graph.Node{
		ID: "repo/storage/disk_test.go::TestLoad", Name: "TestLoad", Kind: graph.KindFunction,
		FilePath: "storage/disk_test.go", Meta: map[string]any{"is_test": true},
	}
	require.True(t, exploreLocalizationTestLaneNode("truncated payload on load", stamped))
}

func TestExploreQuotedRecallCompactTermsRecognizesCompactValuesOnly(t *testing.T) {
	require.True(t, exploreQuotedRecallCompactTerms([]string{"kx"}))
	require.False(t, exploreQuotedRecallCompactTerms(nil))
	require.False(t, exploreQuotedRecallCompactTerms([]string{"DiskStorage"}))
	require.False(t, exploreQuotedRecallCompactTerms([]string{"kx", "DiskStorage"}),
		"a mixed term set is not a compact-value probe")
}

func TestExploreQuotedRecallCompactTermIgnoresUnrelatedRequestAnchor(t *testing.T) {
	node := &graph.Node{
		ID: "repo/storage/disk.go::DiskStorage.Load", Name: "DiskStorage.Load",
		Kind: graph.KindMethod, FilePath: "storage/disk.go",
	}
	task := `DiskStorage.Load throws while resolving region "kx"`

	// The request names this candidate, so ordinary terms are covered by it…
	require.True(t, exploreQuotedRecallHasExactSourceNode(task, []string{"DiskStorage"}, node, query.QueryOptions{}))
	// …but a compact value is settled by declaration text alone, so the bounded
	// literal lane still runs and can reach the registration site.
	require.False(t, exploreQuotedRecallHasExactSourceNode(task, []string{"kx"}, node, query.QueryOptions{}))
}

func TestTerminalExploreResultKeepsEvidenceBesideTheAnswer(t *testing.T) {
	node := &graph.Node{
		ID: "repo/storage/disk.go::DiskStorage.Load", Name: "DiskStorage.Load",
		Kind: graph.KindMethod, FilePath: "storage/disk.go", StartLine: 12,
		QualName: "storage.DiskStorage.Load",
		Meta: map[string]any{
			"signature":        "func (s *DiskStorage) Load(path string) ([]byte, error)",
			"search_qual_name": "storage.DiskStorage.Load",
		},
	}
	result, _, _, completion := buildLocalizationExploreResultForTaskFinalized(
		newLocalizationCompletion(true, ""),
		"DiskStorage.Load truncates large payloads",
		[]exploreTarget{{node: node, source: "func (s *DiskStorage) Load(path string) ([]byte, error) { return nil, nil }"}},
		exploreDefaultBudgetTokens,
	)
	require.Equal(t, localizationStateAnswerReady, completion.State)
	require.NotNil(t, result)

	// The terminal projection must not be the only thing a structuredContent
	// host can see: an answer without its evidence sends the caller looking for
	// the source it was just handed.
	structured, ok := result.StructuredContent.(map[string]any)
	require.True(t, ok, "terminal result must carry structured content")
	require.Contains(t, structured, "final_response")
	require.Contains(t, structured, "files")
	require.Contains(t, structured, "symbols")
	require.Contains(t, structured, "evidence")
	rows, ok := structured["evidence"].([]any)
	require.True(t, ok)
	require.NotEmpty(t, rows, "terminal page keeps its ranked rows")
	first, ok := rows[0].(map[string]any)
	require.True(t, ok)
	require.Equal(t, node.ID, first["id"])
	require.NotEmpty(t, first["source"], "the ranked head keeps the body the caller would otherwise read")
}
