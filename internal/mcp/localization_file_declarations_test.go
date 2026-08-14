package mcp

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

type localizationDeclarationCall struct {
	file  string
	scope graph.LocalizationNodeScope
	limit int
}

type localizationDeclarationSpyReader struct {
	graph.Reader
	files     map[string][]*graph.Node
	totals    map[string]int
	fileCalls map[string]int
	calls     []localizationDeclarationCall
	err       error
}

func (reader *localizationDeclarationSpyReader) GetFileNodes(string) []*graph.Node {
	panic("localization declarations must never use the legacy full-row reader")
}

func (reader *localizationDeclarationSpyReader) FindFileNodesBounded(
	_ context.Context,
	file string,
	scope graph.LocalizationNodeScope,
	limit int,
) (graph.BoundedNodeProjection, error) {
	if reader.fileCalls == nil {
		reader.fileCalls = make(map[string]int)
	}
	reader.fileCalls[file]++
	reader.calls = append(reader.calls, localizationDeclarationCall{file: file, scope: scope, limit: limit})
	if reader.err != nil {
		return graph.BoundedNodeProjection{}, reader.err
	}

	admitted := make([]*graph.Node, 0, min(len(reader.files[file]), limit))
	for _, node := range reader.files[file] {
		if scope.Allows(node) {
			admitted = append(admitted, node)
		}
	}
	total := len(admitted)
	if configured, ok := reader.totals[file]; ok {
		total = configured
	}
	truncated := total > limit
	observed := total
	if observed > limit+1 {
		observed = limit + 1
	}
	if len(admitted) > limit {
		admitted = admitted[:limit]
	}
	return graph.BoundedNodeProjection{Nodes: admitted, Total: observed, Truncated: truncated}, nil
}

func newLocalizationDeclarationTestCache(reader graph.Reader) *localizationFileDeclarationCache {
	return newLocalizationFileDeclarationCache(
		context.Background(), reader, graph.LocalizationNodeScope{},
	)
}

func TestLocalizationFileDeclarationCacheReadsEachFileOnce(t *testing.T) {
	function := &graph.Node{ID: "src/a.go::run", Name: "run", Kind: graph.KindFunction, FilePath: "src/a.go"}
	reader := &localizationDeclarationSpyReader{files: map[string][]*graph.Node{"src/a.go": {function}}}
	cache := newLocalizationDeclarationTestCache(reader)

	require.Equal(t, []*graph.Node{function}, cache.definitions("src/a.go").Nodes)
	require.Equal(t, []*graph.Node{function}, cache.definitions("src/a.go").Nodes)
	require.Equal(t, 1, reader.fileCalls["src/a.go"])
	require.Equal(t, localizationFileNodeLimit, reader.calls[0].limit)
}

func TestLocalizationFileDeclarationCacheCachesMissesAndDistinctFiles(t *testing.T) {
	reader := &localizationDeclarationSpyReader{files: map[string][]*graph.Node{}}
	cache := newLocalizationDeclarationTestCache(reader)

	require.Empty(t, cache.definitions("src/missing.go").Nodes)
	require.Empty(t, cache.definitions("src/missing.go").Nodes)
	require.Empty(t, cache.definitions("src/other.go").Nodes)
	require.Equal(t, map[string]int{"src/missing.go": 1, "src/other.go": 1}, reader.fileCalls)
}

func TestLocalizationFileDeclarationCachePushesScopeAndDefinitionExclusions(t *testing.T) {
	first := &graph.Node{ID: "src/a.go::first", Name: "first", Kind: graph.KindFunction, FilePath: "src/a.go", RepoPrefix: "repo"}
	second := &graph.Node{ID: "src/a.go::Thing", Name: "Thing", Kind: graph.KindType, FilePath: "src/a.go", RepoPrefix: "repo"}
	reader := &localizationDeclarationSpyReader{files: map[string][]*graph.Node{"src/a.go": {
		{ID: "src/a.go", Name: "a.go", Kind: graph.KindFile, FilePath: "src/a.go", RepoPrefix: "repo"},
		first,
		{ID: "src/a.go::arg", Name: "arg", Kind: graph.KindParam, FilePath: "src/a.go", RepoPrefix: "repo"},
		second,
		{ID: "foreign/a.go::hidden", Name: "hidden", Kind: graph.KindFunction, FilePath: "src/a.go", RepoPrefix: "foreign"},
	}}}
	cache := newLocalizationFileDeclarationCache(
		context.Background(), reader,
		graph.LocalizationNodeScope{RepoAllow: map[string]bool{"repo": true}},
	)

	require.Equal(t, []*graph.Node{first, second}, cache.definitions("src/a.go").Nodes)
	require.Len(t, reader.calls, 1)
	for _, kind := range localizationDeclarationExcludedKinds {
		require.Truef(t, reader.calls[0].scope.ExcludeKinds[kind], "kind %q was not pushed down", kind)
	}
	require.True(t, reader.calls[0].scope.RepoAllow["repo"])
}

