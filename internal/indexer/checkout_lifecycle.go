package indexer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"uuid"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/gitstate"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/pathkey"
	"github.com/zzet/gortex/internal/reconcile"
)

// Tracking-intent sources, re-exported so an entry point does not have to
// import the catalog package just to say who asked.
const (
	// TrackSourceCLI is an explicit `gortex track` / daemon control call.
	TrackSourceCLI = store_sqlite.IntentSourceCLITrack
	// TrackSourceMCP is an explicit track_repository tool call.
	TrackSourceMCP = store_sqlite.IntentSourceMCPTrack
	// TrackSourceConfig is a repository named in the global configuration.
	TrackSourceConfig = store_sqlite.IntentSourceManualConfig
	// TrackSourceImplicit records a checkout observed without anyone asking
	// for it — the auto-index path. It is deliberately not an intent kind:
	// the constant exists so a caller can name the case, and the lifecycle
	// writes no tracking intent for it.
	TrackSourceImplicit store_sqlite.IntentSourceKind = ""
)

// ErrCheckoutNotTracked reports a lifecycle operation aimed at a path or
// prefix that names nothing this daemon tracks.
var ErrCheckoutNotTracked = errors.New("indexer: no tracked repository matches")

// LifecycleNotifier is what has to be told that the tracked-repository set
// changed. The MCP server implements it; a daemon without one still keeps
// its catalog, config and watcher coherent.
type LifecycleNotifier interface {
	// InvalidateSessionScopes drops cached per-session workspace bindings.
	InvalidateSessionScopes()
	// RunAnalysis recomputes the graph-wide rollups.
	RunAnalysis()
}

// RepoWatcher is the part of the live file watcher the lifecycle drives.
// *MultiWatcher implements it.
type RepoWatcher interface {
	AddRepo(repoPrefix string, cfg config.WatchConfig) error
	RemoveRepo(repoPrefix string) error
}

// CheckoutLifecycleConfig is what the lifecycle needs to own its side effects.
type CheckoutLifecycleConfig struct {
	// MultiIndexer is the corpus the lifecycle indexes into and evicts from.
	MultiIndexer *MultiIndexer
	// ConfigManager persists the tracked-repository list.
	ConfigManager *config.ConfigManager
	// Graph is the store; the lifecycle uses its catalog when it has one.
	Graph  graph.Store
	Logger *zap.Logger
	// Reconcile carries the two grace windows. A zero value takes the
	// shipped defaults.
	Reconcile reconcile.Config
	// Clock overrides the lifecycle's and the reconciler's clock.
	Clock func() time.Time
}

// CheckoutLifecycle is the single owner of checkout lifecycle side effects.
//
// Every entry point that tracks, forgets, reloads or sweeps a checkout goes
// through it, so identity (which family and incarnation a path is), intent
// (who asked for it), clocks (how long an outage has run) and cleanup (what
// is detached, evicted and persisted, in what order) are decided once
// instead of once per surface.
//
// Full indexing is unchanged: a tracked repository still indexes into the
// base corpus exactly as before. What the lifecycle adds around it is the
// catalog identity and the ordering of the side effects.
//
// A store with no catalog — or a repository git does not administer — still
// works: the catalog steps are skipped and the real side effects (index,
// watcher, config, invalidation) happen exactly as they did before.
type CheckoutLifecycle struct {
	mi      *MultiIndexer
	cfgMgr  *config.ConfigManager
	catalog *store_sqlite.Catalog
	rec     *reconcile.Reconciler
	logger  *zap.Logger
	now     func() time.Time

	// mu guards only the two late-bound collaborators. Neither is held
	// across a saga: the hooks re-enter the lifecycle, and holding a lock
	// over the indexer's own teardown would invert the lock order.
	mu        sync.RWMutex
	watcherFn func() RepoWatcher
	notifier  LifecycleNotifier
	// batchDepth / batchPending coalesce the fan-out across a multi-repo
	// operation. Rerunning the whole-graph analysis once per repository in a
	// reload of twenty of them would cost twenty whole-graph passes to reach
	// the same answer the last one gives.
	batchDepth   int
	batchPending bool
}

// NewCheckoutLifecycle builds the lifecycle. It fails only on a missing
// indexer; everything else degrades to the pre-catalog behaviour.
func NewCheckoutLifecycle(cfg CheckoutLifecycleConfig) (*CheckoutLifecycle, error) {
	if cfg.MultiIndexer == nil {
		return nil, errors.New("indexer: checkout lifecycle needs a multi-repo indexer")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = zap.NewNop()
	}
	now := cfg.Clock
	if now == nil {
		now = time.Now
	}
	l := &CheckoutLifecycle{
		mi:     cfg.MultiIndexer,
		cfgMgr: cfg.ConfigManager,
		logger: logger,
		now:    now,
	}

	provider, ok := cfg.Graph.(interface {
		Catalog() *store_sqlite.Catalog
	})
	if !ok {
		return l, nil
	}
	l.catalog = provider.Catalog()

	rcfg := cfg.Reconcile
	if rcfg.AvailabilityGrace <= 0 || rcfg.RemovalGrace <= 0 {
		rcfg = reconcile.Default()
	}
	rec, err := reconcile.New(l.catalog, cleanupHooks{l: l}, rcfg, reconcile.WithClock(now))
	if err != nil {
		return nil, fmt.Errorf("indexer: build checkout reconciler: %w", err)
	}
	l.rec = rec
	return l, nil
}

