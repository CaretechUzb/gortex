package mcp

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
)

// The review family (pr_review_context, suggested_review_questions, the
// review-pack classification) reads the graph through the per-request
// reader, so a session with pushed buffers is reviewed on what it is
// about to commit rather than on what is still on disk. These tests pin
// that: reverting a site to s.graph serves the indexed payload and keeps
// reporting a symbol the buffer deleted.

const (
	rfKeptFile = "repo/edit.go"
	rfGoneFile = "repo/gone.go"
	rfKeptID   = rfKeptFile + "::Kept"
	rfGoneID   = rfGoneFile + "::Gone"
)

// reviewFamilyServer wires a fully constructed server (engine included —
// the diff-context section walks callers through it) over a base graph.
func reviewFamilyServer(t *testing.T, g *graph.Graph) *Server {
	t.Helper()
	return NewServer(query.NewEngine(g), g, nil, nil, zap.NewNop(), nil)
}

// reviewFamilyFixture is the two-file changeset shape the review handlers
// see: one file the buffer re-parsed (Kept moved down the file) and one
// file the buffer emptied (Gone deleted).
func reviewFamilyFixture(t *testing.T) (*Server, *graph.OverlayLayer) {
	t.Helper()
	g := graph.New()
	g.AddNode(&graph.Node{
		ID: rfKeptID, Name: "Kept", Kind: graph.KindFunction,
		FilePath: rfKeptFile, Language: "go", StartLine: 10, EndLine: 14,
	})
	g.AddNode(&graph.Node{
		ID: rfGoneID, Name: "Gone", Kind: graph.KindFunction,
		FilePath: rfGoneFile, Language: "go", StartLine: 3, EndLine: 6,
	})

	layer := graph.NewOverlayLayer()
	layer.MarkFile(rfKeptFile, false)
	layer.AddNode(rfKeptFile, &graph.Node{
		ID: rfKeptID, Name: "Kept", Kind: graph.KindFunction,
		FilePath: rfKeptFile, Language: "go", StartLine: 40, EndLine: 44,
	})
	layer.MarkFile(rfGoneFile, false)
	layer.MarkRemoved("Gone", rfGoneID)

	return reviewFamilyServer(t, g), layer
}

// reviewFamilyPRReview is the slice of the pr_review_context envelope
// these tests read: the changed-file roll-up and the per-symbol rows,
// each carrying the start line whose provenance is under test.
type reviewFamilyPRReview struct {
	ChangedFiles []string `json:"changed_files"`
	DiffContext  []struct {
		ID   string `json:"id"`
		Line int    `json:"start_line"`
	} `json:"diff_context"`
}

// prReviewDiffContextFor drives pr_review_context over an explicit id set
// (no working tree needed) and decodes the diff_context section.
func prReviewDiffContextFor(t *testing.T, s *Server, ctx context.Context) reviewFamilyPRReview {
	t.Helper()
	res, err := s.handlePRReviewContext(ctx, makeReq("pr_review_context", map[string]any{
		"ids":      rfKeptID + "," + rfGoneID,
		"sections": "diff_context",
	}))
	require.NoError(t, err)
	require.NotNil(t, res)
	require.False(t, res.IsError, "pr_review_context errored: %s", toolResultText(res))
	var out reviewFamilyPRReview
	require.NoError(t, json.Unmarshal([]byte(toolResultText(res)), &out))
	return out
}

// TestPRReviewContextReflectsOverlay is the handler-level proof for the
// prReviewDiffFromIDs → buildDiffContextSection path: the changed-file
// roll-up and every enriched symbol row come from the caller's buffers.
func TestPRReviewContextReflectsOverlay(t *testing.T) {
	srv, layer := reviewFamilyFixture(t)

	lineByID := func(out reviewFamilyPRReview) map[string]int {
		m := make(map[string]int, len(out.DiffContext))
		for _, d := range out.DiffContext {
			m[d.ID] = d.Line
		}
		return m
	}

	onBase := prReviewDiffContextFor(t, srv, context.Background())
	assert.ElementsMatch(t, []string{rfKeptFile, rfGoneFile}, onBase.ChangedFiles,
		"a plain request reports both indexed files")
	baseLines := lineByID(onBase)
	assert.Equal(t, 10, baseLines[rfKeptID], "a plain request reports the indexed line")
	assert.Contains(t, baseLines, rfGoneID, "a plain request still enriches the indexed symbol")

	onView := prReviewDiffContextFor(t, srv, overlayCtx(t, srv, layer))
	assert.Equal(t, []string{rfKeptFile}, onView.ChangedFiles,
		"the file the buffer emptied must drop out of the changeset")
	viewLines := lineByID(onView)
	assert.Equal(t, 40, viewLines[rfKeptID], "the buffer's payload must replace the indexed one")
	assert.NotContains(t, viewLines, rfGoneID, "a symbol the buffer deleted must not be enriched")

	assert.Len(t, srv.graph.AllNodes(), 2, "the overlay request must not mutate the base store")
}

