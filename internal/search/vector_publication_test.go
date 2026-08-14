package search

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph"
)

type publicationTestDelegate struct {
	mu     sync.Mutex
	hits   []graph.VectorHit
	limits []int
}

func (d *publicationTestDelegate) SimilarTo(_ []float32, limit int) ([]graph.VectorHit, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.limits = append(d.limits, limit)
	return append([]graph.VectorHit(nil), d.hits...), nil
}

type publicationTestEmbedder struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (e *publicationTestEmbedder) Embed(_ context.Context, _ string) ([]float32, error) {
	if e.started != nil {
		e.once.Do(func() { close(e.started) })
		<-e.release
	}
	return []float32{1, 0}, nil
}

func (e *publicationTestEmbedder) EmbedBatch(_ context.Context, texts []string) ([][]float32, error) {
	vectors := make([][]float32, len(texts))
	for i := range vectors {
		vectors[i] = []float32{1, 0}
	}
	return vectors, nil
}

func (*publicationTestEmbedder) Dimensions() int { return 2 }
func (*publicationTestEmbedder) Close() error    { return nil }

type publicationTestTextBackend struct {
	mu         sync.Mutex
	closeCount int
}

func (*publicationTestTextBackend) Add(string, ...string)             {}
func (*publicationTestTextBackend) Remove(string)                     {}
func (*publicationTestTextBackend) Search(string, int) []SearchResult { return nil }
func (*publicationTestTextBackend) Count() int                        { return 1 }

func (b *publicationTestTextBackend) Close() {
	b.mu.Lock()
	b.closeCount++
	b.mu.Unlock()
}

func (b *publicationTestTextBackend) closes() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.closeCount
}

func TestNewDelegatedVectorHasNoHeapIndexAndUsesDurableStats(t *testing.T) {
	delegate := &publicationTestDelegate{hits: []graph.VectorHit{
		{NodeID: "symbol-a#chunk0", ParentID: "symbol-a"},
		{NodeID: "symbol-a#chunk1", ParentID: "symbol-a"},
		{NodeID: "symbol-b"},
	}}
	backend := NewDelegatedVector(384, delegate, 17, 2)

	if backend.graph != nil {
		t.Fatal("delegated backend allocated an in-process HNSW graph")
	}
	if got := backend.Count(); got != 17 {
		t.Fatalf("Count() = %d, want complete durable count 17", got)
	}
	if !backend.HasChunks() {
		t.Fatal("HasChunks() = false, want durable chunk metadata")
	}
	if got := backend.SizeBytes(); got != 0 {
		t.Fatalf("SizeBytes() = %d, want zero process-local vector bytes", got)
	}
	if got, want := backend.Search([]float32{1, 0}, 8), []string{"symbol-a", "symbol-b"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Search() = %v, want parent-deduplicated %v", got, want)
	}
}

func TestSetDelegateReleasesHeapAndChunkState(t *testing.T) {
	backend := NewVector(2)
	backend.Add("symbol-a#chunk0", []float32{1, 0})
	backend.Add("symbol-b", []float32{0, 1})
	backend.SetChunkMap(map[string]string{"symbol-a#chunk0": "symbol-a"})
	if backend.SizeBytes() == 0 {
		t.Fatal("heap backend reported no retained vector bytes before delegation")
	}

	backend.SetDelegate(&publicationTestDelegate{})

	if backend.graph != nil {
		t.Fatal("SetDelegate retained the old HNSW graph")
	}
	if backend.chunkMap != nil {
		t.Fatal("SetDelegate retained the old chunk ownership map")
	}
	if got := backend.Count(); got != 0 {
		t.Fatalf("Count() = %d after mode switch, want fresh delegated count", got)
	}
	if backend.HasChunks() {
		t.Fatal("SetDelegate retained old chunk statistics")
	}
	if got := backend.SizeBytes(); got != 0 {
		t.Fatalf("SizeBytes() = %d after delegation, want zero", got)
	}

	// Preserve the legacy build path: Add records successfully accepted
	// delegate-side writes without rebuilding an HNSW graph.
	backend.Add("symbol-c", []float32{1, 0})
	if got := backend.Count(); got != 1 {
		t.Fatalf("Count() = %d after delegated Add, want 1", got)
	}
	if backend.graph != nil {
		t.Fatal("delegated Add allocated an HNSW graph")
	}
}

func TestAcquireBackendPinsReplacementUntilRelease(t *testing.T) {
	text := &publicationTestTextBackend{}
	embedder := &publicationTestEmbedder{}
	oldVector := NewVector(2)
	oldVector.Add("old-symbol", []float32{1, 0})
	swappable := NewSwappable(NewHybrid(text, oldVector, embedder))

	pinned, release := swappable.AcquireBackend()
	if _, ok := pinned.(*HybridBackend); !ok {
		t.Fatalf("AcquireBackend() returned %T, want *HybridBackend", pinned)
	}

	newVector := NewDelegatedVector(2, &publicationTestDelegate{}, 1, 0)
	replaceStarted := make(chan struct{})
	replaceDone := make(chan struct{})
	go func() {
		close(replaceStarted)
		swappable.ReplaceHybridVector(newVector, embedder)
		close(replaceDone)
	}()
	<-replaceStarted

	select {
	case <-replaceDone:
		t.Fatal("replacement completed while AcquireBackend pin was held")
	case <-time.After(50 * time.Millisecond):
	}
	if oldVector.SizeBytes() == 0 {
		t.Fatal("pinned backend was retired before release")
	}

	release()
	release() // The release contract is intentionally idempotent.
	select {
	case <-replaceDone:
	case <-time.After(2 * time.Second):
		t.Fatal("replacement did not complete after AcquireBackend release")
	}
	if got := oldVector.SizeBytes(); got != 0 {
		t.Fatalf("retired vector retains %d bytes after release", got)
	}

	swappable.Close()
	if got := text.closes(); got != 1 {
		t.Fatalf("text backend close count = %d, want 1", got)
	}
}

