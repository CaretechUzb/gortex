package indexer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
)

func newCheckoutMutationFixture(t testing.TB) (*coordinatorFixture, *CheckoutCoordinator, *CheckoutLifecycle) {
	t.Helper()
	f := newCoordinatorFixture(t)
	gate := NewViewBuildGate()
	gate.Open()
	c := f.coordinator(t, CheckoutCoordinatorConfig{Gate: gate, Debounce: time.Hour})
	if out := c.reconcile(context.Background()); out.Err != nil || out.DirtyGenerationID == 0 {
		t.Fatalf("initial reconcile: %+v", out)
	}
	l := &CheckoutLifecycle{
		catalog:      f.catalog,
		store:        f.store,
		coordinators: map[string]*CheckoutCoordinator{f.checkoutID: c},
	}
	return f, c, l
}

func TestCheckoutMutationDryRunLeavesRouteUntouched(t *testing.T) {
	f, _, l := newCheckoutMutationFixture(t)
	before := f.route()
	m, err := l.BeginCheckoutMutation(t.Context(), f.checkoutID, f.worktree, before.RouteEpoch)
	if err != nil {
		t.Fatal(err)
	}
	m.Close()
	m.Close()
	if after := f.route(); after != before {
		t.Fatalf("dry run changed route: before=%+v after=%+v", before, after)
	}
}

