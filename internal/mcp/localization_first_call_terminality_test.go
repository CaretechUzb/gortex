package mcp

import (
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
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

func TestLocalizationEvidencePolicyHoldsRankedReadinessWithoutProof(t *testing.T) {
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
	// Without one of the hard provenance shapes the ranked verdict does not end
	// navigation: the page spends its one bounded call before answering.
	require.Equal(t, localizationStateNeedsRecovery, finalized.State)
	require.Equal(t, "recover_once", finalized.RequiredAction)
	require.Equal(t, 1, finalized.AllowedToolCalls)
	require.False(t, finalized.Enforceable)
	require.False(t, localizationContractFor(finalized).Terminal)
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
	// A source-literal callee is one of the hard provenance shapes, so this page
	// is terminal on any branch — the assertion below is about what a terminal
	// page CARRIES, not about when a page becomes terminal.
	target := exploreTarget{
		node: node, source: "func (s *DiskStorage) Load(path string) ([]byte, error) { return nil, nil }",
		sourceLiteral: true, sourceLiteralCallee: true, exactContent: true,
	}
	result, _, _, completion := buildLocalizationExploreResultForTaskFinalized(
		newLocalizationCompletion(true, ""),
		"DiskStorage.Load truncates large payloads",
		[]exploreTarget{target},
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

func TestTerminalProjectionKeepsTheToolsOwnPayload(t *testing.T) {
	// The authorized read answers with source in its text payload and no
	// structured content of its own. The terminal projection must land on top
	// of it, not in place of it — a caller that renders structured content
	// would otherwise be told to answer from evidence it can no longer see.
	payload := `{"symbol":"repo/storage/disk.go::DiskStorage.Load",` +
		`"source":"func (s *DiskStorage) Load(path string) ([]byte, error) { return nil, nil }"}`
	completion := newLocalizationCompletion(true, "")
	completion.FinalResponse = "LOCALIZATION:\n- PRIMARY — storage/disk.go:12 — repo/storage/disk.go::DiskStorage.Load"

	result := attachLocalizationHostEnvelope(mcpgo.NewToolResultText(payload), completion, nil)
	structured, ok := result.StructuredContent.(map[string]any)
	require.True(t, ok)
	require.Equal(t, "repo/storage/disk.go::DiskStorage.Load", structured["symbol"])
	require.Contains(t, structured["source"], "func (s *DiskStorage) Load")
	require.Contains(t, structured, "final_response")
}

func TestExploreDelegatedImplementationCalleeRecognisesWrapperHelpers(t *testing.T) {
	wrapper := &graph.Node{ID: "repo/storage/disk.go::DiskStorage.Load", Name: "Load", Kind: graph.KindMethod, FilePath: "storage/disk.go"}
	helper := &graph.Node{ID: "repo/storage/disk.go::DiskStorage.LoadRec", Name: "LoadRec", Kind: graph.KindMethod, FilePath: "storage/disk.go"}
	require.False(t, exploreDelegatedImplementationCallee(wrapper, helper),
		"a short base admits the variant family (Load/LoadAll), so it cannot claim LoadRec")

	base := &graph.Node{ID: "repo/storage/disk.go::DiskStorage.resolvePath", Name: "resolvePath", Kind: graph.KindFunction, FilePath: "storage/disk.go"}
	for _, impl := range []struct {
		name string
		want bool
		why  string
	}{
		{"resolvePathRec", true, "recursive helper on the same base"},
		{"resolvePathImpl", true, "implementation helper"},
		{"resolvePath_inner", true, "separator-joined helper"},
		{"resolvePath", false, "identical identifier is the caller itself"},
		{"resolve", false, "shorter identifier is not an extension"},
		{"resolvePathWithAVeryLongTail", false, "an unbounded tail is a different function"},
		{"unrelatedHelper", false, "no shared base"},
	} {
		node := &graph.Node{ID: "repo/storage/disk.go::" + impl.name, Name: impl.name, Kind: graph.KindFunction, FilePath: "storage/disk.go"}
		require.Equal(t, impl.want, exploreDelegatedImplementationCallee(base, node), impl.why)
	}

	// Direction and kind still gate it: a test double or a type never counts.
	testNode := &graph.Node{ID: "repo/storage/disk_test.go::resolvePathRec", Name: "resolvePathRec", Kind: graph.KindFunction, FilePath: "storage/disk_test.go", Meta: map[string]any{"is_test": true}}
	require.False(t, exploreDelegatedImplementationCallee(base, testNode))
	typeNode := &graph.Node{ID: "repo/storage/disk.go::resolvePathRec", Name: "resolvePathRec", Kind: graph.KindType, FilePath: "storage/disk.go"}
	require.False(t, exploreDelegatedImplementationCallee(base, typeNode))
	require.False(t, exploreDelegatedImplementationCallee(nil, typeNode))
}
