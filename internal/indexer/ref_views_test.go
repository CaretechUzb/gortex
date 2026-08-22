package indexer

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/gitstate"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/reconcile"
)

// The ref-view fixture: one repository whose main branch is the base corpus,
// plus branches nobody has ever checked out.
//
// Everything under it is real. The repository is a real git repository, the
// corpus is a real index of its main branch, the catalog rows are written
// through the catalog's own API, and the builds are the production builder.
// What the tests drive is the manager's own decisions, so nothing between the
// selector and the adopted generation is stubbed.
//
// The concurrency tests are deterministic without a clock and therefore
// without synctest: what has to interleave is a build against another
// selection, and the manager's build barrier parks the first exactly where the
// second has to overtake it. A synctest bubble would virtualise time that no
// assertion here depends on, around real git subprocesses and a real SQLite
// writer that it cannot virtualise at all.

type refViewFixture struct {
	t *testing.T

	store   *store_sqlite.Store
	catalog *store_sqlite.Catalog

	// repo is the repository the corpus was indexed from. Its working tree
	// stays on main for the whole of every test: a ref view exists precisely
	// to serve state nobody has checked out.
	repo string

	familyID   string
	graphID    string
	checkoutID string
	// treeA is the committed tree main holds, and the tree the corpus holds.
	treeA string
}

func newRefViewFixture(t *testing.T) *refViewFixture {
	t.Helper()
	builderIsolateGit(t)

	repo := builderTempDir(t, "refview")
	builderGit(t, repo, "init", "--initial-branch=main")
	builderWriteTree(t, repo, builderTreeA())
	builderGit(t, repo, "add", "-A")
	builderGit(t, repo, "commit", "-m", "A")
	treeA := builderGit(t, repo, "rev-parse", "HEAD^{tree}")

	store := builderOpenStore(t, "refview")
	builderIndex(t, store, repo)

	f := &refViewFixture{
		t:          t,
		store:      store,
		catalog:    store.Catalog(),
		repo:       repo,
		familyID:   "family-refview",
		graphID:    GraphIDFor(builderRepoPrefix),
		checkoutID: "checkout-refview",
		treeA:      treeA,
	}
	f.writeCatalogIdentity()
	return f
}

// writeCatalogIdentity records what the reconciler would have recorded: the
// family, the checkout the corpus came from, and its dedicated graph.
func (f *refViewFixture) writeCatalogIdentity() {
	f.t.Helper()
	ctx := context.Background()
	now := time.Now().Unix()

	err := f.catalog.UpsertRepositoryFamily(ctx, store_sqlite.RepositoryFamily{
		FamilyID:          f.familyID,
		CommonDirIdentity: filepath.Join(f.repo, ".git"),
		State:             reconcile.FamilyStateReady,
		CreatedAt:         now,
		LastSeen:          now,
	})
	if err != nil {
		f.t.Fatalf("upsert family: %v", err)
	}

	err = f.catalog.AllocateCheckout(ctx, store_sqlite.Checkout{
		CheckoutID:     f.checkoutID,
		Incarnation:    "incarnation-refview",
		FamilyID:       f.familyID,
		RootPath:       f.repo,
		GitDir:         filepath.Join(f.repo, ".git"),
		AdminName:      "@main",
		State:          store_sqlite.CheckoutStateReady,
		DesiredMode:    store_sqlite.CheckoutModeDedicated,
		EffectiveMode:  store_sqlite.CheckoutModeDedicated,
		HeadRef:        "refs/heads/main",
		HeadTree:       f.treeA,
		LastAccessible: now,
		LastSeen:       now,
	})
	if err != nil {
		f.t.Fatalf("allocate the checkout: %v", err)
	}

	err = f.catalog.UpsertDedicatedGraph(ctx, store_sqlite.DedicatedGraph{
		GraphID:         f.graphID,
		OwnerCheckoutID: f.checkoutID,
		RepoPrefix:      builderRepoPrefix,
		FamilyID:        f.familyID,
		IsPrimaryBase:   true,
		State:           reconcile.GraphStateReady,
	})
	if err != nil {
		f.t.Fatalf("bind the dedicated graph: %v", err)
	}
}

