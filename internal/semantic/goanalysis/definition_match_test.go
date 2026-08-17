package goanalysis

import (
	"go/ast"
	"go/importer"
	goparser "go/parser"
	"go/token"
	"go/types"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

type checkedGoDefinition struct {
	ident    *ast.Ident
	object   types.Object
	position token.Position
	context  goDefinitionContext
}

func checkedGoDefinitions(t *testing.T, source string) []checkedGoDefinition {
	t.Helper()
	fset := token.NewFileSet()
	file, err := goparser.ParseFile(fset, "sample.go", source, 0)
	require.NoError(t, err)
	info := &types.Info{Defs: make(map[*ast.Ident]types.Object)}
	_, err = (&types.Config{Importer: importer.Default()}).Check("example.com/sample", fset, []*ast.File{file}, info)
	require.NoError(t, err)
	contexts := buildGoDefinitionContexts([]*ast.File{file})
	definitions := make([]checkedGoDefinition, 0, len(info.Defs))
	for ident, object := range info.Defs {
		if object == nil {
			continue
		}
		definitions = append(definitions, checkedGoDefinition{
			ident:    ident,
			object:   object,
			position: fset.Position(ident.Pos()),
			context:  contexts[ident],
		})
	}
	return definitions
}

func requireCheckedGoDefinition(t *testing.T, definitions []checkedGoDefinition, name string, line int) checkedGoDefinition {
	t.Helper()
	for _, definition := range definitions {
		if definition.ident.Name == name && definition.position.Line == line {
			return definition
		}
	}
	t.Fatalf("definition %s at line %d not found", name, line)
	return checkedGoDefinition{}
}

func TestMatchRepoDefinitionNodeRejectsSameLineParameterForFunction(t *testing.T) {
	definitions := checkedGoDefinitions(t, "package sample\nfunc Hello(name string) {}\n")
	hello := requireCheckedGoDefinition(t, definitions, "Hello", 2)
	functionID := "sample.go::Hello"
	nodes := []*graph.Node{
		nil,
		{ID: "sample.go", Kind: graph.KindFile, Name: "Hello", StartLine: 2},
		{ID: "sample.go::import", Kind: graph.KindImport, Name: "Hello", StartLine: 2},
		{ID: functionID + "#param:name", Kind: graph.KindParam, Name: "name", StartLine: 2},
		{ID: functionID, Kind: graph.KindFunction, Name: "Hello", StartLine: 2, EndLine: 2},
	}

	matched := matchRepoDefinitionNode(nodes, hello.position, hello.ident.Name, hello.object, hello.context)
	require.NotNil(t, matched)
	assert.Equal(t, functionID, matched.ID)
}

func TestMatchRepoDefinitionNodeObjectRolesAndOwners(t *testing.T) {
	const source = `package sample

type Widget struct {
	Field int
}

type Contract interface {
	Call()
}

const Answer = 42
var Global int

func (w Widget) Method(param int) (result int) {
	local := param
	_ = local
	var declared int
	_ = declared
	return
}
`
	definitions := checkedGoDefinitions(t, source)
	nodes := []*graph.Node{
		{ID: "sample.go::Widget", Kind: graph.KindType, Name: "Widget", StartLine: 3, EndLine: 5},
		{ID: "sample.go::Widget.Field", Kind: graph.KindField, Name: "Field", StartLine: 4, EndLine: 4},
		{ID: "sample.go::Contract", Kind: graph.KindInterface, Name: "Contract", StartLine: 7, EndLine: 9},
		{ID: "sample.go::Contract.Call", Kind: graph.KindMethod, Name: "Call", StartLine: 8, EndLine: 8},
		{ID: "sample.go::Answer", Kind: graph.KindConstant, Name: "Answer", StartLine: 11, EndLine: 11},
		{ID: "sample.go::Global", Kind: graph.KindVariable, Name: "Global", StartLine: 12, EndLine: 12},
		{ID: "sample.go::Other.Method", Kind: graph.KindMethod, Name: "Method", StartLine: 14, EndLine: 20},
		{ID: "sample.go::Widget.Method", Kind: graph.KindMethod, Name: "Method", StartLine: 14, EndLine: 20},
		{ID: "sample.go::Widget.Method#param:w", Kind: graph.KindParam, Name: "w", StartLine: 14},
		{ID: "sample.go::Widget.Method#param:param", Kind: graph.KindParam, Name: "param", StartLine: 14},
		{ID: "sample.go::Widget.Method#param:result", Kind: graph.KindParam, Name: "result", StartLine: 14},
		{ID: "sample.go::Widget.Method#local:local@+1", Kind: graph.KindLocal, Name: "local", StartLine: 15},
		{ID: "sample.go::Widget.Method#local:declared@+3", Kind: graph.KindLocal, Name: "declared", StartLine: 17},
	}

	for _, test := range []struct {
		name   string
		line   int
		wantID string
	}{
		{name: "Widget", line: 3, wantID: "sample.go::Widget"},
		{name: "Field", line: 4, wantID: "sample.go::Widget.Field"},
		{name: "Contract", line: 7, wantID: "sample.go::Contract"},
		{name: "Call", line: 8, wantID: "sample.go::Contract.Call"},
		{name: "Answer", line: 11, wantID: "sample.go::Answer"},
		{name: "Global", line: 12, wantID: "sample.go::Global"},
		{name: "Method", line: 14, wantID: "sample.go::Widget.Method"},
		{name: "param", line: 14, wantID: "sample.go::Widget.Method#param:param"},
		{name: "local", line: 15, wantID: "sample.go::Widget.Method#local:local@+1"},
		{name: "declared", line: 17, wantID: "sample.go::Widget.Method#local:declared@+3"},
		{name: "w", line: 14, wantID: ""},
		{name: "result", line: 14, wantID: ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			definition := requireCheckedGoDefinition(t, definitions, test.name, test.line)
			matched := matchRepoDefinitionNode(nodes, definition.position, definition.ident.Name, definition.object, definition.context)
			if test.wantID == "" {
				assert.Nil(t, matched)
				return
			}
			require.NotNil(t, matched)
			assert.Equal(t, test.wantID, matched.ID)
		})
	}
}

