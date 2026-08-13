package mcp

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

type localizationDeclarationSpyReader struct {
	graph.Reader
	files        map[string][]*graph.Node
	fileCalls    map[string]int
	inEdgeCalls  int
	outEdgeCalls int
}

func (reader *localizationDeclarationSpyReader) GetFileNodes(file string) []*graph.Node {
	reader.fileCalls[file]++
	return reader.files[file]
}

func (reader *localizationDeclarationSpyReader) GetInEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	reader.inEdgeCalls++
	return nil
}

func (reader *localizationDeclarationSpyReader) GetOutEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	reader.outEdgeCalls++
	return nil
}

func TestLocalizationFileDeclarationCacheReadsEachFileOnce(t *testing.T) {
	function := &graph.Node{ID: "src/a.go::run", Name: "run", Kind: graph.KindFunction, FilePath: "src/a.go"}
	reader := &localizationDeclarationSpyReader{
		files:     map[string][]*graph.Node{"src/a.go": {function}},
		fileCalls: make(map[string]int),
	}
	cache := newLocalizationFileDeclarationCache(reader)

	require.Equal(t, []*graph.Node{function}, cache.definitions("src/a.go").Nodes)
	require.Equal(t, []*graph.Node{function}, cache.definitions("src/a.go").Nodes)
	require.Equal(t, 1, reader.fileCalls["src/a.go"])
	require.Zero(t, reader.inEdgeCalls)
	require.Zero(t, reader.outEdgeCalls)
}

func TestLocalizationFileDeclarationCacheCachesMissesAndDistinctFiles(t *testing.T) {
	reader := &localizationDeclarationSpyReader{
		files:     map[string][]*graph.Node{},
		fileCalls: make(map[string]int),
	}
	cache := newLocalizationFileDeclarationCache(reader)

	require.Empty(t, cache.definitions("src/missing.go").Nodes)
	require.Empty(t, cache.definitions("src/missing.go").Nodes)
	require.Empty(t, cache.definitions("src/other.go").Nodes)
	require.Equal(t, map[string]int{"src/missing.go": 1, "src/other.go": 1}, reader.fileCalls)
}

func TestLocalizationFileDeclarationCacheKeepsOnlyDefinitionsInReaderOrder(t *testing.T) {
	first := &graph.Node{ID: "src/a.go::first", Name: "first", Kind: graph.KindFunction, FilePath: "src/a.go"}
	second := &graph.Node{ID: "src/a.go::Thing", Name: "Thing", Kind: graph.KindType, FilePath: "src/a.go"}
	reader := &localizationDeclarationSpyReader{
		files: map[string][]*graph.Node{"src/a.go": {
			nil,
			{ID: "src/a.go", Name: "a.go", Kind: graph.KindFile, FilePath: "src/a.go"},
			first,
			{ID: "src/a.go::arg", Name: "arg", Kind: graph.KindParam, FilePath: "src/a.go"},
			second,
		}},
		fileCalls: make(map[string]int),
	}

	require.Equal(t, []*graph.Node{first, second}, newLocalizationFileDeclarationCache(reader).definitions("src/a.go").Nodes)
}

func TestLocalizationFileDeclarationCacheBoundsRetainedDefinitions(t *testing.T) {
	first := &graph.Node{ID: "src/a.go::first", Name: "first", Kind: graph.KindFunction, FilePath: "src/a.go"}
	second := &graph.Node{ID: "src/a.go::second", Name: "second", Kind: graph.KindFunction, FilePath: "src/a.go"}
	third := &graph.Node{ID: "src/a.go::third", Name: "third", Kind: graph.KindFunction, FilePath: "src/a.go"}
	reader := &localizationDeclarationSpyReader{
		files:     map[string][]*graph.Node{"src/a.go": {first, second, third}},
		fileCalls: make(map[string]int),
	}
	cache := newBoundedLocalizationFileDeclarationCache(reader, 2)

	firstRead := cache.definitions("src/a.go")
	require.Equal(t, []*graph.Node{first, second}, firstRead.Nodes)
	require.Equal(t, 3, firstRead.Declared)
	require.True(t, firstRead.DeclaredKnown)
	require.True(t, firstRead.Truncated)
	require.Equal(t, []*graph.Node{first, second}, cache.definitions("src/a.go").Nodes)
	require.Equal(t, 1, reader.fileCalls["src/a.go"])
}

func TestBoundedLocalizationDeclarationsRenderHonestOutlineCounts(t *testing.T) {
	const (
		file           = "src/generated.go"
		declared       = 140
		retentionLimit = 128
	)
	nodes := make([]*graph.Node, 0, declared)
	for index := 0; index < declared; index++ {
		nodes = append(nodes, &graph.Node{
			ID:        file + "::declaration" + string(rune(index)),
			Name:      "declaration" + string(rune(index)),
			Kind:      graph.KindFunction,
			FilePath:  file,
			StartLine: index + 1,
		})
	}
	reader := &localizationDeclarationSpyReader{
		files:     map[string][]*graph.Node{file: nodes},
		fileCalls: make(map[string]int),
	}
	cache := newBoundedLocalizationFileDeclarationCache(reader, retentionLimit)

	page := localizationPageOutlineProvider(
		nil,
		[]exploreTarget{{node: nodes[0]}},
		nil,
		cache.definitions,
	)()

	require.NotNil(t, page)
	require.NotNil(t, page.Leading)
	require.Equal(t, declared, page.Leading.Declared)
	require.Equal(t, declared-retentionLimit, page.Leading.Elided)
	require.Len(t, page.Leading.Rows, retentionLimit)
	retained := cache.byFile[file]
	require.True(t, retained.DeclaredKnown)
	require.True(t, retained.Truncated)
	require.Len(t, retained.Nodes, retentionLimit)
	require.Equal(t, 1, reader.fileCalls[file])
}