// commitTree commits a tree on a scratch branch and leaves the working tree
// back on main, so the only trace of the commit is in the object store. Its
// commit and tree ids are what a ref is then pointed at.
func (f *refViewFixture) commitTree(tree map[string]string, message string) (commit, treeOID string) {
	f.t.Helper()
	builderGit(f.t, f.repo, "switch", "--force-create", "scratch", "main")
	builderWriteTree(f.t, f.repo, tree)
	builderGit(f.t, f.repo, "add", "-A")
	builderGit(f.t, f.repo, "commit", "-m", message)
	commit = builderGit(f.t, f.repo, "rev-parse", "HEAD^{commit}")
	treeOID = builderGit(f.t, f.repo, "rev-parse", "HEAD^{tree}")
	builderGit(f.t, f.repo, "switch", "--force", "main")
	return commit, treeOID
}

// recommit builds a new commit carrying the SAME tree as its parent — an
// amend, a rebase, an empty commit. It writes a commit object directly, so
// nothing is checked out and no ref moves.
func (f *refViewFixture) recommit(parent string) string {
	f.t.Helper()
	tree := builderGit(f.t, f.repo, "rev-parse", parent+"^{tree}")
	return builderGit(f.t, f.repo, "commit-tree", tree, "-p", parent, "-m", "same tree, new commit")
}

func (f *refViewFixture) setRef(ref, oid string) {
	f.t.Helper()
	builderGit(f.t, f.repo, "update-ref", ref, oid)
}

func (f *refViewFixture) manager(t *testing.T, barrier func()) *RefViewManager {
	t.Helper()
	manager, err := NewRefViewManager(RefViewManagerConfig{
		Store:        f.store,
		Builder:      builderNewBuilder(f.store),
		Config:       config.Default().Index,
		Logger:       zap.NewNop(),
		buildBarrier: barrier,
	})
	if err != nil {
		t.Fatalf("NewRefViewManager: %v", err)
	}
	return manager
}

func (f *refViewFixture) request(ref string) RefViewRequest {
	return RefViewRequest{
		GraphID:       f.graphID,
		SelectorKind:  gitstate.ViewSelectorGitRef,
		SelectorValue: ref,
		RepoDir:       f.repo,
		RepoPrefix:    builderRepoPrefix,
		WorkspaceID:   builderRepoPrefix,
		ProjectID:     builderRepoPrefix,
	}
}

func (f *refViewFixture) view(refViewID string) store_sqlite.RefView {
	f.t.Helper()
	view, found, err := f.catalog.GetRefView(context.Background(), refViewID)
	if err != nil || !found {
		f.t.Fatalf("read ref view %s: found=%v err=%v", refViewID, found, err)
	}
	return view
}

func (f *refViewFixture) builds(refViewID string) []store_sqlite.RefViewBuild {
	f.t.Helper()
	rows, err := f.catalog.ListRefViewBuilds(context.Background(), refViewID)
	if err != nil {
		f.t.Fatalf("list ref view builds: %v", err)
	}
	return rows
}

// generations enumerates every generation a ref view build produced, in any
// state.
func (f *refViewFixture) generations() []store_sqlite.ViewGeneration {
	f.t.Helper()
	rows, err := f.catalog.ListViewGenerations(context.Background(),
		store_sqlite.ViewGenerationFilter{OwnerKind: refViewOwnerKind})
	if err != nil {
		f.t.Fatalf("list view generations: %v", err)
	}
	return rows
}