// SetWatcherSource installs the accessor for the live file watcher. The
// watcher is built during warmup, long after the lifecycle, so it is read
// through a function rather than captured. The accessor must return a nil
// interface — not a typed nil — while no watcher exists.
func (l *CheckoutLifecycle) SetWatcherSource(fn func() RepoWatcher) {
	if l == nil {
		return
	}
	l.mu.Lock()
	l.watcherFn = fn
	l.mu.Unlock()
}

// SetNotifier installs the session/analysis fan-out.
func (l *CheckoutLifecycle) SetNotifier(n LifecycleNotifier) {
	if l == nil {
		return
	}
	l.mu.Lock()
	l.notifier = n
	l.mu.Unlock()
}

// Reconciler returns the lifecycle's reconciler, nil when the store has no
// catalog.
func (l *CheckoutLifecycle) Reconciler() *reconcile.Reconciler {
	if l == nil {
		return nil
	}
	return l.rec
}

// --- registration -------------------------------------------------------

// RegisterResult is what one registration did.
type RegisterResult struct {
	// Prefix is the repo prefix the checkout registered under.
	Prefix string
	// Index is the first index's result, nil when the repository was
	// already tracked.
	Index *IndexResult
	// AlreadyTracked reports that the corpus already held this repository,
	// so only the identity and the side effects were brought up to date.
	AlreadyTracked bool
	// CheckoutID / Incarnation / FamilyID / GraphID are the catalog identity,
	// empty when the store has no catalog or git does not administer the path.
	CheckoutID  string
	Incarnation string
	FamilyID    string
	GraphID     string
	// CatalogErr is a registration failure that left the index in place.
	// It is reported rather than returned: the corpus is the user-visible
	// product of a track, and a catalog that could not record the identity
	// must not undo a successful index.
	CatalogErr error
}

// Register indexes a repository and records everything that follows from it.
//
// This is the one path behind every explicit track, whichever surface asked:
// index, family identity, checkout identity, tracking intent, dedicated-graph
// binding, watcher attach, config persist, session invalidation. The source
// kind is the only thing that differs between surfaces.
func (l *CheckoutLifecycle) Register(
	ctx context.Context,
	entry config.RepoEntry,
	source store_sqlite.IntentSourceKind,
) (RegisterResult, error) {
	if l == nil || l.mi == nil {
		return RegisterResult{}, errors.New("indexer: checkout lifecycle is not wired")
	}
	absPath, err := filepath.Abs(entry.Path)
	if err != nil {
		return RegisterResult{}, fmt.Errorf("resolve path %s: %w", entry.Path, err)
	}

	result, err := l.mi.TrackRepoCtx(ctx, entry)
	if err != nil {
		return RegisterResult{}, err
	}
	out := RegisterResult{Index: result, AlreadyTracked: result == nil}
	switch {
	case result != nil && result.RepoPrefix != "":
		out.Prefix = result.RepoPrefix
	default:
		out.Prefix = l.ResolvePrefix(absPath)
	}
	if out.Prefix == "" {
		out.Prefix = config.ResolvePrefix(entry)
	}

	identity, catalogErr := l.recordCheckout(ctx, out.Prefix, absPath, source, false)
	out.CheckoutID, out.Incarnation = identity.checkoutID, identity.incarnation
	out.FamilyID, out.GraphID = identity.familyID, identity.graphID
	if catalogErr != nil {
		out.CatalogErr = catalogErr
		l.logger.Warn("checkout lifecycle: could not record the tracked checkout",
			zap.String("prefix", out.Prefix), zap.String("root", absPath), zap.Error(catalogErr))
	}

	l.attachWatcher(out.Prefix)
	l.saveConfig("track")
	l.notifyTrackedSetChanged()
	return out, nil
}

// RecordImplicit records a checkout nobody asked for.
//
// The auto-index path indexes the working directory on its own initiative,
// so the checkout is real but the intent is not: the family, checkout and
// graph-binding rows are written, and no tracking intent is. The watcher and
// the session invalidation match an explicit registration, since an
// implicitly indexed repository is served exactly like any other.
//
// The tracked-repository list is deliberately NOT persisted. The indexer adds
// the path to the in-memory configuration, and writing that out would put an
// entry in the user's config file for a path nobody asked to track — which
// the next boot's seeding would read back as explicit configuration and mint a
// manual_config intent for, turning an intent-less observation into intent one
// restart later.
func (l *CheckoutLifecycle) RecordImplicit(ctx context.Context, root string) error {
	if l == nil || l.mi == nil {
		return nil
	}
	prefix := l.ResolvePrefix(root)
	if prefix == "" {
		return fmt.Errorf("%w: %s", ErrCheckoutNotTracked, root)
	}
	_, err := l.recordCheckout(ctx, prefix, root, TrackSourceImplicit, false)
	l.attachWatcher(prefix)
	l.notifyTrackedSetChanged()
	return err
}

// checkoutIdentity is the catalog identity of one registered checkout.
type checkoutIdentity struct {
	familyID    string
	checkoutID  string
	incarnation string
	graphID     string
}

