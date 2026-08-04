package mcp

import (
	"bytes"
	"context"
	"strings"
	"testing"

	wire "github.com/gortexhq/gcx-go"
	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
)

// deadSymbolGraph indexes one function nothing uses — the likely_unused
// case, which is what makes a result carry a caveat at all.
func deadSymbolGraph() *graph.Graph {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "a.go", Kind: graph.KindFile, Name: "a.go", FilePath: "a.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "a.go::Dead", Kind: graph.KindFunction, Name: "Dead", FilePath: "a.go", Language: "go"})
	g.AddEdge(&graph.Edge{From: "a.go", To: "a.go::Dead", Kind: graph.EdgeDefines})
	return g
}

// gcxSections decodes every section of a GCX payload into (header, rows).
func gcxSections(t *testing.T, payload string) []struct {
	Header wire.Header
	Rows   []map[string]string
} {
	t.Helper()
	dec := wire.NewDecoder(strings.NewReader(payload))
	var out []struct {
		Header wire.Header
		Rows   []map[string]string
	}
	h, err := dec.Header()
	require.NoError(t, err, "GCX payload must decode")
	for {
		rows, err := dec.All()
		require.NoError(t, err)
		out = append(out, struct {
			Header wire.Header
			Rows   []map[string]string
		}{h, rows})
		next, err := dec.NextSection()
		if err != nil {
			break
		}
		h = next
	}
	return out
}

func gcxPayload(t *testing.T, tool, id string) string {
	t.Helper()
	srv := serverForGraph(t, deadSymbolGraph())
	req := mcplib.CallToolRequest{}
	req.Params.Name = tool
	req.Params.Arguments = map[string]any{"id": id, "format": "gcx"}
	var res *mcplib.CallToolResult
	var err error
	switch tool {
	case "find_usages":
		res, err = srv.handleFindUsages(context.Background(), req)
	case "get_callers":
		res, err = srv.handleGetCallers(context.Background(), req)
	default:
		t.Fatalf("unhandled tool %q", tool)
	}
	require.NoError(t, err)
	require.False(t, res.IsError)
	return res.Content[0].(mcplib.TextContent).Text
}

// A GCX header is space-separated key=value tokens and the wire escaper
// does not encode spaces, so emitting the caveat's prose message as header
// meta made the whole payload undecodable — destroying the rows along with
// the safety wording an agent needs before deleting a symbol. The class
// stays in the header; the prose rides a `<tool>.notes` section.
func TestFindUsages_GCXCaveatPayloadDecodes(t *testing.T) {
	sections := gcxSections(t, gcxPayload(t, "find_usages", "a.go::Dead"))

	require.NotEmpty(t, sections)
	assert.Equal(t, string(graph.ZeroEdgeLikelyUnused), sections[0].Header.Meta["caveat"],
		"the machine-checkable class belongs in the header")
	assert.NotContains(t, sections[0].Header.Meta, "caveat_message",
		"prose must not ride the header — it cannot represent a space")

	var caveat *struct {
		Header wire.Header
		Rows   []map[string]string
	}
	for i := range sections {
		if sections[i].Header.Tool == "find_usages.notes" {
			caveat = &sections[i]
		}
	}
	require.NotNil(t, caveat, "a caveated result must carry a find_usages.notes section")
	require.Len(t, caveat.Rows, 1)
	assert.Equal(t, "caveat", caveat.Rows[0]["kind"])
	assert.Equal(t, string(graph.ZeroEdgeLikelyUnused), caveat.Rows[0]["class"])
	assert.Contains(t, caveat.Rows[0]["message"], "text search",
		"the section must carry the full prose, spaces intact")
}

func TestGetCallers_GCXCaveatPayloadDecodes(t *testing.T) {
	sections := gcxSections(t, gcxPayload(t, "get_callers", "a.go::Dead"))

	var caveat *struct {
		Header wire.Header
		Rows   []map[string]string
	}
	for i := range sections {
		if sections[i].Header.Tool == "get_callers.notes" {
			caveat = &sections[i]
		}
	}
	require.NotNil(t, caveat, "a caveated get_callers result must carry a notes section")
	require.Len(t, caveat.Rows, 1)
	assert.Equal(t, "caveat", caveat.Rows[0]["kind"])
	assert.Equal(t, string(graph.ZeroEdgeLikelyUnused), caveat.Rows[0]["class"])
	assert.NotEmpty(t, caveat.Rows[0]["message"])
}