func (f *refViewFixture) generation(id int64) store_sqlite.ViewGeneration {
	f.t.Helper()
	row, found, err := f.catalog.GetViewGeneration(context.Background(), id)
	if err != nil || !found {
		f.t.Fatalf("read generation %d: found=%v err=%v", id, found, err)
	}
	return row
}

// --- the ordinary path --------------------------------------------------

// TestRefViewBuildsOnceAndReusesThePayload is the base claim: selecting a
// branch nobody has checked out builds a generation for its tree, and
// selecting it again while nothing moved builds nothing.
func TestRefViewBuildsOnceAndReusesThePayload(t *testing.T) {
	f := newRefViewFixture(t)
	commitB, treeB := f.commitTree(builderTreeB(), "B")
	f.setRef("refs/heads/feature", commitB)

	var builds atomic.Int64
	manager := f.manager(t, func() { builds.Add(1) })
	ctx := context.Background()

	first, err := manager.EnsureRefView(ctx, f.request("refs/heads/feature"))
	if err != nil {
		t.Fatalf("first selection: %v", err)
	}
	if first.State != store_sqlite.RefViewReady || !first.Built || first.GenerationID == 0 {
		t.Fatalf("first selection = %+v, want a ready view built by this call", first)
	}
	if first.Resolved.CommitOID != commitB || first.Resolved.TreeOID != treeB {
		t.Fatalf("first selection resolved %+v, want commit %s tree %s", first.Resolved, commitB, treeB)
	}
	if row := f.generation(first.GenerationID); row.TreeOID != treeB || row.State != store_sqlite.ViewGenerationReady {
		t.Fatalf("generation = %+v, want a ready generation at tree %s", row, treeB)
	}

	view := f.view(first.RefViewID)
	if view.ActiveGenerationID != first.GenerationID || view.ActiveCommit != commitB || view.ActiveTree != treeB {
		t.Fatalf("ref view = %+v, want it serving generation %d at %s", view, first.GenerationID, commitB)
	}
	if view.State != store_sqlite.RefViewReady || !view.ExactView {
		t.Fatalf("ref view state = %q exact=%v", view.State, view.ExactView)
	}

	second, err := manager.EnsureRefView(ctx, f.request("refs/heads/feature"))
	if err != nil {
		t.Fatalf("second selection: %v", err)
	}
	if second.Built || second.GenerationID != first.GenerationID || second.State != store_sqlite.RefViewReady {
		t.Fatalf("second selection = %+v, want the first generation served without a build", second)
	}
	if n := builds.Load(); n != 1 {
		t.Fatalf("%d build passes ran for two selections of an unmoved branch", n)
	}
}

// TestRefViewSelectionIsWhatNoticesMovement pins the cost model: a ref that
// moves while nobody is asking costs nothing, and the rebuild happens on the
// next selection rather than on the movement.
func TestRefViewSelectionIsWhatNoticesMovement(t *testing.T) {
	f := newRefViewFixture(t)
	commitB, _ := f.commitTree(builderTreeB(), "B")
	f.setRef("refs/heads/feature", commitB)

	var builds atomic.Int64
	manager := f.manager(t, func() { builds.Add(1) })
	ctx := context.Background()

	first, err := manager.EnsureRefView(ctx, f.request("refs/heads/feature"))
	if err != nil {
		t.Fatalf("first selection: %v", err)
	}

	// The branch moves to a different tree. Nothing selects it, and nothing
	// watches it, so nothing may happen.
	moved := builderTreeB()
	moved["late.go"] = "package fixture\n\nfunc Late() {\n}\n"
	commitC, treeC := f.commitTree(moved, "C")
	f.setRef("refs/heads/feature", commitC)

	if n := builds.Load(); n != 1 {
		t.Fatalf("%d build passes ran, want only the first selection's", n)
	}
	idle := f.view(first.RefViewID)
	if idle.ActiveGenerationID != first.GenerationID || idle.ActiveCommit != commitB {
		t.Fatalf("ref view moved without a selection: %+v", idle)
	}

	second, err := manager.EnsureRefView(ctx, f.request("refs/heads/feature"))
	if err != nil {
		t.Fatalf("second selection: %v", err)
	}
	if !second.Built || second.GenerationID == first.GenerationID {
		t.Fatalf("second selection = %+v, want a rebuild off generation %d", second, first.GenerationID)
	}
	if n := builds.Load(); n != 2 {
		t.Fatalf("%d build passes ran, want one per noticed movement", n)
	}
	view := f.view(first.RefViewID)
	if view.ActiveGenerationID != second.GenerationID || view.ActiveCommit != commitC || view.ActiveTree != treeC {
		t.Fatalf("ref view = %+v, want it serving generation %d at %s", view, second.GenerationID, commitC)
	}
}