// recordCheckout writes the catalog rows one tracked root implies.
//
// seeding narrows it to a migration: an identity that already exists is left
// exactly as it is, so persisted clocks are honoured rather than reset and a
// second seeding pass writes the same rows as the first.
func (l *CheckoutLifecycle) recordCheckout(
	ctx context.Context,
	prefix, root string,
	source store_sqlite.IntentSourceKind,
	seeding bool,
) (checkoutIdentity, error) {
	if l.catalog == nil || prefix == "" {
		return checkoutIdentity{}, nil
	}
	inv, err := gitstate.Inventory(ctx, root)
	if err != nil {
		// A directory git does not administer has no family to belong to.
		// It is still indexed and served; it simply has no lifecycle
		// identity, which is what the catalog says by holding no row.
		return checkoutIdentity{}, nil
	}
	record := recordForRoot(inv, root)
	if record == nil || record.AdminName == "" {
		return checkoutIdentity{}, fmt.Errorf(
			"git does not list %s as a worktree of %s", root, inv.CommonDir)
	}

	now := l.now()
	familyID := FamilyIDFor(inv.CommonDir)
	if err := l.upsertFamily(ctx, familyID, inv.CommonDir, now.Unix()); err != nil {
		return checkoutIdentity{}, err
	}

	identity := checkoutIdentity{familyID: familyID}
	existing, err := l.checkoutByAdminName(ctx, familyID, record.AdminName)
	if err != nil {
		return identity, err
	}
	switch {
	case existing != nil:
		identity.checkoutID, identity.incarnation = existing.CheckoutID, existing.Incarnation
		if !seeding {
			if err := l.confirmPresent(ctx, *existing, record, inv, now); err != nil {
				return identity, err
			}
		}
	default:
		minted, err := l.allocateCheckout(ctx, familyID, root, record, inv, now)
		if err != nil {
			return identity, err
		}
		identity.checkoutID, identity.incarnation = minted.CheckoutID, minted.Incarnation
	}

	if source != TrackSourceImplicit {
		intent := store_sqlite.TrackingIntent{
			IntentID:      uuid.NewV7().String(),
			CheckoutID:    identity.checkoutID,
			SourceKind:    source,
			SourceLocator: root,
			Active:        true,
			CreatedAt:     now.Unix(),
		}
		if err := l.catalog.UpsertTrackingIntent(ctx, intent); err != nil {
			return identity, err
		}
	}

	graphID, err := l.bindDedicatedGraph(ctx, familyID, identity.checkoutID, prefix)
	if err != nil {
		return identity, err
	}
	identity.graphID = graphID

	if err := l.recordPathEvidence(ctx, identity.checkoutID, root, now, seeding); err != nil {
		return identity, err
	}
	return identity, nil
}

// upsertFamily writes the family row, preserving the creation timestamp of
// one that already exists.
func (l *CheckoutLifecycle) upsertFamily(ctx context.Context, familyID, commonDir string, now int64) error {
	family := store_sqlite.RepositoryFamily{
		FamilyID:          familyID,
		CommonDirIdentity: commonDir,
		State:             reconcile.FamilyStateReady,
		CreatedAt:         now,
		LastSeen:          now,
	}
	existing, ok, err := l.catalog.GetRepositoryFamily(ctx, familyID)
	if err != nil {
		return err
	}
	if ok {
		// The primary epoch is a compare-and-set token; rewriting it here
		// would silently invalidate a promotion another actor is holding.
		family.CreatedAt = existing.CreatedAt
		family.PrimaryEpoch = existing.PrimaryEpoch
		family.DisplayRemote = existing.DisplayRemote
	}
	return l.catalog.UpsertRepositoryFamily(ctx, family)
}

// checkoutByAdminName finds a family's checkout by the name git administers
// it under, which is the identity the lifecycle keys on.
func (l *CheckoutLifecycle) checkoutByAdminName(
	ctx context.Context, familyID, adminName string,
) (*store_sqlite.Checkout, error) {
	checkouts, err := l.catalog.ListCheckouts(ctx, familyID)
	if err != nil {
		return nil, err
	}
	for i := range checkouts {
		if checkouts[i].AdminName == adminName {
			return &checkouts[i], nil
		}
	}
	return nil, nil
}

// allocateCheckout mints a durable identity through the guarded allocator, so
// two surfaces racing to track the same working copy end with one row.
func (l *CheckoutLifecycle) allocateCheckout(
	ctx context.Context,
	familyID, root string,
	record *gitstate.WorktreeRecord,
	inv *gitstate.FamilyInventory,
	now time.Time,
) (store_sqlite.Checkout, error) {
	checkout := store_sqlite.Checkout{
		CheckoutID:     uuid.NewV7().String(),
		Incarnation:    uuid.NewV7().String(),
		FamilyID:       familyID,
		RootPath:       root,
		GitDir:         gitDirFor(inv, record),
		AdminName:      record.AdminName,
		State:          store_sqlite.CheckoutStateReady,
		DesiredMode:    store_sqlite.CheckoutModeDedicated,
		EffectiveMode:  store_sqlite.CheckoutModeDedicated,
		Locked:         record.Locked,
		Prunable:       record.Prunable,
		HeadRef:        record.HEADRef,
		HeadCommit:     record.HEADOID,
		LastAccessible: now.Unix(),
		LastSeen:       now.Unix(),
	}
	err := l.catalog.AllocateCheckout(ctx, checkout)
	if err == nil {
		return checkout, nil
	}
	if !errors.Is(err, store_sqlite.ErrCatalogStaleGuard) {
		return store_sqlite.Checkout{}, err
	}
	// Another actor allocated this administrative name first. Its row is the
	// identity; adopting it is what keeps one working copy to one identity.
	winner, lookupErr := l.checkoutByAdminName(ctx, familyID, record.AdminName)
	if lookupErr != nil {
		return store_sqlite.Checkout{}, lookupErr
	}
	if winner == nil {
		return store_sqlite.Checkout{}, err
	}
	return *winner, nil
}

