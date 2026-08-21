package store_sqlite

import (
	"context"
	"math"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

const (
	genCallerID = "repo/caller.go::Caller"
	genFirstID  = "repo/first.go::First"
	genSecondID = "repo/second.go::Second"
)

// openSeededGenerationStore opens an on-disk store carrying a small graph. On
// disk rather than in memory so the WAL-checkpoint loop actually runs — the
// teardown tests need a goroutine that only the owning handle may stop.
func openSeededGenerationStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "generation.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	s.AddBatch([]*graph.Node{
		{ID: genCallerID, Kind: graph.KindFunction, Name: "Caller", FilePath: "repo/caller.go", RepoPrefix: "repo"},
		{ID: genFirstID, Kind: graph.KindFunction, Name: "First", FilePath: "repo/first.go", RepoPrefix: "repo"},
		{ID: genSecondID, Kind: graph.KindFunction, Name: "Second", FilePath: "repo/second.go", RepoPrefix: "repo"},
	}, []*graph.Edge{
		{From: genCallerID, To: genFirstID, Kind: graph.EdgeCalls, FilePath: "repo/caller.go", Line: 3},
		{From: genCallerID, To: genSecondID, Kind: graph.EdgeCalls, FilePath: "repo/caller.go", Line: 4},
	})
	return s
}

// generationReadView is what a handle answers across the read surface. Two
// handles over the same core must produce equal values today, because nothing
// reads viewGen yet.
type generationReadView struct {
	nodeCount  int
	edgeCount  int
	nodeIDs    []string
	callTarget []string
	callerName string
	repos      []string
}

func readThroughHandle(t *testing.T, s *Store) generationReadView {
	t.Helper()
	view := generationReadView{
		nodeCount: s.NodeCount(),
		edgeCount: s.EdgeCount(),
		repos:     s.RepoPrefixes(),
	}
	sort.Strings(view.repos)
	for _, n := range s.AllNodes() {
		view.nodeIDs = append(view.nodeIDs, n.ID)
	}
	sort.Strings(view.nodeIDs)
	for _, e := range s.GetOutEdges(genCallerID) {
		view.callTarget = append(view.callTarget, e.To)
	}
	sort.Strings(view.callTarget)
	if n := s.GetNode(genCallerID); n != nil {
		view.callerName = n.Name
	}
	return view
}

// TestAtGenerationDerivedHandleSharesCore pins the sharing contract: a derived
// handle is a second view over one open database, not a second database. It
// carries the requested generation, borrows the core, and sees writes made
// through any other handle immediately.
func TestAtGenerationDerivedHandleSharesCore(t *testing.T) {
	base := openSeededGenerationStore(t)
	t.Cleanup(func() { _ = base.Close() })

	if got := base.ViewGeneration(); got != baseViewGeneration {
		t.Fatalf("base handle ViewGeneration = %d, want %d", got, baseViewGeneration)
	}

	derived := base.AtGeneration(7)
	if derived == nil {
		t.Fatal("AtGeneration(7) returned nil for a valid generation")
	}
	if got := derived.ViewGeneration(); got != 7 {
		t.Fatalf("derived handle ViewGeneration = %d, want 7", got)
	}
	if derived.storeCore != base.storeCore {
		t.Fatal("derived handle does not share the base handle's core")
	}
	if derived.ownsCore {
		t.Fatal("derived handle claims core ownership; only Open's handle may")
	}
	if derived == base {
		t.Fatal("AtGeneration returned the receiver instead of a new handle")
	}
	if got := base.ViewGeneration(); got != baseViewGeneration {
		t.Fatalf("deriving mutated the base handle: ViewGeneration = %d", got)
	}

	// Nothing reads viewGen yet, so both handles must answer identically.
	if got, want := readThroughHandle(t, derived), readThroughHandle(t, base); !reflect.DeepEqual(got, want) {
		t.Fatalf("derived handle read view = %+v, want the base handle's %+v", got, want)
	}

	// A write through the derived handle lands in the shared core, so the base
	// handle sees it without reopening anything.
	const thirdID = "repo/third.go::Third"
	derived.AddBatch([]*graph.Node{
		{ID: thirdID, Kind: graph.KindFunction, Name: "Third", FilePath: "repo/third.go", RepoPrefix: "repo"},
	}, nil)
	if n := base.GetNode(thirdID); n == nil {
		t.Fatal("a node written through the derived handle is invisible to the base handle")
	}
	if got, want := readThroughHandle(t, base), readThroughHandle(t, derived); !reflect.DeepEqual(got, want) {
		t.Fatalf("handles diverged after a shared-core write: base %+v, derived %+v", got, want)
	}

	// Deriving from a derived handle keeps the same core, so generations chain
	// without stacking wrappers.
	rederived := derived.AtGeneration(2)
	if rederived == nil || rederived.storeCore != base.storeCore {
		t.Fatal("re-deriving from a derived handle lost the shared core")
	}
	if got := rederived.ViewGeneration(); got != 2 {
		t.Fatalf("re-derived handle ViewGeneration = %d, want 2", got)
	}
}