// --- coalescing ---------------------------------------------------------

// TestRefViewCoalescesConcurrentSelections is the claim the partial unique
// index exists for: two selections of one view produce one build row and one
// generation, and the one that lost is handed the winner's build token rather
// than a bare failure.
//
// The interleaving is exact rather than raced: the winner is parked in its
// build barrier — after its pass, before it may publish — and the second
// selection runs while it is parked, which is the only window in which a
// second claim is possible at all.
func TestRefViewCoalescesConcurrentSelections(t *testing.T) {
	f := newRefViewFixture(t)
	commitB, _ := f.commitTree(builderTreeB(), "B")
	f.setRef("refs/heads/feature", commitB)

	// Only the first pass parks. A second one would mean coalescing failed,
	// and it has to be free to finish so the assertions below can say so
	// instead of the test deadlocking on its own barrier.
	var builds atomic.Int64
	parked := make(chan struct{})
	release := make(chan struct{})
	manager := f.manager(t, func() {
		if builds.Add(1) == 1 {
			close(parked)
			<-release
		}
	})
	ctx := context.Background()

	var (
		wg     sync.WaitGroup
		winner RefViewResult
		winErr error
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		winner, winErr = manager.EnsureRefView(ctx, f.request("refs/heads/feature"))
	}()

	<-parked
	loser, err := manager.EnsureRefView(ctx, f.request("refs/heads/feature"))
	if err != nil {
		t.Fatalf("second selection: %v", err)
	}
	close(release)
	wg.Wait()

	if winErr != nil {
		t.Fatalf("first selection: %v", winErr)
	}
	if winner.State != store_sqlite.RefViewReady || !winner.Built {
		t.Fatalf("first selection = %+v, want a ready view it built itself", winner)
	}
	if loser.State != store_sqlite.RefViewBuilding || loser.Built {
		t.Fatalf("second selection = %+v, want a building answer with no build of its own", loser)
	}
	if n := builds.Load(); n != 1 {
		t.Fatalf("%d build passes ran for two concurrent selections of one view", n)
	}

	rows := f.builds(winner.RefViewID)
	if len(rows) != 1 {
		t.Fatalf("%d build rows, want the two selections to have shared one: %+v", len(rows), rows)
	}
	if loser.BuildToken != rows[0].BuildToken {
		t.Fatalf("second selection got token %q, want the in-flight build's %q", loser.BuildToken, rows[0].BuildToken)
	}
	if rows[0].State != store_sqlite.ViewGenerationReady || rows[0].GenerationID != winner.GenerationID {
		t.Fatalf("build row = %+v, want it finished on generation %d", rows[0], winner.GenerationID)
	}
	if generations := f.generations(); len(generations) != 1 {
		t.Fatalf("%d ref-view generations, want one: %+v", len(generations), generations)
	}
}