// An uncaveated result must keep its previous wire bytes — no empty
// section, no stray header meta.
func TestFindUsages_GCXNoCaveatSectionWhenUncaveated(t *testing.T) {
	g := deadSymbolGraph()
	g.AddNode(&graph.Node{ID: "a.go::Caller", Kind: graph.KindFunction, Name: "Caller", FilePath: "a.go", Language: "go"})
	g.AddEdge(&graph.Edge{From: "a.go", To: "a.go::Caller", Kind: graph.EdgeDefines})
	g.AddEdge(&graph.Edge{From: "a.go::Caller", To: "a.go::Dead", Kind: graph.EdgeCalls,
		FilePath: "a.go", Line: 4, Origin: graph.OriginASTResolved, Confidence: 0.9})

	srv := serverForGraph(t, g)
	req := mcplib.CallToolRequest{}
	req.Params.Name = "find_usages"
	req.Params.Arguments = map[string]any{"id": "a.go::Dead", "format": "gcx"}
	res, err := srv.handleFindUsages(context.Background(), req)
	require.NoError(t, err)
	payload := res.Content[0].(mcplib.TextContent).Text

	assert.NotContains(t, payload, "find_usages.notes")
	sections := gcxSections(t, payload)
	require.NotEmpty(t, sections)
	assert.NotContains(t, sections[0].Header.Meta, "caveat")
	require.Len(t, sections[0].Rows, 1, "the usage row must survive")
}

// The related-tools cue is prose too, and markCueOnce spends its
// once-per-session budget before the encoder runs — so a cue the header
// cannot carry is gone for good. It rides the notes section instead.
func TestFindUsages_GCXRelatedToolsCueRidesNotesSection(t *testing.T) {
	g := deadSymbolGraph()
	srv := serverForGraph(t, g)
	req := mcplib.CallToolRequest{}
	req.Params.Name = "find_usages"
	req.Params.Arguments = map[string]any{"id": "a.go::Dead", "format": "gcx"}
	res, err := srv.handleFindUsages(context.Background(), req)
	require.NoError(t, err)
	payload := res.Content[0].(mcplib.TextContent).Text

	// The cue only fires for a dispatch-heavy target, so drive the encoder
	// directly to pin the wire shape rather than the cue's own trigger.
	var buf bytes.Buffer
	sg := &query.SubGraph{RelatedTools: "target has 3 implementations/overrides; find_implementations lists them"}
	require.NoError(t, writeNotesSection(&buf, "find_usages", sg))
	sections := gcxSections(t, buf.String())
	require.Len(t, sections, 1)
	require.Len(t, sections[0].Rows, 1)
	assert.Equal(t, "related_tools", sections[0].Rows[0]["kind"])
	assert.Contains(t, sections[0].Rows[0]["message"], "find_implementations")

	// And it is no longer attempted as header meta, where it would be dropped.
	assert.NotContains(t, payload, "related_tools=")
}

// newGCX's backstop: a prose value that slips into header meta is dropped
// rather than emitted, so the payload stays decodable and the rows survive.
func TestNewGCX_DropsSpaceBearingHeaderMeta(t *testing.T) {
	var buf bytes.Buffer
	enc := newGCX(&buf, "probe", []string{"a"},
		"safe", "no_spaces",
		"prose", "this value has spaces",
	)
	require.NoError(t, enc.WriteRow("row"))
	require.NoError(t, enc.Close())

	h, err := wire.NewDecoder(strings.NewReader(buf.String())).Header()
	require.NoError(t, err, "a space-bearing meta value must not break the header")
	assert.Equal(t, "no_spaces", h.Meta["safe"])
	assert.NotContains(t, h.Meta, "prose")
}