// confirmPresent tells an existing identity that its root just answered.
//
// An explicit track is first-hand evidence of presence, so it clears both
// clocks the same way a reconciliation pass would: a path that was inside its
// removal grace must not be deleted moments after someone re-tracked it.
func (l *CheckoutLifecycle) confirmPresent(
	ctx context.Context,
	existing store_sqlite.Checkout,
	record *gitstate.WorktreeRecord,
	inv *gitstate.FamilyInventory,
	now time.Time,
) error {
	req := store_sqlite.UpdateCheckoutObservationRequest{
		CheckoutID:     existing.CheckoutID,
		Incarnation:    existing.Incarnation,
		State:          store_sqlite.CheckoutStateReady,
		RootPath:       record.Path,
		GitDir:         gitDirFor(inv, record),
		Locked:         record.Locked,
		Prunable:       record.Prunable,
		HeadRef:        record.HEADRef,
		HeadCommit:     record.HEADOID,
		HeadTree:       existing.HeadTree,
		LastAccessible: now.Unix(),
		LastSeen:       now.Unix(),
	}
	if err := l.catalog.UpdateCheckoutObservation(ctx, req); err != nil {
		if errors.Is(err, store_sqlite.ErrCatalogStaleGuard) {
			// Another actor re-keyed the row between the read and this
			// write. Its incarnation is the current one; the next pass
			// observes the path under it.
			return nil
		}
		return err
	}
	return nil
}

// bindDedicatedGraph binds a checkout to the repo prefix its nodes live
// under, and makes it the family's primary base when the family has none.
func (l *CheckoutLifecycle) bindDedicatedGraph(
	ctx context.Context, familyID, checkoutID, prefix string,
) (string, error) {
	if checkoutID == "" {
		return "", nil
	}
	graphID := GraphIDFor(prefix)
	graphs, err := l.catalog.ListDedicatedGraphs(ctx, familyID)
	if err != nil {
		return "", err
	}
	primaryHeld := false
	for _, g := range graphs {
		if g.IsPrimaryBase && g.GraphID != graphID {
			primaryHeld = true
			break
		}
	}
	row := store_sqlite.DedicatedGraph{
		GraphID:         graphID,
		OwnerCheckoutID: checkoutID,
		RepoPrefix:      prefix,
		FamilyID:        familyID,
		IsPrimaryBase:   !primaryHeld,
		State:           reconcile.GraphStateReady,
	}
	if err := l.catalog.UpsertDedicatedGraph(ctx, row); err != nil {
		if !row.IsPrimaryBase {
			return "", err
		}
		// A concurrent registration won the family's primary slot; the
		// partial unique index is what refused this one. Bind as an
		// ordinary dedicated graph instead.
		row.IsPrimaryBase = false
		if retryErr := l.catalog.UpsertDedicatedGraph(ctx, row); retryErr != nil {
			return "", retryErr
		}
	}
	return graphID, nil
}

// recordPathEvidence stores the filesystem sample a later removal has to be
// compared against. Without it a vanished root can never be told apart from
// an unmounted volume, so the checkout would sit in availability grace
// forever instead of being cleaned up.
//
// A seeding pass never overwrites an existing sample: the stored one is the
// older observation, and the removal test wants the sample from when the root
// was last known good.
func (l *CheckoutLifecycle) recordPathEvidence(
	ctx context.Context, checkoutID, root string, now time.Time, seeding bool,
) error {
	if checkoutID == "" {
		return nil
	}
	stored, present, err := l.catalog.GetCheckoutPathEvidence(ctx, checkoutID)
	if err != nil {
		return err
	}
	if present && seeding {
		return nil
	}
	fresh := reconcile.SampledPathEvidence(gitstate.SamplePathEvidence(root))
	return l.catalog.UpsertCheckoutPathEvidence(ctx,
		fresh.CatalogRow(checkoutID, now.Unix(), stored.SampleGeneration+1))
}

// --- forgetting ---------------------------------------------------------

// UntrackResult is what one explicit forget did.
type UntrackResult struct {
	Prefix       string
	CheckoutID   string
	NodesRemoved int
	EdgesRemoved int
	// Revoked names the intent sources that were withdrawn.
	Revoked []string
	// Dependents is the preview of what the forget took with it.
	Dependents []reconcile.Dependent
}