func TestMatchRepoDefinitionNodeRangeAndDeterminism(t *testing.T) {
	pkg := types.NewPackage("example.com/sample", "sample")
	object := types.NewFunc(token.NoPos, pkg, "Target", types.NewSignatureType(nil, nil, nil, nil, nil, false))
	position := token.Position{Filename: "sample.go", Line: 10}
	node := func(id string, kind graph.NodeKind, name string, start, end int) *graph.Node {
		return &graph.Node{ID: id, Kind: kind, Name: name, StartLine: start, EndLine: end}
	}

	for _, test := range []struct {
		name  string
		nodes []*graph.Node
		want  string
	}{
		{name: "exact", nodes: []*graph.Node{node("exact", graph.KindFunction, "Target", 10, 10)}, want: "exact"},
		{name: "innermost containing", nodes: []*graph.Node{
			node("outer", graph.KindFunction, "Target", 8, 14),
			node("inner", graph.KindFunction, "Target", 9, 11),
		}, want: "inner"},
		{name: "nearest one", nodes: []*graph.Node{node("one", graph.KindFunction, "Target", 11, 11)}, want: "one"},
		{name: "nearest two", nodes: []*graph.Node{node("two", graph.KindFunction, "Target", 12, 12)}, want: "two"},
		{name: "greater than two", nodes: []*graph.Node{node("three", graph.KindFunction, "Target", 13, 13)}},
		{name: "wrong name", nodes: []*graph.Node{node("wrong-name", graph.KindFunction, "Other", 10, 10)}},
		{name: "wrong kind", nodes: []*graph.Node{node("wrong-kind", graph.KindParam, "Target", 10, 10)}},
		{name: "stable node identity", nodes: []*graph.Node{
			node("z-target", graph.KindFunction, "Target", 10, 10),
			node("a-target", graph.KindFunction, "Target", 10, 10),
		}, want: "a-target"},
	} {
		t.Run(test.name, func(t *testing.T) {
			matched := matchRepoDefinitionNode(test.nodes, position, "Target", object, goDefinitionContext{})
			if test.want == "" {
				assert.Nil(t, matched)
				return
			}
			require.NotNil(t, matched)
			assert.Equal(t, test.want, matched.ID)
		})
	}

	variable := types.NewVar(token.NoPos, pkg, "value", types.Typ[types.Int])
	ambiguous := matchRepoDefinitionNode([]*graph.Node{
		node("sample.go::F#param:value", graph.KindParam, "value", 10, 10),
		node("sample.go::F#local:value@+0", graph.KindLocal, "value", 10, 10),
	}, position, "value", variable, goDefinitionContext{})
	assert.Nil(t, ambiguous, "an unknown compiler variable role must fail closed across graph kinds")
}