// TestRefViewCanceledRequestDoesNotWedgeTheView pins what a claim must never
// outlive: the request that made it. A client that gives up mid-build leaves
// the attempt behind, and the coalescing index treats any attempt still in the
// building state as the live claim — so an attempt the canceled request failed
// to close would hand every later selection of that tree a build nobody is
// running, and a commit or tag selector's tree never moves to break the tie.
func TestRefViewCanceledRequestDoesNotWedgeTheView(t *testing.T) {
	f := newRefViewFixture(t)
	commitB, treeB := f.commitTree(builderTreeB(), "B")
	f.setRef("refs/heads/feature", commitB)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var builds atomic.Int64
	manager := f.manager(t, func() {
		if builds.Add(1) == 1 {
			cancel()
		}
	})

	abandoned, err := manager.EnsureRefView(ctx, f.request("refs/heads/feature"))
	if err == nil {
		t.Fatal("a selection whose request was canceled mid-build succeeded")
	}
	rows := f.builds(abandoned.RefViewID)
	if len(rows) != 1 {
		t.Fatalf("%d build rows, want the canceled selection's one: %+v", len(rows), rows)
	}
	if rows[0].State == store_sqlite.ViewGenerationBuilding {
		t.Fatalf("the canceled request left its attempt claimed: %+v", rows[0])
	}
	dead := rows[0].BuildToken

	// The retry is a fresh request, and it must be able to build: nothing is
	// running the attempt the cancellation left behind.
	retry, err := manager.EnsureRefView(context.Background(), f.request("refs/heads/feature"))
	if err != nil {
		t.Fatalf("retry after a canceled selection: %v", err)
	}
	if retry.State != store_sqlite.RefViewReady || !retry.Built {
		t.Fatalf("retry = %+v, want a view it built itself", retry)
	}
	if retry.BuildToken == dead {
		t.Fatalf("the retry was handed the canceled attempt's token %q", dead)
	}
	if retry.Resolved.TreeOID != treeB {
		t.Fatalf("retry resolved %+v, want tree %s", retry.Resolved, treeB)
	}
	if view := f.view(retry.RefViewID); view.ActiveGenerationID != retry.GenerationID {
		t.Fatalf("ref view = %+v, want it serving generation %d", view, retry.GenerationID)
	}
}

// TestRefViewReclaimsAnAbandonedClaim is the same wedge from the other side: a
// daemon killed mid-build leaves a claim no completion will ever close, and no
// janitor touches the build rows. The liveness window is what breaks it — a
// claim that stopped reporting progress is taken over rather than waited on.
func TestRefViewReclaimsAnAbandonedClaim(t *testing.T) {
	f := newRefViewFixture(t)
	commitB, treeB := f.commitTree(builderTreeB(), "B")
	f.setRef("refs/heads/feature", commitB)

	manager := f.manager(t, nil)
	ctx := context.Background()
	req := f.request("refs/heads/feature")
	req.EnrichmentProfile = defaultEnrichmentProfile
	viewID := refViewID(req)

	// The rows a worker that died holding the claim leaves behind: the view it
	// created, and its own attempt, with no progress since.
	view, err := f.catalog.GetOrCreateRefView(ctx, store_sqlite.RefView{
		RefViewID:         viewID,
		GraphID:           req.GraphID,
		SelectorKind:      string(req.SelectorKind),
		SelectorValue:     req.SelectorValue,
		EnrichmentProfile: req.EnrichmentProfile,
		State:             store_sqlite.RefViewPending,
		ExactView:         true,
	})
	if err != nil {
		t.Fatalf("seed the ref view: %v", err)
	}
	base, err := manager.base(ctx, req.GraphID)
	if err != nil {
		t.Fatalf("read the base: %v", err)
	}
	stale := time.Now().Add(-2 * refViewBuildLiveness).Unix()
	err = f.catalog.UpsertRefViewBuild(ctx, store_sqlite.RefViewBuild{
		BuildID:            "build-abandoned",
		RefViewID:          viewID,
		DesiredRef:         req.SelectorValue,
		DesiredCommit:      commitB,
		DesiredTree:        treeB,
		BaseGenerationID:   base.generationID,
		EnrichmentProfile:  req.EnrichmentProfile,
		BuildFingerprint:   refViewBuildFingerprint(manager.identity(viewID, base, treeB), req.EnrichmentProfile),
		CapturedRouteEpoch: view.RouteEpoch,
		State:              store_sqlite.ViewGenerationBuilding,
		BuildToken:         "token-abandoned",
		CreatedAt:          stale,
		LastProgress:       stale,
	})
	if err != nil {
		t.Fatalf("seed the abandoned attempt: %v", err)
	}

	result, err := manager.EnsureRefView(ctx, f.request("refs/heads/feature"))
	if err != nil {
		t.Fatalf("selection over an abandoned claim: %v", err)
	}
	if result.State != store_sqlite.RefViewReady || !result.Built {
		t.Fatalf("selection = %+v, want it to have built the view itself", result)
	}
	if result.BuildToken == "token-abandoned" {
		t.Fatal("the selection was handed the abandoned claim's token")
	}

	rows := f.builds(viewID)
	if len(rows) != 2 {
		t.Fatalf("%d build rows, want the abandoned attempt and its successor: %+v", len(rows), rows)
	}
	for _, row := range rows {
		switch row.BuildID {
		case "build-abandoned":
			if row.State != store_sqlite.ViewGenerationFailed || row.Error == "" {
				t.Errorf("the abandoned attempt = %+v, want it failed with a recorded cause", row)
			}
		default:
			if row.State != store_sqlite.ViewGenerationReady || row.GenerationID != result.GenerationID {
				t.Errorf("the successor = %+v, want it finished on generation %d", row, result.GenerationID)
			}
		}
	}
}