// Untrack forgets one checkout, whichever surface asked.
//
// The order is the point: every revocable tracking intent is withdrawn first
// (a non-revocable one aborts before anything is torn down), then the forget
// saga runs under the checkout's incarnation and drives the cleanup hooks —
// watcher detach, graph eviction, config persist, session invalidation — so
// the same sequence happens no matter who called.
func (l *CheckoutLifecycle) Untrack(ctx context.Context, pathOrPrefix string) (UntrackResult, error) {
	if l == nil || l.mi == nil {
		return UntrackResult{}, errors.New("indexer: checkout lifecycle is not wired")
	}
	prefix := l.ResolvePrefix(pathOrPrefix)
	if prefix == "" {
		return UntrackResult{}, fmt.Errorf("%w: %s", ErrCheckoutNotTracked, pathOrPrefix)
	}
	out := UntrackResult{Prefix: prefix}

	checkout, err := l.checkoutForPrefix(ctx, prefix)
	if err != nil {
		return out, err
	}
	if checkout == nil {
		// No catalog identity: a store without a catalog, or a directory git
		// does not administer. The side effects are the same ones the hooks
		// run, in the same order.
		out.NodesRemoved, out.EdgesRemoved = l.evictRepo(prefix)
		return out, nil
	}
	out.CheckoutID = checkout.CheckoutID

	dependents, err := l.rec.Dependents(ctx, checkout.CheckoutID)
	if err != nil {
		return out, err
	}
	out.Dependents = dependents

	revocation, err := l.rec.RevokeTrackingIntents(ctx, checkout.CheckoutID)
	if err != nil {
		return out, err
	}
	for _, intent := range revocation.Revoked {
		out.Revoked = append(out.Revoked, string(intent.SourceKind))
	}

	// The eviction happens inside the saga, so its counts are read off the
	// repository's last index before it runs — which is the same estimate
	// the store's own repo purge reports.
	before := l.mi.GetMetadata(prefix)
	if err := l.rec.ForgetCheckout(ctx, checkout.CheckoutID, checkout.Incarnation); err != nil {
		return out, err
	}
	if before != nil {
		out.NodesRemoved, out.EdgesRemoved = before.NodeCount, before.EdgeCount
	}
	// The saga evicts through ReleaseGraph. A checkout that never had a
	// graph binding still has to leave the corpus.
	if l.mi.GetMetadata(prefix) != nil {
		out.NodesRemoved, out.EdgesRemoved = l.evictRepo(prefix)
	}
	return out, nil
}

// --- reload -------------------------------------------------------------

// ReloadResult counts what one configuration reload did.
type ReloadResult struct {
	Added   int
	Removed int
	// Pending counts entries whose removal was recorded as an intent
	// transition instead of being applied.
	Pending int
	// Refreshed is the number of tracked repositories whose per-repo config
	// was re-read.
	Refreshed int
}

// ApplyReload brings the tracked set in line with the configuration file.
//
// Additions go through the registration helper, so a repository added by
// editing the config gets the same identity, watcher and invalidation an
// explicit track would have given it. Removals go through the reconciler's
// retirement rule rather than a direct eviction: an entry that cannot be
// dropped safely records a pending transition and stays, which is what stops
// a configuration edit from silently deleting a corpus.
func (l *CheckoutLifecycle) ApplyReload(ctx context.Context) (ReloadResult, error) {
	if l == nil || l.mi == nil || l.cfgMgr == nil {
		return ReloadResult{}, errors.New("indexer: checkout lifecycle is not wired for reload")
	}
	// One fan-out for the whole diff: every add and every removal below
	// changes the tracked set, and telling the sessions after each one would
	// pay for the same answer as many times as the diff is long.
	defer l.beginBatch()()

	out := ReloadResult{Refreshed: l.mi.RefreshRepoConfigs()}

	// Match configured entries to tracked instances by ROOT PATH. A worktree
	// tracked as an independent instance registers under a derived prefix, so
	// a recomputed prefix would not recognise it as wanted.
	trackedByRoot := map[string]string{}
	for prefix, meta := range l.mi.AllMetadata() {
		if meta != nil {
			trackedByRoot[meta.RootPath] = prefix
		}
	}

	wanted := map[string]bool{}
	for _, entry := range l.cfgMgr.Global().Repos {
		abs, err := filepath.Abs(entry.Path)
		if err != nil {
			abs = entry.Path
		}
		if prefix, ok := trackedByRoot[abs]; ok {
			wanted[prefix] = true
			continue
		}
		res, err := l.Register(ctx, entry, TrackSourceConfig)
		if err != nil {
			l.logger.Warn("reload: track failed",
				zap.String("path", entry.Path), zap.Error(err))
			continue
		}
		out.Added++
		if res.Prefix != "" {
			wanted[res.Prefix] = true
		}
	}

	for prefix := range l.mi.AllMetadata() {
		if wanted[prefix] {
			continue
		}
		outcome, err := l.retireOnReload(ctx, prefix)
		if err != nil {
			l.logger.Warn("reload: retire failed",
				zap.String("prefix", prefix), zap.Error(err))
			continue
		}
		switch outcome {
		case reconcile.OutcomeTransitionPending:
			out.Pending++
		default:
			out.Removed++
		}
	}
	return out, nil
}

// retireOnReload applies the reconciler's retirement rule to one prefix that
// left the configuration.
func (l *CheckoutLifecycle) retireOnReload(ctx context.Context, prefix string) (reconcile.RetireOutcome, error) {
	checkout, err := l.checkoutForPrefix(ctx, prefix)
	if err != nil {
		return "", err
	}
	if checkout == nil {
		// No identity to reason about — a store without a catalog, or a
		// directory git does not administer. Keeping the pre-catalog
		// behaviour is what stops such a repository from becoming
		// impossible to remove.
		l.evictRepo(prefix)
		return reconcile.OutcomeForgotten, nil
	}
	outcome, err := l.rec.RetireCheckout(ctx, checkout.CheckoutID, checkout.Incarnation, "reload_removed_from_config")
	if err != nil {
		return "", err
	}
	if outcome == reconcile.OutcomeForgotten && l.mi.GetMetadata(prefix) != nil {
		l.evictRepo(prefix)
	}
	return outcome, nil
}

