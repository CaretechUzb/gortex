package mcp

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search/rerank"
)

func exactNameAnchorServer(t *testing.T, nodes ...*graph.Node) *Server {
	t.Helper()
	g := graph.New()
	for _, node := range nodes {
		g.AddNode(node)
	}
	return &Server{graph: g}
}

func TestExploreTaskAnchorsAdmitLowercaseProseSymbolName(t *testing.T) {
	vprintf := &graph.Node{
		ID: "src/print.c::vprintf", Name: "vprintf",
		Kind: graph.KindFunction, FilePath: "src/print.c", StartLine: 40, EndLine: 88,
	}
	server := exactNameAnchorServer(t, vprintf)
	task := "printing garbles output when the width is negative in vprintf"
	anchors := server.exploreTaskAnchors(context.Background(), task, nil, query.QueryOptions{})
	if len(anchors) != 1 {
		t.Fatalf("anchors = %#v, want the indexed lowercase name", anchors)
	}
	if anchors[0].compact != "vprintf" {
		t.Fatalf("anchor compact = %q, want vprintf", anchors[0].compact)
	}
	got := server.exploreExactAnchorCandidate(
		context.Background(), anchors[0], query.QueryOptions{},
		map[string]struct{}{}, map[string]struct{}{},
	)
	if got == nil || got.Node == nil || got.Node.ID != vprintf.ID {
		t.Fatalf("exact name candidate = %#v, want %q", got, vprintf.ID)
	}
}

func TestReserveExploreSyntacticAnchorCandidatesKeepsGraphResolvedAnchor(t *testing.T) {
	candidates := []*rerank.Candidate{
		{Node: &graph.Node{ID: "semantic", Name: "print_width", Kind: graph.KindFunction, FilePath: "src/fmt.c"}},
		{Node: &graph.Node{ID: "noise-a", Name: "pad_left", Kind: graph.KindFunction, FilePath: "src/pad.c"}},
		{Node: &graph.Node{ID: "noise-b", Name: "pad_right", Kind: graph.KindFunction, FilePath: "src/pad.c"}},
		{Node: &graph.Node{ID: "vprintf", Name: "vprintf", Kind: graph.KindFunction, FilePath: "src/print.c"}},
	}
	// The task carries no code-shaped token, so the graph-resolved anchor is
	// recorded past the task-shaped anchor list and can only be reserved by ID.
	got := reserveExploreSyntacticAnchorCandidates(
		"printing garbles output when the width is negative in vprintf",
		candidates, map[int]string{0: "vprintf"}, 2,
	)
	if len(got) < 2 {
		t.Fatalf("reserved = %#v, want the semantic head plus the anchor", got)
	}
	if got[0].Node.ID != "semantic" || got[1].Node.ID != "vprintf" {
		t.Fatalf("reserved order = [%s, %s], want semantic head then vprintf", got[0].Node.ID, got[1].Node.ID)
	}
}

func TestExploreTaskAnchorsSkipAmbiguousPlainName(t *testing.T) {
	nodes := make([]*graph.Node, 0, 9)
	for index := 0; index < 9; index++ {
		nodes = append(nodes, &graph.Node{
			ID:   fmt.Sprintf("pkg/h%d.go::handle", index),
			Name: "handle", Kind: graph.KindFunction,
			FilePath: fmt.Sprintf("pkg/h%d.go", index),
		})
	}
	server := exactNameAnchorServer(t, nodes...)
	task := "requests handle poorly under sustained load"
	if anchors := server.exploreTaskAnchors(context.Background(), task, nil, query.QueryOptions{}); len(anchors) != 0 {
		t.Fatalf("anchors = %#v, want no anchor for a name shared by too many symbols", anchors)
	}

	pool := []*rerank.Candidate{{Node: &graph.Node{
		ID: "pkg/h3.go::serve", Name: "serve", Kind: graph.KindFunction, FilePath: "pkg/h3.go",
	}}}
	anchors := server.exploreTaskAnchors(context.Background(), task, pool, query.QueryOptions{})
	if len(anchors) != 1 || anchors[0].compact != "handle" {
		t.Fatalf("anchors = %#v, want the ranked-pool file to disambiguate handle", anchors)
	}
	if len(anchors[0].exactNodes) != 1 || anchors[0].exactNodes[0].FilePath != "pkg/h3.go" {
		t.Fatalf("exact nodes = %#v, want only the ranked-pool declaration", anchors[0].exactNodes)
	}
}