func TestCheckoutMutationPublishesOnlyDirtyLayerAndPreservesPinnedView(t *testing.T) {
	f, _, l := newCheckoutMutationFixture(t)
	before := f.route()
	primaryBefore, err := os.ReadFile(filepath.Join(f.primary, "helper.go"))
	if err != nil {
		t.Fatal(err)
	}
	materializer := &graphview.Materializer{Store: f.store, Catalog: f.catalog, Leases: f.leases}
	pinned, err := materializer.MaterializeCheckout(t.Context(), f.checkoutID)
	if err != nil {
		t.Fatal(err)
	}
	defer pinned.Close()
	m, err := l.BeginCheckoutMutation(t.Context(), f.checkoutID, f.worktree, before.RouteEpoch)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if err := m.Prepare(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := m.Prepare(t.Context()); err != nil {
		t.Fatalf("prepare must be idempotent before refresh: %v", err)
	}
	if route := f.route(); route.State != store_sqlite.RoutePending || route.DirtyGenerationID != 0 || route.CommitGenerationID != before.CommitGenerationID {
		t.Fatalf("old exact route remained visible while source is changing: %+v", route)
	}
	if _, found := f.generation(before.DirtyGenerationID); !found {
		t.Fatal("prepare collected an old reader's pinned dirty generation")
	}
	builderWriteFile(t, f.worktree, "helper.go", "package fixture\n\nfunc EditedHelper() {}\n")
	out, err := m.Refresh(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if out.CommitGenerationID != before.CommitGenerationID || out.DirtyGenerationID == before.DirtyGenerationID || !out.DirtyBuilt {
		t.Fatalf("unexpected mutation rebuild: %+v", out)
	}
	if route := f.route(); route.State != store_sqlite.RouteActive || route.DirtyGenerationID != out.DirtyGenerationID {
		t.Fatalf("successful refresh did not leave exact route: %+v", route)
	}
	primaryAfter, err := os.ReadFile(filepath.Join(f.primary, "helper.go"))
	if err != nil || string(primaryAfter) != string(primaryBefore) {
		t.Fatalf("source mutation changed primary: %v", err)
	}
	if err := m.Prepare(t.Context()); !errors.Is(err, ErrCheckoutMutationStale) {
		t.Fatalf("fresh lease admitted a second unguarded disk commit: %v", err)
	}
}

func TestCheckoutMutationRejectsWrongRootEpochAndExternalChanges(t *testing.T) {
	f, _, l := newCheckoutMutationFixture(t)
	before := f.route()
	for _, tc := range []struct {
		root  string
		epoch int64
	}{
		{f.primary, before.RouteEpoch},
		{f.worktree, before.RouteEpoch + 1},
		{f.worktree, 0},
	} {
		if m, err := l.BeginCheckoutMutation(t.Context(), f.checkoutID, tc.root, tc.epoch); !errors.Is(err, ErrCheckoutMutationStale) {
			if m != nil {
				m.Close()
			}
			t.Fatalf("root=%s epoch=%d: %v", tc.root, tc.epoch, err)
		}
	}
	m, err := l.BeginCheckoutMutation(t.Context(), f.checkoutID, f.worktree, before.RouteEpoch)
	if err != nil {
		t.Fatal(err)
	}
	builderWriteFile(t, f.worktree, "helper.go", "package fixture\n\nfunc ExternalEdit() {}\n")
	if err := m.Prepare(t.Context()); !errors.Is(err, ErrCheckoutMutationStale) {
		t.Fatalf("external edit between begin and prepare was not refused: %v", err)
	}
	m.Close()
	if m, err := l.BeginCheckoutMutation(t.Context(), f.checkoutID, f.worktree, before.RouteEpoch); !errors.Is(err, ErrCheckoutMutationStale) {
		if m != nil {
			m.Close()
		}
		t.Fatalf("stale disk at admission was not refused: %v", err)
	}
	if f.route() != before {
		t.Fatal("rejected mutations changed the catalog route")
	}
}

func TestCheckoutMutationCanceledRefreshLeavesPendingAndSignalsRetry(t *testing.T) {
	f, c, l := newCheckoutMutationFixture(t)
	m, err := l.BeginCheckoutMutation(t.Context(), f.checkoutID, f.worktree, f.route().RouteEpoch)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Prepare(t.Context()); err != nil {
		t.Fatal(err)
	}
	builderWriteFile(t, f.worktree, "helper.go", "package fixture\n\nfunc PendingEdit() {}\n")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := m.Refresh(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("refresh: %v", err)
	}
	m.Close()
	if route := f.route(); route.State != store_sqlite.RoutePending {
		t.Fatalf("failed refresh became active: %+v", route)
	}
	c.mu.Lock()
	reason := c.reason
	c.mu.Unlock()
	if reason != "source mutation needs a dirty generation refresh" {
		t.Fatalf("missing retry signal: %q", reason)
	}
}

func TestCheckoutMutationRejectsReplacedRoot(t *testing.T) {
	f, _, l := newCheckoutMutationFixture(t)
	before := f.route()
	m, err := l.BeginCheckoutMutation(t.Context(), f.checkoutID, f.worktree, before.RouteEpoch)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	oldRoot := f.worktree + "-original"
	if err := os.Rename(f.worktree, oldRoot); err != nil {
		t.Fatal(err)
	}
	defer func() {
		// Keep the replacement under this fixture's temporary directory so
		// standard test cleanup owns its removal.
		_ = os.Rename(f.worktree, f.worktree+"-replacement")
		if err := os.Rename(oldRoot, f.worktree); err != nil {
			t.Errorf("restore fixture root: %v", err)
		}
	}()
	if err := os.CopyFS(f.worktree, os.DirFS(oldRoot)); err != nil {
		t.Fatal(err)
	}
	// This replacement precedes lifecycle discovery: catalog id/incarnation
	// and route epoch are unchanged, as are Git and source contents. The pinned
	// filesystem identity must refuse before a content comparison can accept it.
	if err := m.Prepare(t.Context()); !errors.Is(err, ErrCheckoutMutationStale) || !strings.Contains(err.Error(), "root was replaced") {
		t.Fatalf("replaced root admitted a source write: %v", err)
	}
	if f.route() != before {
		t.Fatal("root replacement refusal invalidated the original route")
	}
}

func TestCheckoutMutationCloseJoinsLeaseAndCancelsWaitingAdmission(t *testing.T) {
	f, c, l := newCheckoutMutationFixture(t)
	m, err := l.BeginCheckoutMutation(t.Context(), f.checkoutID, f.worktree, f.route().RouteEpoch)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Millisecond)
	defer cancel()
	if err := c.CloseContext(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shutdown did not wait for active writer: %v", err)
	}
	if err := m.Prepare(t.Context()); !errors.Is(err, context.Canceled) {
		t.Fatalf("closing coordinator admitted a write: %v", err)
	}
	m.Close()
	if err := c.CloseContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	if lease, err := l.BeginCheckoutMutation(t.Context(), f.checkoutID, f.worktree, f.route().RouteEpoch); err == nil {
		lease.Close()
		t.Fatal("closed coordinator admitted new source mutation")
	}
}

func TestCheckoutMutationAdmissionCancellationReleasesResources(t *testing.T) {
	f, c, l := newCheckoutMutationFixture(t)
	block, err := c.gate.Acquire(t.Context(), ViewBuildBackground)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Millisecond)
	defer cancel()
	if m, err := l.BeginCheckoutMutation(ctx, f.checkoutID, f.worktree, f.route().RouteEpoch); !errors.Is(err, context.DeadlineExceeded) {
		if m != nil {
			m.Close()
		}
		t.Fatalf("admission was not canceled: %v", err)
	}
	block()
	c.mu.Lock()
	active := c.sourceMutations
	c.mu.Unlock()
	if active != 0 {
		t.Fatalf("canceled admission leaked %d leases", active)
	}
	m, err := l.BeginCheckoutMutation(t.Context(), f.checkoutID, f.worktree, f.route().RouteEpoch)
	if err != nil {
		t.Fatal(err)
	}
	m.Close()
}