// TestClassifyChangedSymbolsReadsThroughRequestReader pins the review
// package's Reader widening (SymbolHunk + ClassifyChange): the change
// class is decided on the buffer's node kind, not the indexed one.
func TestClassifyChangedSymbolsReadsThroughRequestReader(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{
		ID: rfKeptID, Name: "Kept", Kind: graph.KindFunction,
		FilePath: rfKeptFile, Language: "go", StartLine: 10, EndLine: 14,
	})
	srv := reviewFamilyServer(t, g)

	// The buffer turned the function into a constant under the same id.
	layer := graph.NewOverlayLayer()
	layer.MarkFile(rfKeptFile, false)
	layer.AddNode(rfKeptFile, &graph.Node{
		ID: rfKeptID, Name: "Kept", Kind: graph.KindConstant,
		FilePath: rfKeptFile, Language: "go", StartLine: 10, EndLine: 10,
	})

	diff := &analysis.DiffResult{
		ChangedFiles:   []string{rfKeptFile},
		ChangedSymbols: []analysis.ChangedSymbol{{ID: rfKeptID, Name: "Kept", FilePath: rfKeptFile}},
	}

	onBase := srv.classifyChangedSymbols(context.Background(), diff, nil)
	require.Len(t, onBase, 1)
	assert.NotEqual(t, "config", onBase[0].Class,
		"a plain request classifies against the indexed function node")

	onView := srv.classifyChangedSymbols(overlayCtx(t, srv, layer), diff, nil)
	require.Len(t, onView, 1)
	assert.Equal(t, "config", onView[0].Class,
		"the buffer's node kind must decide the change class")
}

const (
	rqHubID      = "p/hub.go::Hub"
	rqCallerAID  = "p/a.go::A"
	rqCallerBID  = "p/b.go::B"
	rqTestFile   = "p/hub_test.go"
	rqTestSymbol = rqTestFile + "::TestHub"
)

// reviewQuestionsFixture wires a load-bearing symbol (two callers) that
// the index shows is covered by a test, plus the layer for a session
// whose buffer deleted that test.
func reviewQuestionsFixture(t *testing.T) (*Server, *graph.OverlayLayer) {
	t.Helper()
	g := graph.New()
	g.AddNode(&graph.Node{ID: rqHubID, Name: "Hub", Kind: graph.KindFunction, FilePath: "p/hub.go", Language: "go", StartLine: 5})
	g.AddNode(&graph.Node{ID: rqCallerAID, Name: "A", Kind: graph.KindFunction, FilePath: "p/a.go", Language: "go", StartLine: 3})
	g.AddNode(&graph.Node{ID: rqCallerBID, Name: "B", Kind: graph.KindFunction, FilePath: "p/b.go", Language: "go", StartLine: 3})
	g.AddNode(&graph.Node{ID: rqTestSymbol, Name: "TestHub", Kind: graph.KindFunction, FilePath: rqTestFile, Language: "go", StartLine: 7})
	g.AddEdge(&graph.Edge{From: rqCallerAID, To: rqHubID, Kind: graph.EdgeCalls})
	g.AddEdge(&graph.Edge{From: rqCallerBID, To: rqHubID, Kind: graph.EdgeCalls})
	g.AddEdge(&graph.Edge{From: rqTestSymbol, To: rqHubID, Kind: graph.EdgeTests})

	layer := graph.NewOverlayLayer()
	layer.MarkFile(rqTestFile, false)
	layer.MarkRemoved("TestHub", rqTestSymbol)

	return reviewFamilyServer(t, g), layer
}

// untestedHotspotSymbols drives suggested_review_questions narrowed to the
// untested-hotspot category and returns the symbol ids it flagged.
func untestedHotspotSymbols(t *testing.T, s *Server, ctx context.Context) []string {
	t.Helper()
	res, err := s.handleSuggestedReviewQuestions(ctx, makeReq("suggested_review_questions", map[string]any{
		"categories":    "untested_hotspot",
		"hub_threshold": float64(2),
	}))
	require.NoError(t, err)
	require.NotNil(t, res)
	require.False(t, res.IsError, "suggested_review_questions errored: %s", toolResultText(res))

	var out struct {
		Questions []struct {
			SymbolID string `json:"symbol_id"`
		} `json:"questions"`
	}
	require.NoError(t, json.Unmarshal([]byte(toolResultText(res)), &out))
	ids := make([]string, 0, len(out.Questions))
	for _, q := range out.Questions {
		ids = append(ids, q.SymbolID)
	}
	return ids
}

// TestSuggestedReviewQuestionsReflectsOverlay is the handler-level proof
// for the fan-count / inbound-test walk: once the buffer deletes the only
// covering test, the hub is reported as an untested hotspot even though
// the index still carries the tests edge.
func TestSuggestedReviewQuestionsReflectsOverlay(t *testing.T) {
	srv, layer := reviewQuestionsFixture(t)

	onBase := untestedHotspotSymbols(t, srv, context.Background())
	assert.NotContains(t, onBase, rqHubID,
		"the indexed tests edge must keep the hub off the untested list")

	onView := untestedHotspotSymbols(t, srv, overlayCtx(t, srv, layer))
	assert.Contains(t, onView, rqHubID,
		"deleting the covering test in the buffer must surface the hub")

	assert.Len(t, srv.graph.AllNodes(), 4, "the overlay request must not mutate the base store")
}
