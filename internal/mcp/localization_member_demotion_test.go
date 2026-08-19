package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search/rerank"
)

func memberDemotionCandidate(file, name string, kind graph.NodeKind) *rerank.Candidate {
	return &rerank.Candidate{
		Node: &graph.Node{
			ID:         "fixture/" + file + "::" + name,
			Name:       name,
			Kind:       kind,
			FilePath:   "fixture/" + file,
			RepoPrefix: "fixture",
		},
		TextRank:   0,
		VectorRank: -1,
	}
}

func memberDemotionNames(cands []*rerank.Candidate) []string {
	names := make([]string, 0, len(cands))
	for _, cand := range cands {
		if cand == nil || cand.Node == nil {
			names = append(names, "")
			continue
		}
		names = append(names, cand.Node.Name)
	}
	return names
}

func memberDemotionFiles(cands []*rerank.Candidate) []string {
	files := make([]string, 0, len(cands))
	for _, cand := range cands {
		if cand == nil || cand.Node == nil {
			files = append(files, "")
			continue
		}
		files = append(files, cand.Node.FilePath)
	}
	return files
}

func TestLocalizationDemotionYieldsFrontSlotToSiblingCallable(t *testing.T) {
	tests := []struct {
		name    string
		member  string
		ordered []string
	}{
		{name: "constructor", member: "DirectoryConfiguration.<init>", ordered: []string{"detect", "DirectoryConfiguration.<init>"}},
		{name: "destructor", member: "deinit", ordered: []string{"detect", "deinit"}},
		{name: "subscript", member: "subscript", ordered: []string{"detect", "subscript"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			in := []*rerank.Candidate{
				memberDemotionCandidate("config.swift", test.member, graph.KindMethod),
				memberDemotionCandidate("config.swift", "detect", graph.KindMethod),
			}
			require.Equal(t, test.ordered, memberDemotionNames(demoteLocalizationFileMembers(in)))
		})
	}
}

func TestLocalizationDemotionYieldsFrontSlotToSiblingMethodForDataMembers(t *testing.T) {
	tests := []struct {
		name   string
		member string
		kind   graph.NodeKind
	}{
		{name: "field", member: "shaderSlots", kind: graph.KindField},
		{name: "enum member", member: "ShaderStage.vertex", kind: graph.KindEnumMember},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			in := []*rerank.Candidate{
				memberDemotionCandidate("shaders.dart", test.member, test.kind),
				memberDemotionCandidate("shaders.dart", "bindShaders", graph.KindMethod),
			}
			require.Equal(t, []string{"bindShaders", test.member}, memberDemotionNames(demoteLocalizationFileMembers(in)))
		})
	}
}

func TestLocalizationDemotionKeepsTypesAndOrdinaryCallablesInOneLeadingTier(t *testing.T) {
	in := []*rerank.Candidate{
		memberDemotionCandidate("config.swift", "DirectoryConfiguration.<init>", graph.KindMethod),
		memberDemotionCandidate("config.swift", "shaderSlots", graph.KindField),
		memberDemotionCandidate("config.swift", "DirectoryConfiguration", graph.KindType),
		memberDemotionCandidate("config.swift", "deinit", graph.KindMethod),
		memberDemotionCandidate("config.swift", "detect", graph.KindMethod),
		memberDemotionCandidate("config.swift", "reload", graph.KindMethod),
	}
	require.Equal(t, []string{
		"DirectoryConfiguration", "detect", "reload",
		"DirectoryConfiguration.<init>", "deinit",
		"shaderSlots",
	}, memberDemotionNames(demoteLocalizationFileMembers(in)))
}

func TestLocalizationDemotionPermutesOnlyInsideEachFilesOwnSlots(t *testing.T) {
	in := []*rerank.Candidate{
		memberDemotionCandidate("config.swift", "shaderSlots", graph.KindField),
		memberDemotionCandidate("cache.dart", "ShaderCache.<init>", graph.KindMethod),
		memberDemotionCandidate("config.swift", "detect", graph.KindMethod),
		nil,
		memberDemotionCandidate("cache.dart", "warm", graph.KindMethod),
		{Node: &graph.Node{ID: "fixture::floating", Name: "floating", Kind: graph.KindField}},
		memberDemotionCandidate("config.swift", "reload", graph.KindMethod),
	}
	files := memberDemotionFiles(in)
	ids := make(map[string]int, len(in))
	for _, cand := range in {
		if cand != nil && cand.Node != nil {
			ids[cand.Node.ID]++
		}
	}

	out := demoteLocalizationFileMembers(in)

	require.Equal(t, len(in), len(out), "the window keeps the same number of slots")
	require.Equal(t, files, memberDemotionFiles(out), "no candidate moves into another file's slot")
	outIDs := make(map[string]int, len(out))
	for _, cand := range out {
		if cand != nil && cand.Node != nil {
			outIDs[cand.Node.ID]++
		}
	}
	require.Equal(t, ids, outIDs, "the window keeps the same candidate set")
	require.Equal(t, []string{
		"detect", "warm", "reload", "", "ShaderCache.<init>", "floating", "shaderSlots",
	}, memberDemotionNames(out))
	require.Equal(t, memberDemotionNames(out), memberDemotionNames(demoteLocalizationFileMembers(out)), "the reorder is idempotent")
}