// TestAtGenerationZeroMatchesBase covers the identity case: generation 0 is the
// base corpus, so pinning to it must be indistinguishable from the handle Open
// returned on every read.
func TestAtGenerationZeroMatchesBase(t *testing.T) {
	base := openSeededGenerationStore(t)
	t.Cleanup(func() { _ = base.Close() })

	atZero := base.AtGeneration(0)
	if atZero == nil {
		t.Fatal("AtGeneration(0) returned nil")
	}
	if got := atZero.ViewGeneration(); got != baseViewGeneration {
		t.Fatalf("AtGeneration(0).ViewGeneration = %d, want %d", got, baseViewGeneration)
	}
	if got, want := readThroughHandle(t, atZero), readThroughHandle(t, base); !reflect.DeepEqual(got, want) {
		t.Fatalf("generation-0 handle read view = %+v, want the base handle's %+v", got, want)
	}
}

// TestAtGenerationRejectsNegativeGeneration keeps a negative generation from
// silently degrading into a base-corpus read. Generations are minted from 1 and
// 0 is the base, so anything below 0 is a caller bug and gets nil.
func TestAtGenerationRejectsNegativeGeneration(t *testing.T) {
	base := openSeededGenerationStore(t)
	t.Cleanup(func() { _ = base.Close() })

	for _, g := range []int64{-1, -42, math.MinInt64} {
		if derived := base.AtGeneration(g); derived != nil {
			t.Fatalf("AtGeneration(%d) = %+v, want nil for a negative generation", g, derived)
		}
	}
	// Rejection must not disturb the receiver.
	if got := base.ViewGeneration(); got != baseViewGeneration {
		t.Fatalf("base handle ViewGeneration = %d after a rejected derive, want %d", got, baseViewGeneration)
	}
	if got := base.NodeCount(); got != 3 {
		t.Fatalf("base handle node count = %d after a rejected derive, want 3", got)
	}
}

// TestHandleWithoutCoreStaysInert guards the methods that promise to be safe on
// a receiver with nothing behind it. A zero Store value has no core, so every
// one of them would read through a nil pointer without the coreless() guard —
// the same handles used to be inert because the fields lived on Store itself.
func TestHandleWithoutCoreStaysInert(t *testing.T) {
	for name, s := range map[string]*Store{
		"nil handle": nil,
		"zero-value handle": func() *Store {
			var empty Store
			return &empty
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			if db, wal := s.DBStats(); db != 0 || wal != 0 {
				t.Fatalf("DBStats = (%d, %d), want (0, 0)", db, wal)
			}
			if err := s.ScanNodeSearchKeys(context.Background(), 16, func([]graph.NodeSearchKey) bool {
				t.Fatal("ScanNodeSearchKeys yielded a page from a handle with no core")
				return false
			}); err != nil {
				t.Fatalf("ScanNodeSearchKeys = %v, want nil", err)
			}
			byName, err := s.FindNodesByNameBounded(context.Background(), "Caller", graph.LocalizationNodeScope{}, 4)
			if err != nil || len(byName.Nodes) != 0 {
				t.Fatalf("FindNodesByNameBounded = (%+v, %v), want an empty projection and nil", byName, err)
			}
			byFile, err := s.FindFileNodesBounded(context.Background(), "repo/caller.go", graph.LocalizationNodeScope{}, 4)
			if err != nil || len(byFile.Nodes) != 0 {
				t.Fatalf("FindFileNodesBounded = (%+v, %v), want an empty projection and nil", byFile, err)
			}
			s.RecordStructuralIntegrityEvent(graph.StructuralIntegrityEvent{})
			if snap := s.StructuralIntegritySnapshot(graph.StructuralIntegritySnapshotOptions{}); !reflect.DeepEqual(snap, graph.StructuralIntegritySnapshot{}) {
				t.Fatalf("StructuralIntegritySnapshot = %+v, want the zero snapshot", snap)
			}
		})
	}
}