// --- periodic sweep -----------------------------------------------------

// SweepReport is one janitor pass over every family the daemon knows.
type SweepReport struct {
	// Families is the number of families reconciled.
	Families int
	// Reports are the per-family verdicts, in the order they were taken.
	Reports []reconcile.FamilyReport
	// Removed counts checkouts the pass forgot or retired.
	Removed int
}

// Sweep resumes unfinished cleanups and reconciles every known family.
//
// It replaces the old "the directory is gone, evict it" check. That test
// could not tell a deleted worktree from an unmounted volume, so it had to be
// narrowed to linked worktrees to be safe at all; the reconciler decides on
// evidence and two separate clocks, which is what lets it act on any checkout
// without risking a corpus over a transient stat failure.
func (l *CheckoutLifecycle) Sweep(ctx context.Context) (SweepReport, error) {
	var out SweepReport
	if l == nil || l.rec == nil {
		return out, nil
	}
	// One fan-out for the whole sweep, however many families it touches.
	defer l.beginBatch()()

	var errs []error
	if err := l.rec.Resume(ctx); err != nil {
		errs = append(errs, err)
	}
	for _, fam := range l.knownFamilies(ctx) {
		report, err := l.rec.ReconcileFamily(ctx, fam.familyID, fam.probeDir)
		if err != nil {
			errs = append(errs, fmt.Errorf("family %s: %w", fam.familyID, err))
			continue
		}
		out.Families++
		out.Reports = append(out.Reports, report)
		for _, checkout := range report.Checkouts {
			switch checkout.Action {
			case reconcile.ActionForgotten, reconcile.ActionPrimaryClosureRetired:
				out.Removed++
			}
		}
	}
	if out.Removed > 0 {
		// The cleanup hooks drop the removed repositories from the in-memory
		// configuration; without this the removal is forgotten on restart.
		l.saveConfig("janitor")
		l.notifyTrackedSetChanged()
	}
	return out, errors.Join(errs...)
}

// familyProbe is one family and the directory to read its inventory from.
type familyProbe struct {
	familyID string
	probeDir string
}

// knownFamilies enumerates the families reachable from what is tracked and
// from what is configured.
//
// The family of a prefix is read from its dedicated-graph binding rather than
// from the filesystem, so a checkout whose root has vanished is still
// reconciled — that root is exactly the one that cannot answer.
//
// The corpus alone is not enough to enumerate from. Boot skips a configured
// repository whose root cannot be stat'ed, which leaves it with catalog rows
// and no corpus metadata; enumerating from the corpus only would drop exactly
// the checkout that availability handling exists for. So the configured
// entries are resolved to their families too, by the same prefix rule the
// startup seeding uses, and the two sets are unioned.
//
// The probe directory is chosen for the family, not for the checkout: any
// still-reachable checkout root will do, and the family's shared git
// directory is the fallback that keeps working when every worktree root is
// gone but the repository is not.
func (l *CheckoutLifecycle) knownFamilies(ctx context.Context) []familyProbe {
	seen := map[string]bool{}
	var out []familyProbe
	add := func(familyID, fallbackDir string) {
		if familyID == "" || seen[familyID] {
			return
		}
		seen[familyID] = true
		out = append(out, familyProbe{
			familyID: familyID,
			probeDir: l.probeDirFor(ctx, familyID, fallbackDir),
		})
	}

	for prefix, meta := range l.mi.AllMetadata() {
		if meta == nil {
			continue
		}
		add(l.familyForPrefix(ctx, prefix), meta.RootPath)
	}

	if l.cfgMgr == nil {
		return out
	}
	for _, entry := range l.cfgMgr.Global().Repos {
		abs, err := filepath.Abs(entry.Path)
		if err != nil {
			abs = entry.Path
		}
		prefix := l.ResolvePrefix(abs)
		if prefix == "" {
			prefix = EffectiveRepoPrefix(l.cfgMgr, entry)
		}
		add(l.familyForPrefix(ctx, prefix), abs)
	}
	return out
}

// familyForPrefix reads the family a repo prefix is bound to, empty when the
// prefix has no dedicated-graph binding.
func (l *CheckoutLifecycle) familyForPrefix(ctx context.Context, prefix string) string {
	if l.catalog == nil || prefix == "" {
		return ""
	}
	binding, ok, err := l.catalog.GetDedicatedGraph(ctx, GraphIDFor(prefix))
	if err != nil || !ok {
		return ""
	}
	return binding.FamilyID
}

// probeDirFor picks the directory a family's inventory is read from.
func (l *CheckoutLifecycle) probeDirFor(ctx context.Context, familyID, fallback string) string {
	checkouts, err := l.catalog.ListCheckouts(ctx, familyID)
	if err == nil {
		for _, checkout := range checkouts {
			if checkout.RootPath != "" && dirExists(checkout.RootPath) {
				return checkout.RootPath
			}
		}
	}
	if family, ok, err := l.catalog.GetRepositoryFamily(ctx, familyID); err == nil && ok {
		if dirExists(family.CommonDirIdentity) {
			return family.CommonDirIdentity
		}
	}
	return fallback
}

// --- startup ------------------------------------------------------------