func TestCheckoutMutationAdmissionPanicReleasesResources(t *testing.T) {
	f, c, l := newCheckoutMutationFixture(t)
	// A backend panic recovered by MCP must not strand the shared build gate
	// or the checkout route lock. A missing coordinator catalog injects failure after
	// both locks have been acquired, without changing the production path.
	catalog := c.catalog
	c.catalog = nil
	var panicValue any
	func() {
		defer func() { panicValue = recover() }()
		m, _ := l.BeginCheckoutMutation(t.Context(), f.checkoutID, f.worktree, f.route().RouteEpoch)
		if m != nil {
			m.Close()
		}
	}()
	c.catalog = catalog
	if panicValue == nil {
		t.Fatal("fault injection did not panic")
	}
	if !c.cycleMu.TryLock() {
		t.Fatal("panic stranded the checkout route lock")
	}
	c.cycleMu.Unlock()
	if stats := c.gate.Stats(); stats.Active {
		t.Fatal("panic stranded the shared build gate")
	}
	c.mu.Lock()
	active := c.sourceMutations
	c.mu.Unlock()
	if active != 0 {
		t.Fatalf("panic leaked %d admissions", active)
	}
}

func BenchmarkCheckoutMutationDryRunAdmission(b *testing.B) {
	f, _, l := newCheckoutMutationFixture(b)
	epoch := f.route().RouteEpoch
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		m, err := l.BeginCheckoutMutation(b.Context(), f.checkoutID, f.worktree, epoch)
		if err != nil {
			b.Fatal(err)
		}
		m.Close()
	}
}

// TestCheckoutMutationDemandsAnUnbuiltCheckout pins the write side of the lazy
// first build.
//
// An automatic checkout's first layer is built on demand. Reads say "I am
// reading this" through ActivateCheckout; the write path never did, so the
// first edit through an exact worktree view read a route with no
// commit/dirty generation, was told "the route changed, retry", and every
// retry found the same thing — nothing in the loop was ever going to build the
// layer the edit was waiting for.
//
// What is asserted is the demand, not a successful edit: the build is
// asynchronous and BeginCheckoutMutation holds the build gate and the cycle
// lock while it reads the route, so this call cannot be the one that succeeds.
// It has to be the one that makes the retry able to.
func TestCheckoutMutationDemandsAnUnbuiltCheckout(t *testing.T) {
	f := newCoordinatorFixture(t)
	f.deferFirstBuild = true
	gate := NewViewBuildGate()
	gate.Open()
	// An hour of debounce keeps the running loop out of the assertions: the
	// Demand's wake arms a quiet window this test will never reach, so the only
	// cycle that runs is the one called below. cycles is still guarded — the
	// loop owns the callback, and "it cannot fire" is exactly the kind of
	// assumption the race detector is here to check.
	var cyclesMu sync.Mutex
	var cycles []CheckoutCycle
	c := f.coordinator(t, CheckoutCoordinatorConfig{
		Gate: gate, Debounce: time.Hour,
		cycleDone: func(out CheckoutCycle) {
			cyclesMu.Lock()
			defer cyclesMu.Unlock()
			cycles = append(cycles, out)
		},
	})
	l := &CheckoutLifecycle{
		catalog:      f.catalog,
		store:        f.store,
		coordinators: map[string]*CheckoutCoordinator{f.checkoutID: c},
	}

	if _, routed, err := f.catalog.GetCheckoutRoute(t.Context(), f.checkoutID); err != nil || routed {
		t.Fatalf("fixture is not in the unbuilt state this test is about: routed=%v err=%v", routed, err)
	}
	c.mu.Lock()
	demandedBefore := c.demanded
	c.mu.Unlock()
	if demandedBefore {
		t.Fatal("fixture arrived already demanded; the test would prove nothing")
	}

	m, err := l.BeginCheckoutMutation(t.Context(), f.checkoutID, f.worktree, 1)
	if m != nil {
		m.Close()
		t.Fatal("an unbuilt checkout admitted a source mutation")
	}
	if !errors.Is(err, ErrCheckoutMutationStale) {
		t.Fatalf("BeginCheckoutMutation on an unbuilt checkout: %v, want ErrCheckoutMutationStale", err)
	}
	if !strings.Contains(err.Error(), "not built yet") {
		t.Errorf("error %q does not tell the caller the build was requested", err)
	}

	c.mu.Lock()
	demandedAfter := c.demanded
	c.mu.Unlock()
	if !demandedAfter {
		t.Fatal("the refused edit did not demand the build; every retry would refuse identically")
	}

	// The demand is what makes the retry converge: the next cycle now builds
	// instead of deferring.
	c.cycle(t.Context())
	cyclesMu.Lock()
	defer cyclesMu.Unlock()
	if len(cycles) == 0 {
		t.Fatal("the cycle after the edit's demand did not report")
	}
	out := cycles[len(cycles)-1]
	if out.Deferred || !out.CommitBuilt || out.CommitGenerationID == 0 {
		t.Fatalf("the cycle after the edit's demand still deferred the first build: %+v", out)
	}
}