func newIndexedSwiftMemberDemotionServer(t *testing.T) *Server {
	t.Helper()

	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "Sources"), 0o755))
	files := map[string]string{
		"DirectoryConfiguration.swift": `
import Foundation

/// Directory configuration for the shader cache.
public struct DirectoryConfiguration {
    /// Root directory scanned when directory configuration detection runs.
    let root: String
    /// Shader slots reserved by the directory configuration.
    let shaderSlots: Int

    /// Creates a directory configuration.
    public init(root: String, shaderSlots: Int) {
        self.root = root
        self.shaderSlots = shaderSlots
    }

    deinit {
    }

    subscript(index: Int) -> String {
        return root
    }

    /// Detects the directory configuration for a nested path.
    public func detect(path: String) -> Bool {
        return !path.isEmpty
    }

    public func reload(path: String) -> Bool {
        return detect(path: path)
    }
}
`,
		"ShaderCache.swift": `
import Foundation

public struct ShaderCache {
    let slots: Int

    public init(slots: Int) {
        self.slots = slots
    }

    public func warm(configuration: DirectoryConfiguration) -> Bool {
        return configuration.detect(path: "/tmp")
    }
}
`,
	}
	for name, content := range files {
		require.NoError(t, os.WriteFile(filepath.Join(root, "Sources", name), []byte(content), 0o644))
	}

	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })

	registry := parser.NewRegistry()
	registry.Register(languages.NewSwiftExtractor())
	idx := indexer.New(store, registry, config.IndexConfig{Workers: 1}, zap.NewNop())
	idx.SetRepoPrefix("swift-fixture")
	idx.SetWorkspaceID("swift-fixture")
	idx.SetProjectID("swift-fixture")
	_, err = idx.IndexCtx(context.Background(), root)
	require.NoError(t, err)

	engine := query.NewEngine(store)
	engine.SetSearchProvider(idx.Search)
	return NewServer(engine, store, idx, nil, zap.NewNop(), nil)
}

func TestIndexedSwiftMemberDemotionAppliesOnlyToTheLocalizePage(t *testing.T) {
	const (
		task     = "directory configuration detection is broken"
		fieldID  = "swift-fixture/Sources/DirectoryConfiguration.swift::DirectoryConfiguration.root"
		methodID = "swift-fixture/Sources/DirectoryConfiguration.swift::DirectoryConfiguration.detect"
	)
	server := newIndexedSwiftMemberDemotionServer(t)
	call := func(localize bool) *mcpgo.CallToolResult {
		request := mcpgo.CallToolRequest{}
		request.Params.Arguments = map[string]any{
			"task":          task,
			"localize":      localize,
			"max_symbols":   8,
			"token_budget":  2400,
			"repository_id": "swift-fixture",
		}
		result, err := server.handleExplore(context.Background(), request)
		require.NoError(t, err)
		require.False(t, result.IsError)
		require.NotEmpty(t, result.Content)
		return result
	}

	localized, ok := call(true).Content[0].(mcpgo.TextContent)
	require.True(t, ok)
	var envelope localizationExploreEnvelope
	require.NoError(t, json.Unmarshal([]byte(localized.Text), &envelope))
	fieldRank, methodRank := -1, -1
	for index, evidence := range envelope.Evidence {
		switch evidence.ID {
		case fieldID:
			fieldRank = index
		case methodID:
			methodRank = index
		}
	}
	require.GreaterOrEqual(t, methodRank, 0, "the sibling method stays in the window: %#v", envelope.Evidence)
	require.GreaterOrEqual(t, fieldRank, 0, "the demoted field stays in the window: %#v", envelope.Evidence)
	require.Less(t, methodRank, fieldRank, "localization seats the sibling method ahead of the field: %#v", envelope.Evidence)

	// The non-localize lane keeps whatever order ranking produced, so this
	// fixture's field — whose doc comment carries every task term — still leads
	// its sibling method there.
	plain, plainOK := call(false).Content[0].(mcpgo.TextContent)
	require.True(t, plainOK)
	plainField := strings.Index(plain.Text, "id: "+fieldID)
	plainMethod := strings.Index(plain.Text, "id: "+methodID)
	require.GreaterOrEqual(t, plainField, 0, "explore lists the ranked field: %s", plain.Text)
	require.GreaterOrEqual(t, plainMethod, 0, "explore lists the ranked method: %s", plain.Text)
	require.Less(t, plainField, plainMethod, "the non-localize page is untouched by localization demotion: %s", plain.Text)
}