// --- publish-time revalidation ------------------------------------------

// TestRefViewSupersedesABuildWhoseTreeMoved pins the half of the revalidation
// that refuses: a branch that moved to a different tree while the build ran
// describes a state the view has left, so the finished generation is
// superseded and the view's active pointer does not move.
func TestRefViewSupersedesABuildWhoseTreeMoved(t *testing.T) {
	f := newRefViewFixture(t)
	commitB, _ := f.commitTree(builderTreeB(), "B")
	moved := builderTreeB()
	moved["late.go"] = "package fixture\n\nfunc Late() {\n}\n"
	commitC, treeC := f.commitTree(moved, "C")
	f.setRef("refs/heads/feature", commitB)

	var builds atomic.Int64
	manager := f.manager(t, func() {
		if builds.Add(1) == 1 {
			f.setRef("refs/heads/feature", commitC)
		}
	})
	ctx := context.Background()

	result, err := manager.EnsureRefView(ctx, f.request("refs/heads/feature"))
	if err != nil {
		t.Fatalf("selection: %v", err)
	}
	if result.State != store_sqlite.RefViewBuilding || !result.Built {
		t.Fatalf("selection = %+v, want a build that ran and could not be adopted", result)
	}
	if result.GenerationID != 0 {
		t.Fatalf("selection named generation %d, want none — nothing was adopted", result.GenerationID)
	}
	if result.Resolved.TreeOID != treeC {
		t.Fatalf("selection resolved %+v, want the tree the branch moved to (%s)", result.Resolved, treeC)
	}

	view := f.view(result.RefViewID)
	if view.ActiveGenerationID != 0 || view.ActiveCommit != "" {
		t.Fatalf("ref view flipped to a superseded build: %+v", view)
	}
	generations := f.generations()
	if len(generations) != 1 || generations[0].State != store_sqlite.ViewGenerationSuperseded {
		t.Fatalf("generations = %+v, want the one build's generation superseded", generations)
	}
	rows := f.builds(view.RefViewID)
	if len(rows) != 1 || rows[0].State != store_sqlite.ViewGenerationSuperseded {
		t.Fatalf("build rows = %+v, want the attempt recorded as superseded", rows)
	}
}