// Seed brings the catalog in line with what the daemon already tracks.
//
// It is the migration path for an installation that predates the catalog and
// the restart path for one that does not: every configured repository gets
// its family, checkout, intent and graph rows without being re-indexed, an
// identity that already exists is left untouched so its clocks survive the
// restart, and any teardown that was in flight when the process died is
// resumed.
func (l *CheckoutLifecycle) Seed(ctx context.Context) error {
	if l == nil || l.rec == nil || l.cfgMgr == nil {
		return nil
	}
	var errs []error
	for _, entry := range l.cfgMgr.Global().Repos {
		abs, err := filepath.Abs(entry.Path)
		if err != nil {
			abs = entry.Path
		}
		prefix := l.ResolvePrefix(abs)
		if prefix == "" {
			prefix = EffectiveRepoPrefix(l.cfgMgr, entry)
		}
		if prefix == "" {
			continue
		}
		if _, err := l.recordCheckout(ctx, prefix, abs, TrackSourceConfig, true); err != nil {
			errs = append(errs, fmt.Errorf("seed %s: %w", abs, err))
		}
	}
	if err := l.rec.Resume(ctx); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// --- cleanup hooks ------------------------------------------------------

// cleanupHooks binds the reconciler's two extension points to what the
// daemon actually owns today.
type cleanupHooks struct{ l *CheckoutLifecycle }

// PurgeCheckoutLayers drops what has been built for one incarnation.
//
// Built layers do not exist yet. What does exist for a served checkout is its
// live file watcher, so purging is detaching it: the checkout keeps its
// identity and its nodes, it just stops absorbing filesystem events while it
// is unreachable. Layer deletion and in-flight build cancellation extend
// here, alongside the detach.
func (h cleanupHooks) PurgeCheckoutLayers(ctx context.Context, checkoutID, _ string) error {
	prefix := h.l.prefixForCheckout(ctx, checkoutID)
	if prefix == "" {
		return nil
	}
	h.l.detachWatcher(prefix)
	return nil
}

// ReleaseGraph gives up whatever holds a dedicated graph open.
//
// The graph row names the repo prefix its nodes live under, so releasing it
// is the repository eviction the untrack path has always run — in the order
// that path established: detach the watcher before evicting, so a late
// filesystem event cannot re-index files whose nodes are already gone.
func (h cleanupHooks) ReleaseGraph(ctx context.Context, graphID string) error {
	row, ok, err := h.l.catalog.GetDedicatedGraph(ctx, graphID)
	if err != nil {
		return err
	}
	if !ok || row.RepoPrefix == "" {
		return nil
	}
	h.l.evictRepo(row.RepoPrefix)
	return nil
}

// --- side effects -------------------------------------------------------

// evictRepo runs the repository teardown every caller shares: watcher first,
// then the graph, then the persisted configuration, then the sessions.
func (l *CheckoutLifecycle) evictRepo(prefix string) (nodesRemoved, edgesRemoved int) {
	if prefix == "" {
		return 0, 0
	}
	l.detachWatcher(prefix)
	nodesRemoved, edgesRemoved = l.mi.UntrackRepo(prefix)
	l.saveConfig("untrack")
	l.notifyTrackedSetChanged()
	return nodesRemoved, edgesRemoved
}

// attachWatcher wires a tracked prefix into the live file watcher. A failure
// leaves an indexed but unwatched repository, which is queryable and only
// goes stale on edit — not a reason to fail the track.
func (l *CheckoutLifecycle) attachWatcher(prefix string) {
	watcher := l.watcher()
	if watcher == nil || prefix == "" || l.cfgMgr == nil {
		return
	}
	if err := watcher.AddRepo(prefix, l.cfgMgr.GetRepoConfig(prefix).Watch); err != nil {
		l.logger.Warn("checkout lifecycle: attach watcher failed",
			zap.String("prefix", prefix), zap.Error(err))
	}
}

// detachWatcher stops watching a prefix. Detaching one that is not attached
// is not an error worth reporting: every teardown path calls it, and the
// second call is the idempotent one.
func (l *CheckoutLifecycle) detachWatcher(prefix string) {
	watcher := l.watcher()
	if watcher == nil || prefix == "" {
		return
	}
	if err := watcher.RemoveRepo(prefix); err != nil {
		l.logger.Debug("checkout lifecycle: detach watcher",
			zap.String("prefix", prefix), zap.Error(err))
	}
}

// saveConfig flushes the tracked-repository list. The indexer mutates it in
// memory; without this the change vanishes on the next restart.
func (l *CheckoutLifecycle) saveConfig(reason string) {
	if l.cfgMgr == nil {
		return
	}
	if err := l.cfgMgr.Global().Save(); err != nil {
		l.logger.Warn("checkout lifecycle: save config failed",
			zap.String("reason", reason), zap.Error(err))
	}
}

// notifyTrackedSetChanged tells the query surface that the tracked set moved,
// or records that it will have to be told once the running batch ends.
func (l *CheckoutLifecycle) notifyTrackedSetChanged() {
	l.mu.Lock()
	notifier := l.notifier
	if l.batchDepth > 0 {
		l.batchPending = true
		l.mu.Unlock()
		return
	}
	l.mu.Unlock()
	if notifier == nil {
		return
	}
	notifier.InvalidateSessionScopes()
	notifier.RunAnalysis()
}

// beginBatch coalesces every fan-out until the returned function runs.
func (l *CheckoutLifecycle) beginBatch() func() {
	l.mu.Lock()
	l.batchDepth++
	l.mu.Unlock()
	return func() {
		l.mu.Lock()
		l.batchDepth--
		fire := l.batchDepth == 0 && l.batchPending
		if fire {
			l.batchPending = false
		}
		l.mu.Unlock()
		if fire {
			l.notifyTrackedSetChanged()
		}
	}
}

// watcher reads the late-bound watcher accessor.
func (l *CheckoutLifecycle) watcher() RepoWatcher {
	l.mu.RLock()
	fn := l.watcherFn
	l.mu.RUnlock()
	if fn == nil {
		return nil
	}
	return fn()
}

// --- lookups ------------------------------------------------------------

// ResolvePrefix resolves a repo prefix, an absolute root path, or a path
// inside a tracked repository to the prefix it is served under.
func (l *CheckoutLifecycle) ResolvePrefix(pathOrPrefix string) string {
	if l == nil || l.mi == nil || pathOrPrefix == "" {
		return ""
	}
	if meta := l.mi.GetMetadata(pathOrPrefix); meta != nil {
		return pathOrPrefix
	}
	abs, err := filepath.Abs(pathOrPrefix)
	if err != nil {
		return ""
	}
	best, bestLen := "", -1
	for prefix, meta := range l.mi.AllMetadata() {
		if meta == nil || meta.RootPath == "" {
			continue
		}
		root, err := filepath.Abs(meta.RootPath)
		if err != nil {
			continue
		}
		if pathkey.EqualPaths(root, abs) {
			return prefix
		}
		rel, err := filepath.Rel(root, abs)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
			continue
		}
		if len(root) > bestLen {
			best, bestLen = prefix, len(root)
		}
	}
	return best
}