// checkpointLoopRunning reports whether the core's WAL-checkpoint goroutine is
// still alive. Its done channel closes only when the loop returns, which only
// Close triggers.
func checkpointLoopRunning(s *Store) bool {
	if s.checkpointDone == nil {
		return false
	}
	select {
	case <-s.checkpointDone:
		return false
	default:
		return true
	}
}

// TestCloseTearsDownOnlyThroughOwningHandle is the teardown contract. Pools,
// prepared statements and the checkpoint goroutine belong to the core, so a
// derived handle closing them would break every handle over the same database.
// Only the handle Open returned does the work; a derived Close is inert.
func TestCloseTearsDownOnlyThroughOwningHandle(t *testing.T) {
	base := openSeededGenerationStore(t)
	// The owning Close is part of the assertions below, so it cannot be a plain
	// cleanup; this only catches an early t.Fatal, which would otherwise leave
	// the checkpoint goroutine running.
	tornDown := false
	t.Cleanup(func() {
		if !tornDown {
			_ = base.Close()
		}
	})
	if !base.ownsCore {
		t.Fatal("the handle Open returned does not own its core")
	}
	if !checkpointLoopRunning(base) {
		t.Fatal("on-disk store did not start its checkpoint loop")
	}

	derived := base.AtGeneration(3)
	if derived == nil {
		t.Fatal("AtGeneration(3) returned nil")
	}
	if err := derived.Close(); err != nil {
		t.Fatalf("derived handle Close = %v, want nil", err)
	}
	// Closing the derived handle must have changed nothing.
	if !checkpointLoopRunning(base) {
		t.Fatal("derived Close stopped the core's checkpoint loop")
	}
	if err := base.writerDB.Ping(); err != nil {
		t.Fatalf("derived Close closed the writer pool: %v", err)
	}
	if got := base.NodeCount(); got != 3 {
		t.Fatalf("base node count after derived Close = %d, want 3", got)
	}
	if got := derived.NodeCount(); got != 3 {
		t.Fatalf("derived node count after its own Close = %d, want 3", got)
	}
	// Closing it again is still inert.
	if err := derived.Close(); err != nil {
		t.Fatalf("second derived handle Close = %v, want nil", err)
	}
	if err := base.writerDB.Ping(); err != nil {
		t.Fatalf("second derived Close closed the writer pool: %v", err)
	}

	// The owning handle does the real teardown.
	if err := base.Close(); err != nil {
		t.Fatalf("base handle Close = %v, want nil", err)
	}
	tornDown = true
	if checkpointLoopRunning(base) {
		t.Fatal("base Close left the checkpoint loop running")
	}
	if err := base.writerDB.Ping(); err == nil {
		t.Fatal("base Close left the writer pool open")
	}
	if err := base.db.Ping(); err == nil {
		t.Fatal("base Close left the read pool open")
	}

	// Teardown happens once: a derived handle closing after the fact must not
	// re-run it or report the already-closed pools as a failure.
	if err := derived.Close(); err != nil {
		t.Fatalf("derived Close after base teardown = %v, want nil", err)
	}
}