func TestExploreTaskAnchorsKeepCodeShapedAnchorsAhead(t *testing.T) {
	vprintf := &graph.Node{
		ID: "src/print.c::vprintf", Name: "vprintf",
		Kind: graph.KindFunction, FilePath: "src/print.c",
	}
	server := exactNameAnchorServer(t, vprintf)

	full := server.exploreTaskAnchors(
		context.Background(),
		"buildContent then flushBuffer then handleBatch all break vprintf",
		nil, query.QueryOptions{},
	)
	if len(full) != exploreSyntacticAnchorMaxTerms {
		t.Fatalf("anchors = %#v, want the three code-shaped anchors", full)
	}
	for _, anchor := range full {
		if anchor.compact == "vprintf" {
			t.Fatalf("plain exact name displaced a code-shaped anchor: %#v", full)
		}
	}

	partial := server.exploreTaskAnchors(
		context.Background(),
		"buildContent then flushBuffer both break vprintf",
		nil, query.QueryOptions{},
	)
	if len(partial) != exploreSyntacticAnchorMaxTerms {
		t.Fatalf("anchors = %#v, want the plain exact name to fill the free slot", partial)
	}
	if partial[2].compact != "vprintf" {
		t.Fatalf("anchor order = %#v, want vprintf last", partial)
	}
}

func TestExploreExactNameAnchorTokensAreBoundedAndDeduplicated(t *testing.T) {
	words := []string{
		"alpha", "bravo", "delta", "gamma", "kappa", "sigma", "theta", "omega",
		"cargo", "flare", "gauge", "hover", "index", "joint", "knurl", "latch",
		"mount", "nudge", "orbit", "plumb",
	}
	got := exploreExactNameAnchorTokens(strings.Join(words, " "))
	if len(got) != exploreExactNameAnchorMaxTokens {
		t.Fatalf("tokens = %d, want the %d-token cap", len(got), exploreExactNameAnchorMaxTokens)
	}
	if deduped := exploreExactNameAnchorTokens("mount mount latch"); len(deduped) != 2 {
		t.Fatalf("tokens = %#v, want deduplicated candidates", deduped)
	}
	if short := exploreExactNameAnchorTokens("the fix is in fmt and the tests"); len(short) != 0 {
		t.Fatalf("tokens = %#v, want short, generic and stopword tokens rejected", short)
	}
	if shaped := exploreExactNameAnchorTokens("buildContent and mount"); len(shaped) != 1 || shaped[0] != "mount" {
		t.Fatalf("tokens = %#v, want only the plain token — code-shaped tokens own the syntactic lane", shaped)
	}
}

func TestExploreSyntacticAnchorsResolveDottedOwnerMember(t *testing.T) {
	anchors := exploreSyntacticAnchors("strip long messages in HipChatHandler.buildContent")
	if len(anchors) != 1 {
		t.Fatalf("anchors = %#v, want the dotted qualified member", anchors)
	}
	if anchors[0].qualifiedName != "HipChatHandler.buildContent" {
		t.Fatalf("qualified graph name = %q, want HipChatHandler.buildContent", anchors[0].qualifiedName)
	}
	if anchors[0].query != "HipChatHandler buildContent" {
		t.Fatalf("qualified member lookup = %q, want literal owner and member", anchors[0].query)
	}
	for _, anchor := range exploreSyntacticAnchors("panic in tree.go getValue and package.json") {
		if anchor.qualifiedName != "" {
			t.Fatalf("file mention became a qualified member: %#v", anchor)
		}
	}
}

func TestExploreDottedQualifiedMentionResolvesMemberAndOwner(t *testing.T) {
	member := &graph.Node{
		ID:   "src/Handler/HipChatHandler.php::HipChatHandler.buildContent",
		Name: "buildContent", Kind: graph.KindMethod,
		FilePath: "src/Handler/HipChatHandler.php", StartLine: 89,
	}
	owner := &graph.Node{
		ID:   "src/Handler/HipChatHandler.php::HipChatHandler",
		Name: "HipChatHandler", Kind: graph.KindType,
		FilePath: "src/Handler/HipChatHandler.php", StartLine: 20,
	}
	elsewhere := &graph.Node{
		ID:   "src/Handler/Legacy/HipChatHandler.php::HipChatHandler",
		Name: "HipChatHandler", Kind: graph.KindType,
		FilePath: "src/Handler/Legacy/HipChatHandler.php", StartLine: 12,
	}
	server := exactNameAnchorServer(t, member, owner, elsewhere)
	anchors := exploreSyntacticAnchors("strip long messages in HipChatHandler.buildContent")
	if len(anchors) != 1 {
		t.Fatalf("anchors = %#v, want the dotted qualified member", anchors)
	}
	got := server.exploreExactAnchorCandidate(
		context.Background(), anchors[0], query.QueryOptions{},
		map[string]struct{}{}, map[string]struct{}{},
	)
	if got == nil || got.Node == nil || got.Node.ID != member.ID {
		t.Fatalf("dotted member candidate = %#v, want %q", got, member.ID)
	}
	ownerCandidate := server.exploreQualifiedAnchorOwnerCandidate(
		context.Background(), anchors[0], got.Node, query.QueryOptions{}, map[string]struct{}{},
	)
	if ownerCandidate == nil || ownerCandidate.Node == nil || ownerCandidate.Node.ID != owner.ID {
		t.Fatalf("owner candidate = %#v, want the owner declared beside the member", ownerCandidate)
	}
}
