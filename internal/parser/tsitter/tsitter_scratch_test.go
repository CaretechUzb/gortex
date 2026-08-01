package tsitter

import (
	"testing"
	"unsafe"
)

func TestNodeWithScratchRewindsNestedScopes(t *testing.T) {
	a := newNodeArena()
	root := a.alloc()
	root.valid = true
	root.arena = a
	persistent := a.alloc()
	persistent.valid = true
	persistent.arena = a
	entry := a.mark()

	var outer, inner, reused *Node
	root.WithScratch(func() {
		outer = a.alloc()
		outer.valid = true
		outer.arena = a
		nestedEntry := a.mark()

		root.WithScratch(func() {
			inner = a.alloc()
			inner.valid = true
			inner.arena = a
		})

		if got := a.mark(); got != nestedEntry {
			t.Fatalf("nested scope cursor = %+v, want %+v", got, nestedEntry)
		}
		if inner.valid || inner.arena != nil {
			t.Fatal("nested scope left its Node wrapper live")
		}
		reused = a.alloc()
		if reused != inner {
			t.Fatal("nested scope did not make its released slot reusable")
		}
		reused.valid = true
		reused.arena = a
	})

	if got := a.mark(); got != entry {
		t.Fatalf("outer scope cursor = %+v, want %+v", got, entry)
	}
	if !persistent.valid || persistent.arena != a {
		t.Fatal("scope invalidated a Node allocated before entry")
	}
	if outer.valid || outer.arena != nil || reused.valid || reused.arena != nil {
		t.Fatal("outer scope left a scratch Node wrapper live")
	}
}

func TestNodeWithScratchRewindsAfterPanic(t *testing.T) {
	a := newNodeArena()
	root := a.alloc()
	root.valid = true
	root.arena = a
	entry := a.mark()

	var scratch *Node
	var recovered any
	func() {
		defer func() { recovered = recover() }()
		root.WithScratch(func() {
			scratch = a.alloc()
			scratch.valid = true
			scratch.arena = a
			panic("sentinel")
		})
	}()

	if recovered != "sentinel" {
		t.Fatalf("recovered = %v, want sentinel", recovered)
	}
	if got := a.mark(); got != entry {
		t.Fatalf("cursor after panic = %+v, want %+v", got, entry)
	}
	if scratch.valid || scratch.arena != nil {
		t.Fatal("panic path left its scratch Node wrapper live")
	}
}

func TestNodeWithScratchBoundsMultiPassHighWater(t *testing.T) {
	a := newNodeArena()
	root := a.alloc()
	root.valid = true
	root.arena = a

	const (
		passes       = 9
		nodesPerPass = 12_000
	)
	var firstPassBytes uintptr
	for pass := 0; pass < passes; pass++ {
		root.WithScratch(func() {
			for i := 0; i < nodesPerPass; i++ {
				n := a.alloc()
				n.valid = true
				n.arena = a
			}
		})
		if pass == 0 {
			firstPassBytes = a.retainedBytes()
			continue
		}
		if got := a.retainedBytes(); got != firstPassBytes {
			t.Fatalf("pass %d retained %d bytes, first pass retained %d", pass+1, got, firstPassBytes)
		}
	}

	// Model the previous monotonic lifetime: the same nine traversals append
	// into one arena until Tree.Close. The exact ratio varies with geometric
	// chunk rounding, but scratch scoping must keep substantially less capacity.
	monotonic := newNodeArena()
	for i := 0; i < passes*nodesPerPass+1; i++ {
		monotonic.alloc()
	}
	monotonicBytes := monotonic.retainedBytes()
	if firstPassBytes*4 >= monotonicBytes {
		t.Fatalf("scratch retained %d bytes versus monotonic %d; want at least 4x reduction", firstPassBytes, monotonicBytes)
	}
	t.Logf("scratch high-water %d bytes; monotonic nine-pass high-water %d bytes", firstPassBytes, monotonicBytes)
}

func TestPutArenaCapsOversizedHighWater(t *testing.T) {
	a := newNodeArena()
	count := int(arenaPoolRetainBytes/uintptr(unsafe.Sizeof(Node{}))) + arenaMaxChunk + 1
	for i := 0; i < count; i++ {
		n := a.alloc()
		n.valid = true
		n.arena = a
	}
	if got := a.retainedBytes(); got <= arenaPoolRetainBytes {
		t.Fatalf("test arena retained %d bytes, want more than %d", got, arenaPoolRetainBytes)
	}
	headers := a.chunks

	putArena(a)
	if got := a.retainedBytes(); got == 0 || got > arenaPoolRetainBytes {
		t.Fatalf("capped arena retained %d bytes, want 1..%d", got, arenaPoolRetainBytes)
	}
	for i := len(a.chunks); i < len(headers); i++ {
		if headers[i] != nil {
			t.Fatalf("discarded chunk header %d still retains its Node slab", i)
		}
	}
	if a.ci != 0 || a.used != 0 {
		t.Fatalf("oversized arena cursor = (%d,%d), want (0,0)", a.ci, a.used)
	}
	// The sync.Pool holds this same pointer. Clear it after inspection so the
	// test itself does not deliberately retain an 8 MiB fixture until process
	// exit.
	clear(a.chunks)
	a.chunks = nil
}
