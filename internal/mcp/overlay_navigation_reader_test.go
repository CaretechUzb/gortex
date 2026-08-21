package mcp

import (
	"context"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
)

const (
	navRepo      = "repo"
	navStableFl  = "repo/stable.go"
	navBufferFl  = "repo/buffer.go"
	navTargetID  = navStableFl + "::Target"
	navKeptID    = navBufferFl + "::Kept"
	navDroppedID = navBufferFl + "::Dropped"
	navFreshID   = navBufferFl + "::Fresh"
)

// navOverlayServer wires a base graph plus the layer one editor buffer
// would push for repo/buffer.go: Kept is re-emitted under the same ID
// with a fresh payload, Dropped is gone from the buffer, and Fresh is a
// symbol that exists only in the buffer.
func navOverlayServer(t *testing.T) (*Server, *graph.OverlayLayer) {
	t.Helper()
	base := graph.New()
	base.AddNode(&graph.Node{ID: navTargetID, Name: "Target", Kind: graph.KindInterface, FilePath: navStableFl, RepoPrefix: navRepo})
	base.AddNode(&graph.Node{ID: navKeptID, Name: "Kept", Kind: graph.KindType, FilePath: navBufferFl, RepoPrefix: navRepo, StartLine: 10})
	base.AddNode(&graph.Node{ID: navDroppedID, Name: "Dropped", Kind: graph.KindType, FilePath: navBufferFl, RepoPrefix: navRepo, StartLine: 20})
	base.AddEdge(&graph.Edge{From: navKeptID, To: navTargetID, Kind: graph.EdgeImplements, FilePath: navBufferFl, Line: 10})
	base.AddEdge(&graph.Edge{From: navDroppedID, To: navTargetID, Kind: graph.EdgeImplements, FilePath: navBufferFl, Line: 20})

	layer := graph.NewOverlayLayer()
	layer.MarkFile(navBufferFl, false)
	layer.AddNode(navBufferFl, &graph.Node{
		ID: navKeptID, Name: "Kept", Kind: graph.KindType,
		FilePath: navBufferFl, RepoPrefix: navRepo, StartLine: 40,
	})
	layer.AddNode(navBufferFl, &graph.Node{
		ID: navFreshID, Name: "Fresh", Kind: graph.KindType,
		FilePath: navBufferFl, RepoPrefix: navRepo, StartLine: 60,
	})
	layer.MarkRemoved("Dropped", navDroppedID)
	layer.AddEdge(&graph.Edge{From: navKeptID, To: navTargetID, Kind: graph.EdgeImplements, FilePath: navBufferFl, Line: 41})

	return &Server{graph: base, engine: query.NewEngine(base)}, layer
}

// TestSymbolTargetResolutionReadsThroughRequestReader pins id_resolve's
// name/id resolution to the request reader: an overlay-active call
// resolves a name the buffer introduced, refuses a definition the buffer
// deleted, and reports the buffer's payload for a re-emitted ID — while
// the same calls against a plain context still answer from base.
func TestSymbolTargetResolutionReadsThroughRequestReader(t *testing.T) {
	server, layer := navOverlayServer(t)
	plain := context.Background()
	overlaid := overlayCtx(t, server, layer)

	// Deleted symbol absent: base resolves "Dropped", the overlay does not.
	if got := server.resolveNameToIDs(plain, "Dropped"); len(got) != 1 || got[0] != navDroppedID {
		t.Fatalf("resolveNameToIDs over the base store = %v, want [%s]", got, navDroppedID)
	}
	if got := server.resolveNameToIDs(overlaid, "Dropped"); len(got) != 0 {
		t.Fatalf("resolveNameToIDs served a symbol the buffer deleted: %v", got)
	}
	if id, cands := server.resolveSymbolTarget(overlaid, "Dropped"); id != "Dropped" || cands != nil {
		t.Fatalf("resolveSymbolTarget(%q) = (%q, %v), want the unresolved target back", "Dropped", id, cands)
	}

	// Buffer-only symbol visible: base cannot name it, the overlay can.
	if got := server.resolveNameToIDs(plain, "Fresh"); len(got) != 0 {
		t.Fatalf("base resolved a buffer-only symbol: %v", got)
	}
	if id, cands := server.resolveSymbolTarget(overlaid, "Fresh"); id != navFreshID || cands != nil {
		t.Fatalf("resolveSymbolTarget(%q) = (%q, %v), want %q", "Fresh", id, cands, navFreshID)
	}

	// Replaced payload visible: the re-emitted ID keeps resolving, and the
	// node behind it is the buffer's, not base's.
	if id, _ := server.resolveSymbolTarget(overlaid, "Kept"); id != navKeptID {
		t.Fatalf("resolveSymbolTarget(%q) = %q, want %q", "Kept", id, navKeptID)
	}
	if n := server.readerFor(overlaid).GetNode(navKeptID); n == nil || n.StartLine != 40 {
		t.Fatalf("the request reader served base's payload for %s: %+v", navKeptID, n)
	}

	// The base store must not have been touched by the overlay request.
	if n := server.graph.GetNode(navKeptID); n == nil || n.StartLine != 10 {
		t.Fatalf("the overlay request mutated the base store: %+v", n)
	}
}