func TestReplaceHybridVectorWaitsForReadersAndTransfersTextOwnership(t *testing.T) {
	text := &publicationTestTextBackend{}
	oldVector := NewVector(2)
	oldVector.Add("old-symbol", []float32{1, 0})
	oldEmbedder := &publicationTestEmbedder{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	swappable := NewSwappable(NewHybrid(text, oldVector, oldEmbedder))

	oldResults := make(chan []string, 1)
	go func() {
		ids, _ := swappable.VectorChannelOnly("old", 1)
		oldResults <- ids
	}()
	<-oldEmbedder.started

	newDelegate := &publicationTestDelegate{hits: []graph.VectorHit{{
		NodeID:   "new-symbol#chunk0",
		ParentID: "new-symbol",
	}}}
	newVector := NewDelegatedVector(2, newDelegate, 1, 1)
	newEmbedder := &publicationTestEmbedder{}
	replaceStarted := make(chan struct{})
	replaceDone := make(chan struct{})
	go func() {
		close(replaceStarted)
		swappable.ReplaceHybridVector(newVector, newEmbedder)
		close(replaceDone)
	}()
	<-replaceStarted

	select {
	case <-replaceDone:
		t.Fatal("replacement completed while an old-backend reader was active")
	case <-time.After(50 * time.Millisecond):
	}
	if oldVector.SizeBytes() == 0 {
		t.Fatal("old vector was retired before its active reader completed")
	}

	close(oldEmbedder.release)
	if got, want := <-oldResults, []string{"old-symbol"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("in-flight reader saw %v, want complete old result %v", got, want)
	}
	select {
	case <-replaceDone:
	case <-time.After(2 * time.Second):
		t.Fatal("replacement did not complete after the old reader drained")
	}

	if got := oldVector.SizeBytes(); got != 0 {
		t.Fatalf("retired vector retains %d bytes, want zero", got)
	}
	if got := text.closes(); got != 0 {
		t.Fatalf("transferred text backend closed %d times during replacement", got)
	}
	if got, want := vectorIDs(swappable, "new"), []string{"new-symbol"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("new reader saw %v, want complete new result %v", got, want)
	}

	current, ok := swappable.Inner().(*HybridBackend)
	if !ok {
		t.Fatalf("active backend is %T, want *HybridBackend", swappable.Inner())
	}
	if current.TextBackend() != text {
		t.Fatal("replacement did not retain the original text backend")
	}
	if _, nested := current.TextBackend().(*HybridBackend); nested {
		t.Fatal("replacement nested a HybridBackend inside another HybridBackend")
	}

	swappable.Close()
	if got := text.closes(); got != 1 {
		t.Fatalf("text backend close count = %d, want exactly one final close", got)
	}
	if got := newVector.Count(); got != 0 {
		t.Fatalf("final vector Count() = %d after close, want 0", got)
	}
}

func TestReplaceHybridVectorFlattensAndSupportsRepeatedReplacement(t *testing.T) {
	text := &publicationTestTextBackend{}
	embedder := &publicationTestEmbedder{}
	first := NewVector(2)
	first.Add("first", []float32{1, 0})
	second := NewVector(2)
	second.Add("second", []float32{1, 0})
	legacyNested := NewHybrid(NewHybrid(text, first, embedder), second, embedder)
	swappable := NewSwappable(legacyNested)

	third := NewDelegatedVector(2, &publicationTestDelegate{}, 3, 0)
	swappable.ReplaceHybridVector(third, embedder)
	assertSingleHybrid(t, swappable, text)
	if first.Count() != 0 || second.Count() != 0 {
		t.Fatalf("flattening did not retire both legacy vectors: first=%d second=%d", first.Count(), second.Count())
	}

	fourth := NewDelegatedVector(2, &publicationTestDelegate{}, 4, 1)
	swappable.ReplaceHybridVector(fourth, embedder)
	assertSingleHybrid(t, swappable, text)
	if got := third.Count(); got != 0 {
		t.Fatalf("repeated replacement retained prior delegated vector count %d", got)
	}
	if got := text.closes(); got != 0 {
		t.Fatalf("text backend closed %d times before final Swappable.Close", got)
	}

	swappable.Close()
	if got := text.closes(); got != 1 {
		t.Fatalf("text backend close count = %d, want 1", got)
	}
	if got := fourth.Count(); got != 0 {
		t.Fatalf("last vector Count() = %d after close, want 0", got)
	}
}

func vectorIDs(swappable *Swappable, query string) []string {
	ids, _ := swappable.VectorChannelOnly(query, 1)
	return ids
}

func assertSingleHybrid(t *testing.T, swappable *Swappable, wantText Backend) {
	t.Helper()
	hybrid, ok := swappable.Inner().(*HybridBackend)
	if !ok {
		t.Fatalf("active backend is %T, want *HybridBackend", swappable.Inner())
	}
	if hybrid.TextBackend() != wantText {
		t.Fatalf("hybrid text backend is %T, want original %T", hybrid.TextBackend(), wantText)
	}
	if _, nested := hybrid.TextBackend().(*HybridBackend); nested {
		t.Fatal("active HybridBackend contains a nested HybridBackend")
	}
}
