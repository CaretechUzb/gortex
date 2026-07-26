package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
)

// newTestTargetsServer builds a graph where one symbol is exercised by three
// test functions across two files, plus one uncovered symbol. EdgeTests runs
// test-function -> symbol-under-test, which is the fast path GetTesters walks.
func newTestTargetsServer(t *testing.T) *Server {
	t.Helper()
	g := graph.New()
	g.AddNode(&graph.Node{ID: "pkg/a.go::A", Kind: graph.KindFunction, Name: "A", FilePath: "pkg/a.go"})
	g.AddNode(&graph.Node{ID: "pkg/u.go::Uncovered", Kind: graph.KindFunction, Name: "Uncovered", FilePath: "pkg/u.go"})

	// Deliberately declared out of alphabetical order so a stable response
	// cannot come from insertion order alone.
	for _, tc := range []struct{ id, name, file string }{
		{"pkg/b_test.go::TestBeta", "TestBeta", "pkg/b_test.go"},
		{"pkg/a_test.go::TestZeta", "TestZeta", "pkg/a_test.go"},
		{"pkg/a_test.go::TestAlpha", "TestAlpha", "pkg/a_test.go"},
	} {
		g.AddNode(&graph.Node{ID: tc.id, Kind: graph.KindFunction, Name: tc.name, FilePath: tc.file})
		g.AddEdge(&graph.Edge{From: tc.id, To: "pkg/a.go::A", Kind: graph.EdgeTests})
	}

	return &Server{
		engine:     query.NewEngine(g),
		graph:      g,
		session:    newSessionState(),
		tokenStats: &tokenStats{},
		symHistory: &symbolHistory{entries: make(map[string][]SymbolModification)},
		sessions:   newSessionMap(),
		toolScopes: newScopeRegistry(),
	}
}

func testTargetsText(t *testing.T, s *Server, args map[string]any) string {
	t.Helper()
	req := mcp.CallToolRequest{}
	req.Params.Arguments = args
	res, err := s.handleGetTestTargets(context.Background(), req)
	require.NoError(t, err)
	require.NotNil(t, res)
	require.False(t, res.IsError)
	tc, ok := res.Content[0].(mcp.TextContent)
	require.True(t, ok)
	return tc.Text
}

// TestGetTestTargets_CompactIsText is the regression test for the Stop hook's
// raw-JSON leak: the tool has always been asked for compact:true and, before
// the compact branch existed, silently answered with a JSON payload.
func TestGetTestTargets_CompactIsText(t *testing.T) {
	s := newTestTargetsServer(t)
	out := testTargetsText(t, s, map[string]any{
		"ids":     "pkg/a.go::A,pkg/u.go::Uncovered",
		"compact": true,
	})

	if strings.HasPrefix(strings.TrimSpace(out), "{") {
		t.Fatalf("compact output is still JSON:\n%s", out)
	}
	var discard any
	if json.Unmarshal([]byte(out), &discard) == nil {
		t.Errorf("compact output should not parse as JSON:\n%s", out)
	}

	for _, want := range []string{
		"pkg/a_test.go TestAlpha TestZeta", // one line per file, functions sorted
		"pkg/b_test.go TestBeta",
		"run: go test -run TestAlpha|TestZeta ./pkg/",
		"uncovered: pkg/u.go::Uncovered",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("compact output missing %q\n---\n%s", want, out)
		}
	}
}

// TestGetTestTargets_CompactIsDeterministic guards the map-iteration bug: both
// the test-file set and each file's function set were ranged as maps, so the
// response — and the `-run A|B` join baked into run_commands — differed per
// call. That makes the payload unhashable and the CLI output unstable.
func TestGetTestTargets_CompactIsDeterministic(t *testing.T) {
	s := newTestTargetsServer(t)
	args := map[string]any{"ids": "pkg/a.go::A", "compact": true}

	first := testTargetsText(t, s, args)
	for i := 0; i < 20; i++ {
		if got := testTargetsText(t, s, args); got != first {
			t.Fatalf("output varied between calls\ncall 0:\n%s\ncall %d:\n%s", first, i+1, got)
		}
	}
}

// TestGetTestTargets_CompactNoTargets pins the exact sentinel the Stop hook's
// suppression guard tests for.
func TestGetTestTargets_CompactNoTargets(t *testing.T) {
	s := newTestTargetsServer(t)
	out := testTargetsText(t, s, map[string]any{
		"ids":     "pkg/u.go::Uncovered",
		"compact": true,
	})
	if !strings.HasPrefix(out, "no test targets found\n") {
		t.Errorf("expected the no-targets sentinel to lead the output, got:\n%s", out)
	}
	// The uncovered list still rides along — "nothing covers your change" is
	// the actionable signal, not a reason to say nothing.
	if !strings.Contains(out, "uncovered: pkg/u.go::Uncovered") {
		t.Errorf("expected the uncovered list alongside the sentinel:\n%s", out)
	}
}

// TestGetTestTargets_JSONShapeUnchanged locks the non-compact contract that
// `gortex affected` and `gortex edit tests` parse.
func TestGetTestTargets_JSONShapeUnchanged(t *testing.T) {
	s := newTestTargetsServer(t)
	out := testTargetsText(t, s, map[string]any{"ids": "pkg/a.go::A", "format": "json"})

	var payload struct {
		TestTargets []struct {
			File      string   `json:"file"`
			Functions []string `json:"functions"`
		} `json:"test_targets"`
		RunCommands []string `json:"run_commands"`
		TotalFiles  int      `json:"total_files"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("non-compact output must stay JSON: %v\n%s", err, out)
	}
	require.Len(t, payload.TestTargets, 2)
	// Sorting applies to the JSON path too — it was nondeterministic before.
	require.Equal(t, "pkg/a_test.go", payload.TestTargets[0].File)
	require.Equal(t, []string{"TestAlpha", "TestZeta"}, payload.TestTargets[0].Functions)
	require.NotEmpty(t, payload.RunCommands)
}