func TestLocalizationFileDeclarationCacheBoundsRetainedDefinitions(t *testing.T) {
	first := &graph.Node{ID: "src/a.go::first", Name: "first", Kind: graph.KindFunction, FilePath: "src/a.go"}
	second := &graph.Node{ID: "src/a.go::second", Name: "second", Kind: graph.KindFunction, FilePath: "src/a.go"}
	third := &graph.Node{ID: "src/a.go::third", Name: "third", Kind: graph.KindFunction, FilePath: "src/a.go"}
	reader := &localizationDeclarationSpyReader{files: map[string][]*graph.Node{"src/a.go": {first, second, third}}}
	cache := newBoundedLocalizationFileDeclarationCache(
		context.Background(), reader, graph.LocalizationNodeScope{}, 2,
	)

	firstRead := cache.definitions("src/a.go")
	require.Equal(t, []*graph.Node{first, second}, firstRead.Nodes)
	require.Equal(t, 3, firstRead.Declared)
	require.False(t, firstRead.DeclaredKnown)
	require.True(t, firstRead.Truncated)
	require.Equal(t, []*graph.Node{first, second}, cache.definitions("src/a.go").Nodes)
	require.Equal(t, 1, reader.fileCalls["src/a.go"])
	require.Equal(t, 2, reader.calls[0].limit)
}

func TestBoundedLocalizationDeclarationsRenderHonestLowerBounds(t *testing.T) {
	const (
		file           = "src/generated.go"
		declared       = 140
		retentionLimit = 128
	)
	nodes := make([]*graph.Node, 0, declared)
	for index := 0; index < declared; index++ {
		nodes = append(nodes, &graph.Node{
			ID: file + fmt.Sprintf("::declaration%03d", index), Name: fmt.Sprintf("declaration%03d", index),
			Kind: graph.KindFunction, FilePath: file, StartLine: index + 1,
		})
	}
	reader := &localizationDeclarationSpyReader{files: map[string][]*graph.Node{file: nodes}}
	cache := newBoundedLocalizationFileDeclarationCache(
		context.Background(), reader, graph.LocalizationNodeScope{}, retentionLimit,
	)

	page := localizationPageOutlineProvider(
		nil, []exploreTarget{{node: nodes[0]}}, nil, cache.definitions,
	)()

	require.NotNil(t, page)
	require.NotNil(t, page.Leading)
	require.Equal(t, retentionLimit+1, page.Leading.Declared)
	require.Equal(t, 1, page.Leading.Elided)
	require.True(t, page.Leading.Truncated)
	require.Len(t, page.Leading.Rows, retentionLimit)
	retained := cache.byFile[file]
	require.False(t, retained.DeclaredKnown)
	require.True(t, retained.Truncated)
	require.Len(t, retained.Nodes, retentionLimit)
	require.Equal(t, 1, reader.fileCalls[file])
}

func TestLocalizationFileDeclarationCacheFailsClosed(t *testing.T) {
	legacy := &struct{ graph.Reader }{Reader: graph.New()}
	unsupported := newLocalizationDeclarationTestCache(legacy).definitions("src/a.go")
	require.Empty(t, unsupported.Nodes)
	require.True(t, unsupported.Truncated)
	require.False(t, unsupported.DeclaredKnown)

	failedReader := &localizationDeclarationSpyReader{err: errors.New("read failed")}
	failed := newLocalizationDeclarationTestCache(failedReader).definitions("src/a.go")
	require.Empty(t, failed.Nodes)
	require.True(t, failed.Truncated)

	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	cancelled := newLocalizationFileDeclarationCache(
		cancelledCtx, &localizationDeclarationSpyReader{}, graph.LocalizationNodeScope{},
	).definitions("src/a.go")
	require.Empty(t, cancelled.Nodes)
	require.True(t, cancelled.Truncated)
}

func TestLocalizationFileDeclarationCacheSharesRequestBudgetInPriorityOrder(t *testing.T) {
	reader := &localizationDeclarationSpyReader{files: make(map[string][]*graph.Node)}
	ctx := withLocalizationFileRequestBudget(context.Background())
	cache := newLocalizationFileDeclarationCache(ctx, reader, graph.LocalizationNodeScope{})
	for index := 0; index < 5; index++ {
		file := fmt.Sprintf("src/file-%d.go", index)
		for declaration := 0; declaration < localizationFileNodeLimit; declaration++ {
			reader.files[file] = append(reader.files[file], &graph.Node{
				ID: fmt.Sprintf("%s::owner-%04d", file, declaration), Name: "owner",
				Kind: graph.KindFunction, FilePath: file,
			})
		}
		page := cache.definitions(file)
		if index < localizationFileRequestLimit/localizationFileNodeLimit {
			require.False(t, page.Truncated)
			require.Len(t, page.Nodes, localizationFileNodeLimit)
		} else {
			require.True(t, page.Truncated)
			require.Empty(t, page.Nodes)
		}
	}
	require.Len(t, reader.calls, localizationFileRequestLimit/localizationFileNodeLimit)
	for index, call := range reader.calls {
		require.Equal(t, fmt.Sprintf("src/file-%d.go", index), call.file)
		require.Equal(t, localizationFileNodeLimit, call.limit)
	}
}