// checkoutForPrefix reads the checkout a repo prefix is bound to, nil when
// the prefix has no catalog identity.
func (l *CheckoutLifecycle) checkoutForPrefix(ctx context.Context, prefix string) (*store_sqlite.Checkout, error) {
	if l.catalog == nil || prefix == "" {
		return nil, nil
	}
	graph, ok, err := l.catalog.GetDedicatedGraph(ctx, GraphIDFor(prefix))
	if err != nil || !ok || graph.OwnerCheckoutID == "" {
		return nil, err
	}
	checkout, ok, err := l.catalog.GetCheckout(ctx, graph.OwnerCheckoutID)
	if err != nil || !ok {
		return nil, err
	}
	return &checkout, nil
}

// prefixForCheckout resolves a checkout back to the repo prefix serving it.
func (l *CheckoutLifecycle) prefixForCheckout(ctx context.Context, checkoutID string) string {
	if l.catalog == nil || checkoutID == "" {
		return ""
	}
	checkout, ok, err := l.catalog.GetCheckout(ctx, checkoutID)
	if err != nil || !ok {
		return ""
	}
	graphs, err := l.catalog.ListDedicatedGraphs(ctx, checkout.FamilyID)
	if err == nil {
		for _, g := range graphs {
			if g.OwnerCheckoutID == checkoutID && g.RepoPrefix != "" {
				return g.RepoPrefix
			}
		}
	}
	return l.ResolvePrefix(checkout.RootPath)
}

// --- identifiers --------------------------------------------------------

// FamilyIDFor derives a checkout family's identifier from the shared git
// directory every worktree of the family reads objects from.
//
// It is derived rather than generated so two processes — and the same daemon
// across restarts — reach the same identity for the same repository without
// having to look one up by common directory first.
func FamilyIDFor(commonDir string) string {
	return "family-" + digest(filepath.Clean(commonDir))
}

// GraphIDFor derives a dedicated graph's identifier from the repo prefix its
// nodes are stored under. The prefix is unique across the corpus, so the
// binding is reproducible from either side.
func GraphIDFor(repoPrefix string) string {
	return "graph-" + digest(repoPrefix)
}

// digest renders a stable short identifier for a string.
func digest(in string) string {
	sum := sha256.Sum256([]byte(in))
	return hex.EncodeToString(sum[:16])
}

// recordForRoot finds the inventory record describing one worktree root.
//
// Git spells every path with its symlinks resolved; a tracked root is
// spelled the way the configuration wrote it, which on some platforms is a
// path through a symlink to the very same directory. So a failed string
// comparison falls back to filesystem identity rather than concluding that
// git does not know the checkout.
func recordForRoot(inv *gitstate.FamilyInventory, root string) *gitstate.WorktreeRecord {
	if inv == nil {
		return nil
	}
	for i := range inv.Records {
		if pathkey.EqualPaths(inv.Records[i].Path, root) {
			return &inv.Records[i]
		}
	}
	rootInfo, err := os.Stat(root)
	if err != nil {
		return nil
	}
	for i := range inv.Records {
		info, err := os.Stat(inv.Records[i].Path)
		if err == nil && os.SameFile(rootInfo, info) {
			return &inv.Records[i]
		}
	}
	return nil
}

// gitDirFor spells out a record's own git directory: the shared directory for
// the main worktree, an administrative directory underneath it for a linked
// one.
func gitDirFor(inv *gitstate.FamilyInventory, record *gitstate.WorktreeRecord) string {
	if inv == nil || record == nil {
		return ""
	}
	if record.IsMain || record.AdminName == gitstate.MainAdminName {
		return inv.CommonDir
	}
	if record.AdminName == "" {
		return ""
	}
	return filepath.Join(inv.CommonDir, "worktrees", record.AdminName)
}

// dirExists reports whether a directory is reachable right now.
func dirExists(path string) bool {
	if path == "" {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