func TestGoAnalysisDefinitionTargetsMatchAcrossMemoryAndSQLite(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			var store graph.Store
			switch backend {
			case "memory":
				store = graph.New()
			case "sqlite":
				sqliteStore, err := store_sqlite.Open(filepath.Join(t.TempDir(), "semantic.sqlite"))
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, sqliteStore.Close()) })
				store = sqliteStore
			}
			assertGoAnalysisDefinitionTargets(t, store)
		})
	}
}

func assertGoAnalysisDefinitionTargets(t *testing.T, store graph.Store) {
	t.Helper()
	root := resolvedTempDir(t)
	writeGoMod(t, root, "example.com/definitiontarget")
	writeFile(t, root, "main.go", `package sample

func Hello(name string) {}

func UseConfirmed() { Hello("confirmed") }

func UseAdded() { Hello("added") }
`)

	const (
		helloID     = "main.go::Hello"
		parameterID = helloID + "#param:name"
		confirmedID = "main.go::UseConfirmed"
		addedID     = "main.go::UseAdded"
	)
	store.AddBatch([]*graph.Node{
		{ID: parameterID, Kind: graph.KindParam, Name: "name", FilePath: "main.go", StartLine: 3, Language: "go"},
		{ID: helloID, Kind: graph.KindFunction, Name: "Hello", FilePath: "main.go", StartLine: 3, EndLine: 3, Language: "go"},
		{ID: confirmedID, Kind: graph.KindFunction, Name: "UseConfirmed", FilePath: "main.go", StartLine: 5, EndLine: 5, Language: "go"},
		{ID: addedID, Kind: graph.KindFunction, Name: "UseAdded", FilePath: "main.go", StartLine: 7, EndLine: 7, Language: "go"},
	}, []*graph.Edge{{
		From:            confirmedID,
		To:              helloID,
		Kind:            graph.EdgeCalls,
		FilePath:        "main.go",
		Line:            5,
		Confidence:      0.4,
		ConfidenceLabel: "INFERRED",
		Origin:          graph.OriginASTInferred,
	}})

	provider := newTestProvider(t)
	t.Cleanup(func() { require.NoError(t, provider.Close()) })
	result, err := provider.Enrich(store, root)
	require.NoError(t, err)
	require.GreaterOrEqual(t, result.EdgesConfirmed, 1)
	require.GreaterOrEqual(t, result.EdgesAdded, 1)

	for _, callerID := range []string{confirmedID, addedID} {
		var correct *graph.Edge
		for _, edge := range store.GetOutEdges(callerID) {
			if edge.Kind != graph.EdgeCalls {
				continue
			}
			assert.NotEqual(t, parameterID, edge.To, "compiler call target must never be the same-line parameter")
			if edge.To == helloID {
				correct = edge
			}
		}
		require.NotNil(t, correct, "semantic call from %s must target Hello", callerID)
		assert.Equal(t, 1.0, correct.Confidence)
		assert.Equal(t, graph.OriginLSPResolved, correct.Origin)
	}
}