// TestDispatchImplementorCountReadsThroughRequestReader pins the
// find_usages dispatch cue to the request reader: the implementor count
// drops the buffer-deleted implementation and keeps the re-emitted one,
// so the cue never advertises dispatch the buffer already removed.
func TestDispatchImplementorCountReadsThroughRequestReader(t *testing.T) {
	server, layer := navOverlayServer(t)

	if got := server.dispatchImplementorCount(context.Background(), navTargetID); got != 2 {
		t.Fatalf("dispatchImplementorCount over the base store = %d, want 2", got)
	}
	if got := server.dispatchImplementorCount(overlayCtx(t, server, layer), navTargetID); got != 1 {
		t.Fatalf("dispatchImplementorCount over the request reader = %d, want 1", got)
	}
}

// TestWhyEntriesReadThroughRequestReader pins the why handler's rationale
// projection to the request reader: the entry for a re-emitted rationale
// carries the buffer's text, and a rationale the buffer deleted no longer
// motivates the symbol.
func TestWhyEntriesReadThroughRequestReader(t *testing.T) {
	base := graph.New()
	base.AddNode(&graph.Node{ID: navTargetID, Name: "Target", Kind: graph.KindFunction, FilePath: navStableFl, RepoPrefix: navRepo})
	base.AddNode(&graph.Node{
		ID: navKeptID, Name: "Kept", Kind: graph.KindRationale, FilePath: navBufferFl, RepoPrefix: navRepo,
		Meta: map[string]any{"rationale_kind": "decision", "section_text": "base text"},
	})
	base.AddNode(&graph.Node{
		ID: navDroppedID, Name: "Dropped", Kind: graph.KindRationale, FilePath: navBufferFl, RepoPrefix: navRepo,
		Meta: map[string]any{"rationale_kind": "decision", "section_text": "stale text"},
	})
	base.AddEdge(&graph.Edge{From: navKeptID, To: navTargetID, Kind: graph.EdgeMotivates, FilePath: navBufferFl, Line: 3})
	base.AddEdge(&graph.Edge{From: navDroppedID, To: navTargetID, Kind: graph.EdgeMotivates, FilePath: navBufferFl, Line: 9})

	layer := graph.NewOverlayLayer()
	layer.MarkFile(navBufferFl, false)
	layer.AddNode(navBufferFl, &graph.Node{
		ID: navKeptID, Name: "Kept", Kind: graph.KindRationale, FilePath: navBufferFl, RepoPrefix: navRepo,
		Meta: map[string]any{"rationale_kind": "decision", "section_text": "buffer text"},
	})
	layer.MarkRemoved("Dropped", navDroppedID)
	layer.AddEdge(&graph.Edge{From: navKeptID, To: navTargetID, Kind: graph.EdgeMotivates, FilePath: navBufferFl, Line: 4})

	server := &Server{graph: base, engine: query.NewEngine(base)}

	baseEntries := server.whyEntriesFor(context.Background(), navTargetID)
	if len(baseEntries) != 2 {
		t.Fatalf("whyEntriesFor over the base store returned %d entries, want 2: %+v", len(baseEntries), baseEntries)
	}

	entries := server.whyEntriesFor(overlayCtx(t, server, layer), navTargetID)
	if len(entries) != 1 {
		t.Fatalf("whyEntriesFor over the request reader returned %d entries, want 1: %+v", len(entries), entries)
	}
	if entries[0].SourceID != navKeptID {
		t.Fatalf("whyEntriesFor kept the wrong rationale: %+v", entries[0])
	}
	if entries[0].Text != "buffer text" {
		t.Fatalf("whyEntriesFor served base's rationale text %q, want the buffer's", entries[0].Text)
	}
}