// TestRefViewAdoptsANewCommitOnTheSameTree pins the half that adopts: a branch
// that moved to a different COMMIT carrying the same tree describes exactly
// the payload that was just built, so the generation is adopted and the new
// commit is stamped beside it.
//
// The build counter is the load-bearing assertion. A payload is a function of
// the tree, so noticing the commit must not cost a second pass — and the
// counter is what says it did not.
func TestRefViewAdoptsANewCommitOnTheSameTree(t *testing.T) {
	f := newRefViewFixture(t)
	commitB, treeB := f.commitTree(builderTreeB(), "B")
	sameTree := f.recommit(commitB)
	f.setRef("refs/heads/feature", commitB)

	var builds atomic.Int64
	manager := f.manager(t, func() {
		if builds.Add(1) == 1 {
			f.setRef("refs/heads/feature", sameTree)
		}
	})
	ctx := context.Background()

	result, err := manager.EnsureRefView(ctx, f.request("refs/heads/feature"))
	if err != nil {
		t.Fatalf("selection: %v", err)
	}
	if result.State != store_sqlite.RefViewReady || result.GenerationID == 0 {
		t.Fatalf("selection = %+v, want the built generation adopted", result)
	}
	if n := builds.Load(); n != 1 {
		t.Fatalf("%d build passes ran, want one — the tree never changed", n)
	}
	if result.Resolved.CommitOID != sameTree || result.Resolved.TreeOID != treeB {
		t.Fatalf("selection resolved %+v, want commit %s on tree %s", result.Resolved, sameTree, treeB)
	}

	view := f.view(result.RefViewID)
	if view.ActiveGenerationID != result.GenerationID {
		t.Fatalf("ref view = %+v, want it serving generation %d", view, result.GenerationID)
	}
	if view.ActiveCommit != sameTree || view.ActiveTree != treeB {
		t.Fatalf("ref view = %+v, want the commit the branch moved to (%s) on tree %s", view, sameTree, treeB)
	}
	if row := f.generation(result.GenerationID); row.State != store_sqlite.ViewGenerationReady {
		t.Fatalf("generation = %+v, want it ready", row)
	}

	// The next selection has nothing left to do: the payload matches the tree
	// and the metadata already names the commit.
	next, err := manager.EnsureRefView(ctx, f.request("refs/heads/feature"))
	if err != nil {
		t.Fatalf("follow-up selection: %v", err)
	}
	if next.Built || next.GenerationID != result.GenerationID {
		t.Fatalf("follow-up selection = %+v, want the adopted generation served as it stands", next)
	}
}

// --- resolution failures ------------------------------------------------

// TestRefViewRecordsAnUnresolvableSelector pins the failure path: a selector
// that names nothing local fails the selection with the typed availability
// error and leaves the view's active pointer alone.
func TestRefViewRecordsAnUnresolvableSelector(t *testing.T) {
	f := newRefViewFixture(t)
	manager := f.manager(t, nil)
	ctx := context.Background()

	result, err := manager.EnsureRefView(ctx, f.request("refs/heads/never-created"))
	if err == nil {
		t.Fatal("selecting an absent branch succeeded")
	}
	if !errors.Is(err, gitstate.ErrRefNotAvailableLocally) {
		t.Fatalf("selection error = %v, want a local-availability failure", err)
	}
	if result.State != store_sqlite.RefViewFailed {
		t.Fatalf("selection = %+v, want a failed view", result)
	}

	view := f.view(result.RefViewID)
	if view.State != store_sqlite.RefViewFailed || view.LastError == "" {
		t.Fatalf("ref view = %+v, want the failure recorded", view)
	}
	if view.ActiveGenerationID != 0 {
		t.Fatalf("a failed selection flipped the active pointer: %+v", view)
	}
	if generations := f.generations(); len(generations) != 0 {
		t.Fatalf("a failed selection built %d generations: %+v", len(generations), generations)
	}
}
