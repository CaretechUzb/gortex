package graph

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"testing"
)

func TestFindNodesByNameBoundedCapsAndCancels(t *testing.T) {
	graph := New()
	for index := 0; index < 128; index++ {
		graph.AddNode(&Node{
			ID:       fmt.Sprintf("repo/file-%03d.go::handle", index),
			Name:     "handle",
			Kind:     KindFunction,
			FilePath: fmt.Sprintf("repo/file-%03d.go", index),
		})
	}

	page, err := graph.FindNodesByNameBounded(
		context.Background(), "handle",
		LocalizationNodeScope{Kinds: map[NodeKind]bool{KindFunction: true}},
		8,
	)
	if err != nil {
		t.Fatalf("bounded lookup: %v", err)
	}
	if page.Total != 9 || !page.Truncated || len(page.Nodes) != 8 {
		t.Fatalf("page = %#v, want threshold total 9, truncated, cap 8", page)
	}
	if !sort.SliceIsSorted(page.Nodes, func(i, j int) bool { return page.Nodes[i].ID < page.Nodes[j].ID }) {
		t.Fatalf("nodes are not deterministic: %#v", page.Nodes)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if cancelled, err := graph.FindNodesByNameBounded(ctx, "handle", LocalizationNodeScope{}, 8); err == nil || len(cancelled.Nodes) != 0 {
		t.Fatalf("cancelled lookup = %#v, %v; want empty error result", cancelled, err)
	}
}

func TestFindNodesByNameBoundedWeakSnapshotKeepsPageInvariants(t *testing.T) {
	graph := New()
	var writer sync.WaitGroup
	writer.Add(1)
	go func() {
		defer writer.Done()
		for index := 0; index < 2_000; index++ {
			graph.AddNode(&Node{
				ID:       fmt.Sprintf("repo/live-%04d.go::handle", index),
				Name:     "handle",
				Kind:     KindFunction,
				FilePath: fmt.Sprintf("repo/live-%04d.go", index),
			})
		}
	}()

	for attempt := 0; attempt < 100; attempt++ {
		page, err := graph.FindNodesByNameBounded(context.Background(), "handle", LocalizationNodeScope{}, 16)
		if err != nil {
			t.Fatalf("bounded lookup: %v", err)
		}
		if len(page.Nodes) > 16 || page.Total < len(page.Nodes) {
			t.Fatalf("weak snapshot violated bounds: %#v", page)
		}
		for index := 1; index < len(page.Nodes); index++ {
			if page.Nodes[index-1].ID >= page.Nodes[index].ID {
				t.Fatalf("weak snapshot is not sorted/deduplicated: %#v", page.Nodes)
			}
		}
	}
	writer.Wait()
}

func TestOverlaidViewFindNodesByNameBoundedRefillsAfterFileTombstone(t *testing.T) {
	base := New()
	for index := 0; index < 32; index++ {
		base.AddNode(&Node{
			ID: fmt.Sprintf("repo/a-hidden.go::handle:%02d", index), Name: "handle",
			Kind: KindFunction, FilePath: "repo/a-hidden.go",
		})
	}
	visible := &Node{
		ID: "repo/z-visible.go::handle", Name: "handle", Kind: KindFunction,
		FilePath: "repo/z-visible.go",
	}
	base.AddNode(visible)

	layer := NewOverlayLayer()
	layer.MarkFile("repo/a-hidden.go", true)
	view := NewOverlaidView(base, layer)

	page, err := view.FindNodesByNameBounded(context.Background(), "handle", LocalizationNodeScope{}, 8)
	if err != nil {
		t.Fatalf("bounded overlay lookup: %v", err)
	}
	if page.Total != 1 || page.Truncated || len(page.Nodes) != 1 || page.Nodes[0].ID != visible.ID {
		t.Fatalf("page = %#v, want the later visible homonym after file tombstone", page)
	}
}

type recordingBoundedExactNameReader struct {
	Reader
	bounded BoundedExactNameReader
	limits  []int
}

func (reader *recordingBoundedExactNameReader) FindNodesByNameBounded(
	ctx context.Context,
	name string,
	scope LocalizationNodeScope,
	limit int,
) (BoundedNodeProjection, error) {
	reader.limits = append(reader.limits, limit)
	return reader.bounded.FindNodesByNameBounded(ctx, name, scope, limit)
}

func TestOverlaidViewFindNodesByNameBoundedDoesNotInflateWholeFileShadows(t *testing.T) {
	base := New()
	visible := &Node{
		ID: "repo/visible.go::handle", Name: "handle", Kind: KindFunction,
		FilePath: "repo/visible.go",
	}
	base.AddNode(visible)
	recording := &recordingBoundedExactNameReader{Reader: base, bounded: base}

	layer := NewOverlayLayer()
	layer.MarkFile("repo/generated.go", false)
	for index := 0; index < 2_000; index++ {
		layer.MarkRemoved("handle", fmt.Sprintf("repo/generated.go::handle:%04d", index))
	}
	view := NewOverlaidView(recording, layer)

	page, err := view.FindNodesByNameBounded(context.Background(), "handle", LocalizationNodeScope{}, 8)
	if err != nil {
		t.Fatalf("bounded overlay lookup: %v", err)
	}
	if len(recording.limits) != 1 || recording.limits[0] != 8 {
		t.Fatalf("base limits = %v, want unchanged request limit 8", recording.limits)
	}
	if page.Total != 1 || page.Truncated || len(page.Nodes) != 1 || page.Nodes[0].ID != visible.ID {
		t.Fatalf("page = %#v, want the one visible base homonym", page)
	}
}

func TestOverlaidViewFindNodesByNameBoundedHonorsDetachedRemoval(t *testing.T) {
	base := New()
	stale := &Node{
		ID: "repo/old.go::handle", Name: "handle", Kind: KindFunction,
		FilePath: "repo/old.go",
	}
	base.AddNode(stale)
	recording := &recordingBoundedExactNameReader{Reader: base, bounded: base}

	layer := NewOverlayLayer()
	// MarkRemoved is sufficient in the legacy overlay contract even when the
	// layer was assembled without MarkFile. The bounded path must keep parity.
	layer.MarkRemoved(stale.Name, stale.ID)
	view := NewOverlaidView(recording, layer)

	page, err := view.FindNodesByNameBounded(context.Background(), "handle", LocalizationNodeScope{}, 8)
	if err != nil {
		t.Fatalf("bounded overlay lookup: %v", err)
	}
	if len(recording.limits) != 1 || recording.limits[0] != 9 {
		t.Fatalf("base limits = %v, want one detached-shadow refill slot", recording.limits)
	}
	if page.Total != 0 || len(page.Nodes) != 0 || page.Truncated {
		t.Fatalf("detached removal leaked stale base node: %#v", page)
	}
}
