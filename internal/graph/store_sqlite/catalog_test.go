package store_sqlite

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// catalogTables is every table the checkout-lifecycle control plane owns. The
// migration test drops all of them to recreate a pre-catalog store, and the
// fresh-store test asserts Open creates all of them.
var catalogTables = []string{
	"repository_families",
	"checkouts",
	"tracking_intents",
	"intent_transitions",
	"checkout_path_evidence",
	"dedicated_graphs",
	"view_generations",
	"view_layers",
	"checkout_routes",
	"ref_views",
	"ref_view_builds",
	"cleanup_journal",
}

func openCatalogStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "catalog.sqlite"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func hasTable(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var present bool
	if err := db.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = ?)`, name,
	).Scan(&present); err != nil {
		t.Fatalf("probe table %s: %v", name, err)
	}
	return present
}

// seedFamilyAndCheckout installs the minimum control plane a lifecycle test
// needs: one family and one ready checkout inside it.
func seedFamilyAndCheckout(t *testing.T, catalog *Catalog, familyID, checkoutID, incarnation string) {
	t.Helper()
	ctx := context.Background()
	if err := catalog.UpsertRepositoryFamily(ctx, RepositoryFamily{
		FamilyID:          familyID,
		CommonDirIdentity: "identity/" + familyID,
		DisplayRemote:     "git@example.invalid:" + familyID + ".git",
		State:             "family_ready",
		CreatedAt:         100,
		LastSeen:          100,
	}); err != nil {
		t.Fatalf("UpsertRepositoryFamily: %v", err)
	}
	if err := catalog.UpsertCheckout(ctx, Checkout{
		CheckoutID:    checkoutID,
		Incarnation:   incarnation,
		FamilyID:      familyID,
		RootPath:      "/tmp/" + checkoutID,
		GitDir:        "/tmp/" + checkoutID + "/.git",
		AdminName:     checkoutID,
		State:         CheckoutStateReady,
		DesiredMode:   CheckoutModeAutomatic,
		EffectiveMode: CheckoutModeAutomatic,
		HeadRef:       "refs/heads/main",
		HeadCommit:    "c0ffee",
		HeadTree:      "7ee7",
		LastSeen:      101,
	}); err != nil {
		t.Fatalf("UpsertCheckout: %v", err)
	}
}

// seedBuildingGeneration creates one generation in the building state.
func seedBuildingGeneration(t *testing.T, catalog *Catalog, graphID string) int64 {
	t.Helper()
	id, err := catalog.CreateViewGeneration(context.Background(), ViewGeneration{
		OwnerKind:      "dedicated_graph",
		GraphID:        graphID,
		GenerationKind: "commit",
		TreeOID:        "tree-" + graphID,
		State:          ViewGenerationBuilding,
		CreatedAt:      200,
	})
	if err != nil {
		t.Fatalf("CreateViewGeneration: %v", err)
	}
	return id
}

// TestCatalogSchemaAppliesOnFreshStore proves Open creates the whole control
// plane on a brand-new database, and that a round trip through the accessors
// preserves every column — including the nullable ones whose empty Go value
// must come back as an empty value rather than a scan error.
func TestCatalogSchemaAppliesOnFreshStore(t *testing.T) {
	store := openCatalogStore(t)
	for _, name := range catalogTables {
		if !hasTable(t, store.writerDB, name) {
			t.Fatalf("fresh store is missing catalog table %s", name)
		}
	}

	ctx := context.Background()
	catalog := store.Catalog()
	seedFamilyAndCheckout(t, catalog, "fam", "wt", "inc-1")

	family, ok, err := catalog.GetRepositoryFamily(ctx, "fam")
	if err != nil || !ok {
		t.Fatalf("GetRepositoryFamily = %v, %v, %v", family, ok, err)
	}
	if family.CommonDirIdentity != "identity/fam" || family.PrimaryEpoch != 0 {
		t.Fatalf("family round trip = %+v", family)
	}

	checkout, ok, err := catalog.GetCheckout(ctx, "wt")
	if err != nil || !ok {
		t.Fatalf("GetCheckout = %v, %v, %v", checkout, ok, err)
	}
	if checkout.State != CheckoutStateReady || checkout.EffectiveMode != CheckoutModeAutomatic {
		t.Fatalf("checkout round trip = %+v", checkout)
	}
	if checkout.ActiveIntentTransitionID != "" {
		t.Fatalf("unset transition pointer = %q, want empty", checkout.ActiveIntentTransitionID)
	}

	checkouts, err := catalog.ListCheckouts(ctx, "fam")
	if err != nil {
		t.Fatalf("ListCheckouts: %v", err)
	}
	if len(checkouts) != 1 || checkouts[0].CheckoutID != "wt" {
		t.Fatalf("ListCheckouts = %+v, want the one seeded checkout", checkouts)
	}

	if err := catalog.UpsertCheckoutPathEvidence(ctx, CheckoutPathEvidence{
		CheckoutID:                  "wt",
		RootPathIdentity:            "dev:1,ino:2",
		RootVolumeKind:              "local",
		RootVolumeToken:             "vol-a",
		NearestExistingAncestorPath: "/tmp",
		AncestorVolumeKind:          "local",
		AncestorVolumeToken:         "vol-a",
		CommonDirVolumeKind:         "local",
		CommonDirVolumeToken:        "vol-a",
		SampledAt:                   300,
		SampleGeneration:            4,
	}); err != nil {
		t.Fatalf("UpsertCheckoutPathEvidence: %v", err)
	}
	evidence, ok, err := catalog.GetCheckoutPathEvidence(ctx, "wt")
	if err != nil || !ok {
		t.Fatalf("GetCheckoutPathEvidence = %v, %v, %v", evidence, ok, err)
	}
	if evidence.SampleGeneration != 4 || evidence.RootPathIdentity != "dev:1,ino:2" {
		t.Fatalf("path evidence round trip = %+v", evidence)
	}

	if err := catalog.UpsertViewLayer(ctx, ViewLayer{
		LayerID:      "layer-1",
		Kind:         "commit",
		GraphID:      "graph-1",
		CheckoutID:   "wt",
		TargetRef:    "refs/heads/main",
		TargetCommit: "c0ffee",
		TargetTree:   "7ee7",
	}); err != nil {
		t.Fatalf("UpsertViewLayer: %v", err)
	}
	layer, ok, err := catalog.GetViewLayer(ctx, "layer-1")
	if err != nil || !ok {
		t.Fatalf("GetViewLayer = %v, %v, %v", layer, ok, err)
	}
	if layer.TargetRef != "refs/heads/main" || layer.CheckoutID != "wt" {
		t.Fatalf("layer round trip = %+v", layer)
	}

	generationID := seedBuildingGeneration(t, catalog, "graph-1")
	generation, ok, err := catalog.GetViewGeneration(ctx, generationID)
	if err != nil || !ok {
		t.Fatalf("GetViewGeneration = %v, %v, %v", generation, ok, err)
	}
	if generation.State != ViewGenerationBuilding || generation.BaseGenerationID != 0 {
		t.Fatalf("generation round trip = %+v", generation)
	}
}

// TestCatalogSchemaMigratesExistingStore is the backward-compatibility proof:
// an on-disk store written before the catalog existed gains every table on its
// next Open, keeps its graph rows, and does not signal a rebuild. The catalog
// is additive, so this must be an in-place upgrade rather than a wipe.
func TestCatalogSchemaMigratesExistingStore(t *testing.T) {
	if currentSchemaVersion < 13 {
		t.Fatalf("currentSchemaVersion = %d, want >= 13 for the catalog migration", currentSchemaVersion)
	}
	var step *schemaMigration
	for i := range schemaMigrations {
		if schemaMigrations[i].version == 13 {
			step = &schemaMigrations[i]
			break
		}
	}
	if step == nil || step.rebuild || step.inPlace == nil {
		t.Fatalf("v13 migration = %+v, want a registered in-place step", step)
	}

	path := filepath.Join(t.TempDir(), "pre-catalog.sqlite")
	seed, err := Open(path)
	if err != nil {
		t.Fatalf("create current store: %v", err)
	}
	seed.AddBatch([]*graph.Node{
		{ID: "repo/a.go::Legacy", Kind: graph.KindFunction, Name: "Legacy", FilePath: "repo/a.go", RepoPrefix: "repo"},
	}, nil)
	if err := seed.Close(); err != nil {
		t.Fatalf("close seed store: %v", err)
	}

	// Recreate the exact pre-catalog shape: the graph exists, the catalog does
	// not, and the file is stamped at the version before the catalog shipped.
	withRawDB(t, path, func(db *sql.DB) {
		for _, name := range catalogTables {
			if _, err := db.Exec(`DROP TABLE IF EXISTS ` + name); err != nil {
				t.Fatalf("drop %s: %v", name, err)
			}
		}
		if _, err := db.Exec(`PRAGMA user_version = 12`); err != nil {
			t.Fatalf("stamp v12: %v", err)
		}
	})

	migrated, err := Open(path)
	if err != nil {
		t.Fatalf("reopen pre-catalog store: %v", err)
	}
	t.Cleanup(func() { _ = migrated.Close() })

	if migrated.NeedsRebuild() {
		t.Fatal("an additive catalog upgrade must not signal a wipe/reindex")
	}
	for _, name := range catalogTables {
		if !hasTable(t, migrated.writerDB, name) {
			t.Fatalf("migrated store is missing catalog table %s", name)
		}
	}
	if version, err := readUserVersion(migrated.writerDB); err != nil || version != currentSchemaVersion {
		t.Fatalf("post-migration user_version = %d (err %v), want %d", version, err, currentSchemaVersion)
	}
	if migrated.GetNode("repo/a.go::Legacy") == nil {
		t.Fatal("existing graph rows must survive the in-place catalog upgrade")
	}

	// The upgraded store is fully usable, not merely present.
	seedFamilyAndCheckout(t, migrated.Catalog(), "fam", "wt", "inc-1")
	if _, ok, err := migrated.Catalog().GetCheckout(context.Background(), "wt"); err != nil || !ok {
		t.Fatalf("write to migrated catalog = %v, %v", ok, err)
	}
}

// TestCatalogCheckoutDeleteCascades proves the ON DELETE CASCADE wiring: a
// checkout's intents, in-flight transition and path evidence go with it, while
// the cleanup journal — which deliberately has no foreign keys — survives,
// because its whole purpose is to outlive the rows it names.
func TestCatalogCheckoutDeleteCascades(t *testing.T) {
	store := openCatalogStore(t)
	ctx := context.Background()
	catalog := store.Catalog()
	seedFamilyAndCheckout(t, catalog, "fam", "wt", "inc-1")

	if err := catalog.UpsertTrackingIntent(ctx, TrackingIntent{
		IntentID:      "intent-1",
		CheckoutID:    "wt",
		SourceKind:    IntentSourceCLITrack,
		SourceLocator: "gortex track /tmp/wt",
		Active:        true,
		CreatedAt:     400,
	}); err != nil {
		t.Fatalf("UpsertTrackingIntent: %v", err)
	}
	if err := catalog.BeginIntentTransition(ctx, IntentTransition{
		TransitionID:       "trans-1",
		CheckoutID:         "wt",
		Cause:              "user_requested_dedicated",
		PriorDesiredMode:   CheckoutModeAutomatic,
		PriorEffectiveMode: CheckoutModeAutomatic,
		RequestedMode:      CheckoutModeDedicated,
		PriorCheckoutState: CheckoutStateReady,
		State:              IntentTransitionPending,
		CreatedAt:          401,
	}); err != nil {
		t.Fatalf("BeginIntentTransition: %v", err)
	}
	if err := catalog.UpsertCheckoutPathEvidence(ctx, CheckoutPathEvidence{
		CheckoutID: "wt", RootPathIdentity: "dev:1,ino:2", SampledAt: 402,
	}); err != nil {
		t.Fatalf("UpsertCheckoutPathEvidence: %v", err)
	}
	if err := catalog.UpsertCleanupEntry(ctx, CleanupEntry{
		CleanupID:       "cleanup-1",
		OpaqueTargetIDs: "wt",
		Reason:          "checkout_forgotten",
		Phase:           CleanupPhaseGrace,
		GraceDeadline:   500,
		PrimaryEpoch:    0,
	}); err != nil {
		t.Fatalf("UpsertCleanupEntry: %v", err)
	}

	if err := catalog.DeleteCheckout(ctx, "wt"); err != nil {
		t.Fatalf("DeleteCheckout: %v", err)
	}

	intents, err := catalog.ListTrackingIntents(ctx, "wt")
	if err != nil {
		t.Fatalf("ListTrackingIntents: %v", err)
	}
	if len(intents) != 0 {
		t.Fatalf("tracking intents survived the checkout delete: %+v", intents)
	}
	if transition, ok, err := catalog.GetIntentTransition(ctx, "wt"); err != nil || ok {
		t.Fatalf("intent transition survived the checkout delete: %+v, %v, %v", transition, ok, err)
	}
	if evidence, ok, err := catalog.GetCheckoutPathEvidence(ctx, "wt"); err != nil || ok {
		t.Fatalf("path evidence survived the checkout delete: %+v, %v, %v", evidence, ok, err)
	}

	entry, ok, err := catalog.GetCleanupEntry(ctx, "cleanup-1")
	if err != nil || !ok {
		t.Fatalf("cleanup journal entry must outlive its target: %v, %v, %v", entry, ok, err)
	}
	if entry.Phase != CleanupPhaseGrace || entry.OpaqueTargetIDs != "wt" {
		t.Fatalf("cleanup entry round trip = %+v", entry)
	}

	if err := catalog.DeleteCheckout(ctx, "wt"); !errors.Is(err, ErrCatalogNotFound) {
		t.Fatalf("second DeleteCheckout = %v, want ErrCatalogNotFound", err)
	}
}

// TestCatalogRoutedGenerationCannotBeDeleted proves the RESTRICT-style
// protection on view generations: a generation a route, a ref view, another
// generation's base pointer, or a dedicated graph's active pointer still names
// cannot be deleted, and becomes deletable only once the last reference drops.
func TestCatalogRoutedGenerationCannotBeDeleted(t *testing.T) {
	store := openCatalogStore(t)
	ctx := context.Background()
	catalog := store.Catalog()
	seedFamilyAndCheckout(t, catalog, "fam", "wt", "inc-1")

	base := seedBuildingGeneration(t, catalog, "graph-1")
	if err := catalog.PublishViewGeneration(ctx, base, 600); err != nil {
		t.Fatalf("PublishViewGeneration: %v", err)
	}

	if err := catalog.UpsertCheckoutRoute(ctx, CheckoutRoute{
		CheckoutID:         "wt",
		GraphID:            "graph-1",
		CommitGenerationID: base,
		RouteEpoch:         0,
		State:              RouteActive,
	}); err != nil {
		t.Fatalf("UpsertCheckoutRoute: %v", err)
	}

	err := catalog.DeleteViewGeneration(ctx, base)
	if !errors.Is(err, ErrCatalogGenerationReferenced) {
		t.Fatalf("delete of a routed generation = %v, want ErrCatalogGenerationReferenced", err)
	}

	// A route does not cascade, so the checkout under it is pinned too: routes
	// must be retired deliberately rather than vanishing with their checkout.
	if err := catalog.DeleteCheckout(ctx, "wt"); err == nil {
		t.Fatal("deleting a routed checkout succeeded; the route foreign key does not restrict")
	}

	// The database refuses it too, not just the Go guard: with the store's
	// per-connection foreign_keys(ON), the non-deferred NO ACTION constraint on
	// checkout_routes.commit_generation_id behaves as RESTRICT.
	if _, err := store.writerDB.ExecContext(ctx,
		`DELETE FROM view_generations WHERE generation_id = ?`, base); err == nil {
		t.Fatal("raw delete of a routed generation succeeded; the foreign key is not enforced")
	}

	// Move the route off the generation, then pin it three other ways in turn.
	if err := catalog.FlipCheckoutRoute(ctx, FlipCheckoutRouteRequest{
		CheckoutID: "wt", ExpectedRouteEpoch: 0, GraphID: "graph-1", State: RoutePending,
	}); err != nil {
		t.Fatalf("FlipCheckoutRoute: %v", err)
	}

	overlay, err := catalog.CreateViewGeneration(ctx, ViewGeneration{
		OwnerKind:        "checkout",
		GraphID:          "graph-1",
		CheckoutID:       "wt",
		GenerationKind:   "dirty",
		BaseGenerationID: base,
		State:            ViewGenerationBuilding,
		CreatedAt:        601,
	})
	if err != nil {
		t.Fatalf("CreateViewGeneration overlay: %v", err)
	}
	if err := catalog.DeleteViewGeneration(ctx, base); !errors.Is(err, ErrCatalogGenerationReferenced) {
		t.Fatalf("delete under a base pointer = %v, want ErrCatalogGenerationReferenced", err)
	}
	if err := catalog.DeleteViewGeneration(ctx, overlay); err != nil {
		t.Fatalf("DeleteViewGeneration overlay: %v", err)
	}

	if err := catalog.UpsertRefView(ctx, RefView{
		RefViewID:          "rv-1",
		GraphID:            "graph-1",
		SelectorKind:       "branch",
		SelectorValue:      "main",
		DesiredRef:         "refs/heads/main",
		DesiredTree:        "7ee7",
		ActiveGenerationID: base,
		EnrichmentProfile:  "default",
		State:              RefViewReady,
		ExactView:          true,
	}); err != nil {
		t.Fatalf("UpsertRefView: %v", err)
	}
	if err := catalog.DeleteViewGeneration(ctx, base); !errors.Is(err, ErrCatalogGenerationReferenced) {
		t.Fatalf("delete under a ref view = %v, want ErrCatalogGenerationReferenced", err)
	}
	if _, err := store.writerDB.ExecContext(ctx, `DELETE FROM ref_views WHERE ref_view_id = ?`, "rv-1"); err != nil {
		t.Fatalf("drop ref view: %v", err)
	}

	if err := catalog.UpsertDedicatedGraph(ctx, DedicatedGraph{
		GraphID:            "graph-1",
		OwnerCheckoutID:    "wt",
		RepoPrefix:         "wt-prefix",
		FamilyID:           "fam",
		ActiveGenerationID: base,
		State:              "graph_ready",
	}); err != nil {
		t.Fatalf("UpsertDedicatedGraph: %v", err)
	}
	if err := catalog.DeleteViewGeneration(ctx, base); !errors.Is(err, ErrCatalogGenerationReferenced) {
		t.Fatalf("delete under a dedicated graph pointer = %v, want ErrCatalogGenerationReferenced", err)
	}
	if err := catalog.UpsertDedicatedGraph(ctx, DedicatedGraph{
		GraphID:         "graph-1",
		OwnerCheckoutID: "wt",
		RepoPrefix:      "wt-prefix",
		FamilyID:        "fam",
		State:           "graph_ready",
	}); err != nil {
		t.Fatalf("clear dedicated graph pointer: %v", err)
	}

	if err := catalog.DeleteViewGeneration(ctx, base); err != nil {
		t.Fatalf("DeleteViewGeneration once unreferenced: %v", err)
	}
	if _, ok, err := catalog.GetViewGeneration(ctx, base); err != nil || ok {
		t.Fatalf("generation still present after delete: %v, %v", ok, err)
	}
}

// TestCatalogPrimaryBaseIsUniquePerFamily proves the partial unique index: a
// family may hold at most one primary base, but each family holds its own.
func TestCatalogPrimaryBaseIsUniquePerFamily(t *testing.T) {
	store := openCatalogStore(t)
	ctx := context.Background()
	catalog := store.Catalog()
	seedFamilyAndCheckout(t, catalog, "fam-a", "wt-a", "inc-1")
	seedFamilyAndCheckout(t, catalog, "fam-b", "wt-b", "inc-1")

	primaryA := DedicatedGraph{
		GraphID: "graph-a1", OwnerCheckoutID: "wt-a", RepoPrefix: "prefix-a1",
		FamilyID: "fam-a", IsPrimaryBase: true, State: "graph_ready",
	}
	if err := catalog.UpsertDedicatedGraph(ctx, primaryA); err != nil {
		t.Fatalf("first primary in fam-a: %v", err)
	}

	second := DedicatedGraph{
		GraphID: "graph-a2", RepoPrefix: "prefix-a2",
		FamilyID: "fam-a", IsPrimaryBase: true, State: "graph_ready",
	}
	if err := catalog.UpsertDedicatedGraph(ctx, second); err == nil {
		t.Fatal("a second primary base in one family must be rejected")
	} else if !strings.Contains(strings.ToLower(err.Error()), "unique") {
		t.Fatalf("second primary rejection = %v, want a uniqueness failure", err)
	}

	// A non-primary sibling in the same family is fine — the index is partial.
	second.IsPrimaryBase = false
	if err := catalog.UpsertDedicatedGraph(ctx, second); err != nil {
		t.Fatalf("non-primary sibling in fam-a: %v", err)
	}

	// And each family gets its own primary.
	if err := catalog.UpsertDedicatedGraph(ctx, DedicatedGraph{
		GraphID: "graph-b1", OwnerCheckoutID: "wt-b", RepoPrefix: "prefix-b1",
		FamilyID: "fam-b", IsPrimaryBase: true, State: "graph_ready",
	}); err != nil {
		t.Fatalf("first primary in fam-b: %v", err)
	}
	for _, id := range []string{"graph-a1", "graph-b1"} {
		dedicated, ok, err := catalog.GetDedicatedGraph(ctx, id)
		if err != nil || !ok {
			t.Fatalf("GetDedicatedGraph(%s) = %v, %v, %v", id, dedicated, ok, err)
		}
		if !dedicated.IsPrimaryBase {
			t.Fatalf("%s should be its family's primary base: %+v", id, dedicated)
		}
	}
}

// TestCatalogIncarnationGuardRejectsStaleWrite proves a checkout state write
// aimed at a previous incarnation of a recreated working copy changes nothing.
func TestCatalogIncarnationGuardRejectsStaleWrite(t *testing.T) {
	store := openCatalogStore(t)
	ctx := context.Background()
	catalog := store.Catalog()
	seedFamilyAndCheckout(t, catalog, "fam", "wt", "inc-2")

	stale := UpdateCheckoutStateRequest{
		CheckoutID: "wt", Incarnation: "inc-1",
		State:       CheckoutStateUnavailable,
		DesiredMode: CheckoutModeDedicated, EffectiveMode: CheckoutModeDedicated,
		LastSeen: 700, LastError: "stale writer",
	}
	if err := catalog.UpdateCheckoutState(ctx, stale); !errors.Is(err, ErrCatalogStaleGuard) {
		t.Fatalf("stale incarnation write = %v, want ErrCatalogStaleGuard", err)
	}
	checkout, ok, err := catalog.GetCheckout(ctx, "wt")
	if err != nil || !ok {
		t.Fatalf("GetCheckout = %v, %v, %v", checkout, ok, err)
	}
	if checkout.State != CheckoutStateReady || checkout.LastError != "" || checkout.LastSeen != 101 {
		t.Fatalf("a rejected guard must change nothing: %+v", checkout)
	}

	current := stale
	current.Incarnation = "inc-2"
	current.State = CheckoutStateReconciling
	current.LastError = ""
	if err := catalog.UpdateCheckoutState(ctx, current); err != nil {
		t.Fatalf("current incarnation write: %v", err)
	}
	checkout, _, err = catalog.GetCheckout(ctx, "wt")
	if err != nil {
		t.Fatalf("GetCheckout: %v", err)
	}
	if checkout.State != CheckoutStateReconciling || checkout.EffectiveMode != CheckoutModeDedicated || checkout.LastSeen != 700 {
		t.Fatalf("accepted guard did not apply: %+v", checkout)
	}

	// A value outside the vocabulary never reaches SQL.
	bad := current
	bad.State = CheckoutState("checkout_teleporting")
	if err := catalog.UpdateCheckoutState(ctx, bad); !errors.Is(err, ErrCatalogInvalidValue) {
		t.Fatalf("out-of-vocabulary state = %v, want ErrCatalogInvalidValue", err)
	}
}

// TestCatalogRouteEpochCASRejectsStaleFlip proves the route compare-and-set:
// the first flip wins and bumps the epoch, a second flip replaying the old
// epoch changes nothing.
func TestCatalogRouteEpochCASRejectsStaleFlip(t *testing.T) {
	store := openCatalogStore(t)
	ctx := context.Background()
	catalog := store.Catalog()
	seedFamilyAndCheckout(t, catalog, "fam", "wt", "inc-1")

	first := seedBuildingGeneration(t, catalog, "graph-1")
	second := seedBuildingGeneration(t, catalog, "graph-1")
	if err := catalog.UpsertCheckoutRoute(ctx, CheckoutRoute{
		CheckoutID: "wt", GraphID: "graph-1", CommitGenerationID: first,
		RouteEpoch: 0, State: RouteActive,
	}); err != nil {
		t.Fatalf("UpsertCheckoutRoute: %v", err)
	}

	flip := FlipCheckoutRouteRequest{
		CheckoutID: "wt", ExpectedRouteEpoch: 0, GraphID: "graph-1",
		CommitGenerationID: second, State: RouteActive,
	}
	if err := catalog.FlipCheckoutRoute(ctx, flip); err != nil {
		t.Fatalf("first flip: %v", err)
	}
	route, ok, err := catalog.GetCheckoutRoute(ctx, "wt")
	if err != nil || !ok {
		t.Fatalf("GetCheckoutRoute = %v, %v, %v", route, ok, err)
	}
	if route.RouteEpoch != 1 || route.CommitGenerationID != second {
		t.Fatalf("first flip result = %+v", route)
	}

	// A concurrent reconciler replaying the pre-flip epoch must lose.
	replay := flip
	replay.CommitGenerationID = first
	if err := catalog.FlipCheckoutRoute(ctx, replay); !errors.Is(err, ErrCatalogStaleGuard) {
		t.Fatalf("stale route flip = %v, want ErrCatalogStaleGuard", err)
	}
	route, _, err = catalog.GetCheckoutRoute(ctx, "wt")
	if err != nil {
		t.Fatalf("GetCheckoutRoute: %v", err)
	}
	if route.RouteEpoch != 1 || route.CommitGenerationID != second {
		t.Fatalf("a rejected flip must change nothing: %+v", route)
	}

	// Clearing both pointers is expressible and still bumps the epoch.
	clear := FlipCheckoutRouteRequest{
		CheckoutID: "wt", ExpectedRouteEpoch: 1, GraphID: "graph-1", State: RoutePending,
	}
	if err := catalog.FlipCheckoutRoute(ctx, clear); err != nil {
		t.Fatalf("clearing flip: %v", err)
	}
	route, _, err = catalog.GetCheckoutRoute(ctx, "wt")
	if err != nil {
		t.Fatalf("GetCheckoutRoute: %v", err)
	}
	if route.RouteEpoch != 2 || route.CommitGenerationID != 0 || route.State != RoutePending {
		t.Fatalf("cleared route = %+v", route)
	}
}

// TestCatalogPrimaryEpochCASRejectsStaleFlip proves the primary-base
// compare-and-set on the family row, and that promoting a second graph moves
// the flag rather than colliding with the partial unique index.
func TestCatalogPrimaryEpochCASRejectsStaleFlip(t *testing.T) {
	store := openCatalogStore(t)
	ctx := context.Background()
	catalog := store.Catalog()
	seedFamilyAndCheckout(t, catalog, "fam", "wt", "inc-1")

	for _, spec := range []struct{ graphID, prefix string }{
		{"graph-1", "prefix-1"},
		{"graph-2", "prefix-2"},
	} {
		if err := catalog.UpsertDedicatedGraph(ctx, DedicatedGraph{
			GraphID: spec.graphID, RepoPrefix: spec.prefix,
			FamilyID: "fam", State: "graph_ready",
		}); err != nil {
			t.Fatalf("UpsertDedicatedGraph(%s): %v", spec.graphID, err)
		}
	}

	promote := SetPrimaryDedicatedGraphRequest{
		FamilyID: "fam", GraphID: "graph-1", ExpectedPrimaryEpoch: 0, LastSeen: 800,
	}
	if err := catalog.SetPrimaryDedicatedGraph(ctx, promote); err != nil {
		t.Fatalf("first promotion: %v", err)
	}
	family, _, err := catalog.GetRepositoryFamily(ctx, "fam")
	if err != nil {
		t.Fatalf("GetRepositoryFamily: %v", err)
	}
	if family.PrimaryEpoch != 1 {
		t.Fatalf("primary epoch = %d, want 1", family.PrimaryEpoch)
	}

	// Replaying the pre-promotion epoch must change nothing at all.
	replay := promote
	replay.GraphID = "graph-2"
	if err := catalog.SetPrimaryDedicatedGraph(ctx, replay); !errors.Is(err, ErrCatalogStaleGuard) {
		t.Fatalf("stale primary flip = %v, want ErrCatalogStaleGuard", err)
	}
	first, _, err := catalog.GetDedicatedGraph(ctx, "graph-1")
	if err != nil {
		t.Fatalf("GetDedicatedGraph: %v", err)
	}
	if !first.IsPrimaryBase {
		t.Fatalf("a rejected primary flip must leave the incumbent: %+v", first)
	}
	second, _, err := catalog.GetDedicatedGraph(ctx, "graph-2")
	if err != nil {
		t.Fatalf("GetDedicatedGraph: %v", err)
	}
	if second.IsPrimaryBase {
		t.Fatalf("a rejected primary flip must not promote: %+v", second)
	}

	// With the current epoch the flag moves: the incumbent is cleared inside
	// the same transaction, so the partial unique index is never violated.
	move := SetPrimaryDedicatedGraphRequest{
		FamilyID: "fam", GraphID: "graph-2", ExpectedPrimaryEpoch: 1, LastSeen: 801,
	}
	if err := catalog.SetPrimaryDedicatedGraph(ctx, move); err != nil {
		t.Fatalf("moving the primary base: %v", err)
	}
	first, _, err = catalog.GetDedicatedGraph(ctx, "graph-1")
	if err != nil {
		t.Fatalf("GetDedicatedGraph: %v", err)
	}
	second, _, err = catalog.GetDedicatedGraph(ctx, "graph-2")
	if err != nil {
		t.Fatalf("GetDedicatedGraph: %v", err)
	}
	if first.IsPrimaryBase || !second.IsPrimaryBase {
		t.Fatalf("primary base did not move: %+v / %+v", first, second)
	}

	// Promoting a graph that is not in the family leaves nothing behind.
	if err := catalog.SetPrimaryDedicatedGraph(ctx, SetPrimaryDedicatedGraphRequest{
		FamilyID: "fam", GraphID: "graph-missing", ExpectedPrimaryEpoch: 2, LastSeen: 802,
	}); !errors.Is(err, ErrCatalogNotFound) {
		t.Fatalf("promoting an unknown graph = %v, want ErrCatalogNotFound", err)
	}
	family, _, err = catalog.GetRepositoryFamily(ctx, "fam")
	if err != nil {
		t.Fatalf("GetRepositoryFamily: %v", err)
	}
	if family.PrimaryEpoch != 2 {
		t.Fatalf("a rolled-back promotion must not bump the epoch: %d", family.PrimaryEpoch)
	}
}

// TestCatalogGenerationIsImmutableOnceReady proves publish is a one-way
// building -> ready transition: a second publish, and any publish of a
// generation that never was building, are refused.
func TestCatalogGenerationIsImmutableOnceReady(t *testing.T) {
	store := openCatalogStore(t)
	ctx := context.Background()
	catalog := store.Catalog()

	generationID := seedBuildingGeneration(t, catalog, "graph-1")
	if err := catalog.PublishViewGeneration(ctx, generationID, 900); err != nil {
		t.Fatalf("PublishViewGeneration: %v", err)
	}
	published, ok, err := catalog.GetViewGeneration(ctx, generationID)
	if err != nil || !ok {
		t.Fatalf("GetViewGeneration = %v, %v, %v", published, ok, err)
	}
	if published.State != ViewGenerationReady || published.PublishedAt != 900 {
		t.Fatalf("published generation = %+v", published)
	}

	if err := catalog.PublishViewGeneration(ctx, generationID, 901); !errors.Is(err, ErrCatalogStaleGuard) {
		t.Fatalf("republish = %v, want ErrCatalogStaleGuard", err)
	}
	republished, _, err := catalog.GetViewGeneration(ctx, generationID)
	if err != nil {
		t.Fatalf("GetViewGeneration: %v", err)
	}
	if republished.PublishedAt != 900 {
		t.Fatalf("a ready generation must be immutable: %+v", republished)
	}

	failed, err := catalog.CreateViewGeneration(ctx, ViewGeneration{
		OwnerKind: "dedicated_graph", GraphID: "graph-1", GenerationKind: "commit",
		State: ViewGenerationFailed, CreatedAt: 902, Error: "extractor crashed",
	})
	if err != nil {
		t.Fatalf("CreateViewGeneration failed-state: %v", err)
	}
	if err := catalog.PublishViewGeneration(ctx, failed, 903); !errors.Is(err, ErrCatalogStaleGuard) {
		t.Fatalf("publish of a failed generation = %v, want ErrCatalogStaleGuard", err)
	}
}

// TestCatalogRefViewSelectorAndBuildCoalescing proves the two ref-view
// constraints: one row per (graph, selector, profile), and one in-flight build
// per (ref view, tree, base, fingerprint).
func TestCatalogRefViewSelectorAndBuildCoalescing(t *testing.T) {
	store := openCatalogStore(t)
	ctx := context.Background()
	catalog := store.Catalog()

	base := seedBuildingGeneration(t, catalog, "graph-1")
	view := RefView{
		RefViewID: "rv-1", GraphID: "graph-1", SelectorKind: "branch", SelectorValue: "main",
		DesiredRef: "refs/heads/main", DesiredCommit: "c0ffee", DesiredTree: "7ee7",
		EnrichmentProfile: "default", DesiredBuildFingerprint: "fp-1",
		State: RefViewPending, ExactView: true, LastResolved: 1000,
	}
	if err := catalog.UpsertRefView(ctx, view); err != nil {
		t.Fatalf("UpsertRefView: %v", err)
	}

	duplicate := view
	duplicate.RefViewID = "rv-2"
	if err := catalog.UpsertRefView(ctx, duplicate); err == nil {
		t.Fatal("a second row for the same selector and profile must be rejected")
	} else if !strings.Contains(strings.ToLower(err.Error()), "unique") {
		t.Fatalf("duplicate selector rejection = %v, want a uniqueness failure", err)
	}

	// A different enrichment profile is a different view of the same selector.
	otherProfile := view
	otherProfile.RefViewID = "rv-3"
	otherProfile.EnrichmentProfile = "deep"
	if err := catalog.UpsertRefView(ctx, otherProfile); err != nil {
		t.Fatalf("second profile for the same selector: %v", err)
	}

	build := RefViewBuild{
		BuildID: "build-1", RefViewID: "rv-1", DesiredRef: "refs/heads/main",
		DesiredCommit: "c0ffee", DesiredTree: "7ee7", BaseGenerationID: base,
		EnrichmentProfile: "default", BuildFingerprint: "fp-1",
		CapturedRouteEpoch: 3, State: ViewGenerationBuilding,
		BuildToken: "token-1", CreatedAt: 1001,
	}
	if err := catalog.UpsertRefViewBuild(ctx, build); err != nil {
		t.Fatalf("UpsertRefViewBuild: %v", err)
	}

	racing := build
	racing.BuildID = "build-2"
	racing.BuildToken = "token-2"
	if err := catalog.UpsertRefViewBuild(ctx, racing); err == nil {
		t.Fatal("a second in-flight build for the same work must be rejected")
	} else if !strings.Contains(strings.ToLower(err.Error()), "unique") {
		t.Fatalf("coalescing rejection = %v, want a uniqueness failure", err)
	}

	// Once the first attempt leaves the building state the slot is free again.
	finished := build
	finished.State = ViewGenerationReady
	finished.LastProgress = 1002
	if err := catalog.UpsertRefViewBuild(ctx, finished); err != nil {
		t.Fatalf("finish first build: %v", err)
	}
	if err := catalog.UpsertRefViewBuild(ctx, racing); err != nil {
		t.Fatalf("retry after the first build finished: %v", err)
	}

	stored, ok, err := catalog.GetRefViewBuild(ctx, "build-1")
	if err != nil || !ok {
		t.Fatalf("GetRefViewBuild = %v, %v, %v", stored, ok, err)
	}
	if stored.State != ViewGenerationReady || stored.BaseGenerationID != base || stored.CapturedRouteEpoch != 3 {
		t.Fatalf("build round trip = %+v", stored)
	}

	// Deleting a ref view takes its builds with it.
	if _, err := store.writerDB.ExecContext(ctx, `DELETE FROM ref_views WHERE ref_view_id = ?`, "rv-1"); err != nil {
		t.Fatalf("delete ref view: %v", err)
	}
	if stored, ok, err := catalog.GetRefViewBuild(ctx, "build-1"); err != nil || ok {
		t.Fatalf("builds survived their ref view: %+v, %v, %v", stored, ok, err)
	}
	if stored, ok, err := catalog.GetRefView(ctx, "rv-3"); err != nil || !ok || stored.EnrichmentProfile != "deep" {
		t.Fatalf("unrelated ref view = %+v, %v, %v", stored, ok, err)
	}
}

// TestCatalogIntentTransitionIsSinglePerCheckout proves the UNIQUE(checkout_id)
// contract: one transition at a time, the checkout points at it while it is in
// flight, completing it frees the slot, and a stale completion is refused.
func TestCatalogIntentTransitionIsSinglePerCheckout(t *testing.T) {
	store := openCatalogStore(t)
	ctx := context.Background()
	catalog := store.Catalog()
	seedFamilyAndCheckout(t, catalog, "fam", "wt", "inc-1")

	first := IntentTransition{
		TransitionID: "trans-1", CheckoutID: "wt", Cause: "user_requested_dedicated",
		PriorDesiredMode: CheckoutModeAutomatic, PriorEffectiveMode: CheckoutModeAutomatic,
		RequestedMode: CheckoutModeDedicated, PriorCheckoutState: CheckoutStateReady,
		SourceSnapshotHash: "hash-1", State: IntentTransitionRunning, CreatedAt: 1100,
	}
	if err := catalog.BeginIntentTransition(ctx, first); err != nil {
		t.Fatalf("BeginIntentTransition: %v", err)
	}
	checkout, _, err := catalog.GetCheckout(ctx, "wt")
	if err != nil {
		t.Fatalf("GetCheckout: %v", err)
	}
	if checkout.ActiveIntentTransitionID != "trans-1" {
		t.Fatalf("checkout does not point at its transition: %+v", checkout)
	}

	second := first
	second.TransitionID = "trans-2"
	if err := catalog.BeginIntentTransition(ctx, second); !errors.Is(err, ErrCatalogIntentTransitionActive) {
		t.Fatalf("second transition = %v, want ErrCatalogIntentTransitionActive", err)
	}
	stored, ok, err := catalog.GetIntentTransition(ctx, "wt")
	if err != nil || !ok {
		t.Fatalf("GetIntentTransition = %v, %v, %v", stored, ok, err)
	}
	if stored.TransitionID != "trans-1" || stored.State != IntentTransitionRunning ||
		stored.RequestedMode != CheckoutModeDedicated || stored.SourceSnapshotHash != "hash-1" {
		t.Fatalf("transition round trip = %+v", stored)
	}

	if err := catalog.CompleteIntentTransition(ctx, "wt", "trans-2"); !errors.Is(err, ErrCatalogStaleGuard) {
		t.Fatalf("completing someone else's transition = %v, want ErrCatalogStaleGuard", err)
	}
	if _, ok, err := catalog.GetIntentTransition(ctx, "wt"); err != nil || !ok {
		t.Fatalf("a rejected completion must leave the transition: %v, %v", ok, err)
	}

	if err := catalog.CompleteIntentTransition(ctx, "wt", "trans-1"); err != nil {
		t.Fatalf("CompleteIntentTransition: %v", err)
	}
	if stored, ok, err := catalog.GetIntentTransition(ctx, "wt"); err != nil || ok {
		t.Fatalf("completed transition still present: %+v, %v, %v", stored, ok, err)
	}
	checkout, _, err = catalog.GetCheckout(ctx, "wt")
	if err != nil {
		t.Fatalf("GetCheckout: %v", err)
	}
	if checkout.ActiveIntentTransitionID != "" {
		t.Fatalf("completion did not clear the checkout pointer: %+v", checkout)
	}

	// The slot is genuinely free again.
	if err := catalog.BeginIntentTransition(ctx, second); err != nil {
		t.Fatalf("transition after completion: %v", err)
	}

	// A transition for a checkout that does not exist rolls back whole.
	orphan := first
	orphan.TransitionID = "trans-3"
	orphan.CheckoutID = "missing"
	if err := catalog.BeginIntentTransition(ctx, orphan); err == nil {
		t.Fatal("a transition for an unknown checkout must be refused")
	}
	if _, ok, err := catalog.GetIntentTransition(ctx, "missing"); err != nil || ok {
		t.Fatalf("rolled-back transition left a row: %v, %v", ok, err)
	}
}

// TestCatalogWriteValidationRejectsUnknownVocabulary proves every typed state
// column is checked in Go before it reaches SQL, and that the required
// identifiers cannot be empty.
func TestCatalogWriteValidationRejectsUnknownVocabulary(t *testing.T) {
	store := openCatalogStore(t)
	ctx := context.Background()
	catalog := store.Catalog()
	seedFamilyAndCheckout(t, catalog, "fam", "wt", "inc-1")

	cases := []struct {
		name string
		call func() error
	}{
		{"family without id", func() error {
			return catalog.UpsertRepositoryFamily(ctx, RepositoryFamily{CommonDirIdentity: "x", State: "s"})
		}},
		{"checkout state", func() error {
			return catalog.UpsertCheckout(ctx, Checkout{
				CheckoutID: "x", Incarnation: "i", FamilyID: "fam", State: CheckoutState("nope"),
				DesiredMode: CheckoutModeAutomatic, EffectiveMode: CheckoutModeAutomatic,
			})
		}},
		{"checkout mode", func() error {
			return catalog.UpsertCheckout(ctx, Checkout{
				CheckoutID: "x", Incarnation: "i", FamilyID: "fam", State: CheckoutStateDemoting,
				DesiredMode: CheckoutMode("hybrid"), EffectiveMode: CheckoutModeAutomatic,
			})
		}},
		{"intent source kind", func() error {
			return catalog.UpsertTrackingIntent(ctx, TrackingIntent{
				IntentID: "i", CheckoutID: "wt", SourceKind: IntentSourceKind("telepathy"), SourceLocator: "l",
			})
		}},
		{"transition state", func() error {
			return catalog.BeginIntentTransition(ctx, IntentTransition{
				TransitionID: "t", CheckoutID: "wt", Cause: "c", State: IntentTransitionState("done"),
			})
		}},
		{"transition prior mode", func() error {
			return catalog.BeginIntentTransition(ctx, IntentTransition{
				TransitionID: "t", CheckoutID: "wt", Cause: "c", State: IntentTransitionPending,
				PriorDesiredMode: CheckoutMode("hybrid"),
			})
		}},
		{"generation state", func() error {
			_, err := catalog.CreateViewGeneration(ctx, ViewGeneration{
				OwnerKind: "o", GenerationKind: "k", State: ViewGenerationState("published"),
			})
			return err
		}},
		{"route state", func() error {
			return catalog.UpsertCheckoutRoute(ctx, CheckoutRoute{
				CheckoutID: "wt", GraphID: "g", State: RouteState("live"),
			})
		}},
		{"ref view state", func() error {
			return catalog.UpsertRefView(ctx, RefView{
				RefViewID: "rv", GraphID: "g", SelectorKind: "branch", SelectorValue: "main",
				EnrichmentProfile: "default", State: RefViewState("warm"),
			})
		}},
		{"build state", func() error {
			return catalog.UpsertRefViewBuild(ctx, RefViewBuild{
				BuildID: "b", RefViewID: "rv", BuildFingerprint: "fp", BuildToken: "tok",
				State: ViewGenerationState("queued"),
			})
		}},
		{"cleanup phase", func() error {
			return catalog.UpsertCleanupEntry(ctx, CleanupEntry{
				CleanupID: "c", OpaqueTargetIDs: "t", Reason: "r", Phase: CleanupPhase("soon"),
			})
		}},
		{"layer without kind", func() error {
			return catalog.UpsertViewLayer(ctx, ViewLayer{LayerID: "l", GraphID: "g"})
		}},
		{"dedicated graph without prefix", func() error {
			return catalog.UpsertDedicatedGraph(ctx, DedicatedGraph{
				GraphID: "g", FamilyID: "fam", State: "graph_ready",
			})
		}},
		{"publish of a non-generation", func() error {
			return catalog.PublishViewGeneration(ctx, 0, 1)
		}},
		{"delete of a non-generation", func() error {
			return catalog.DeleteViewGeneration(ctx, -1)
		}},
	}
	for _, tc := range cases {
		if err := tc.call(); !errors.Is(err, ErrCatalogInvalidValue) {
			t.Errorf("%s: err = %v, want ErrCatalogInvalidValue", tc.name, err)
		}
	}

	// Nothing above should have written a row.
	for _, table := range []string{"checkouts", "tracking_intents", "intent_transitions",
		"view_generations", "checkout_routes", "ref_views", "ref_view_builds",
		"cleanup_journal", "view_layers", "dedicated_graphs"} {
		var count int
		if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		want := 0
		if table == "checkouts" {
			want = 1 // the seeded checkout
		}
		if count != want {
			t.Errorf("%s holds %d rows after rejected writes, want %d", table, count, want)
		}
	}

	// The valid vocabulary values all round-trip, so the validators are not
	// simply refusing everything.
	for _, state := range []CheckoutState{
		CheckoutStateReady, CheckoutStateAvailabilityGrace, CheckoutStateUnavailable,
		CheckoutStateReconciling, CheckoutStateDemoting, CheckoutStateForgetting,
		CheckoutStatePrimaryClosureRetiring,
	} {
		if err := catalog.UpdateCheckoutState(ctx, UpdateCheckoutStateRequest{
			CheckoutID: "wt", Incarnation: "inc-1", State: state,
			DesiredMode: CheckoutModeDedicated, EffectiveMode: CheckoutModeAutomatic,
			LastSeen: 1200,
		}); err != nil {
			t.Errorf("state %q rejected: %v", state, err)
		}
	}
	for _, phase := range []CleanupPhase{
		CleanupPhasePending, CleanupPhaseGrace, CleanupPhaseDeleting,
		CleanupPhaseDone, CleanupPhaseFailed,
	} {
		if err := catalog.UpsertCleanupEntry(ctx, CleanupEntry{
			CleanupID: "c-" + string(phase), OpaqueTargetIDs: "t", Reason: "r", Phase: phase,
		}); err != nil {
			t.Errorf("phase %q rejected: %v", phase, err)
		}
	}
	for _, state := range []RefViewState{
		RefViewPending, RefViewBuilding, RefViewReady, RefViewStale, RefViewFailed,
	} {
		if err := catalog.UpsertRefView(ctx, RefView{
			RefViewID: "rv-" + string(state), GraphID: "g", SelectorKind: "branch",
			SelectorValue: string(state), EnrichmentProfile: "default", State: state,
		}); err != nil {
			t.Errorf("ref view state %q rejected: %v", state, err)
		}
	}
	for _, state := range []RouteState{RoutePending, RouteActive, RouteRetired} {
		if err := catalog.UpsertCheckoutRoute(ctx, CheckoutRoute{
			CheckoutID: "wt", GraphID: "g", State: state,
		}); err != nil {
			t.Errorf("route state %q rejected: %v", state, err)
		}
	}
	for _, state := range []ViewGenerationState{
		ViewGenerationBuilding, ViewGenerationReady, ViewGenerationSuperseded,
		ViewGenerationRetiring, ViewGenerationFailed,
	} {
		if _, err := catalog.CreateViewGeneration(ctx, ViewGeneration{
			OwnerKind: "dedicated_graph", GraphID: "g", GenerationKind: "commit", State: state,
		}); err != nil {
			t.Errorf("generation state %q rejected: %v", state, err)
		}
	}
	for _, kind := range []IntentSourceKind{
		IntentSourceCLITrack, IntentSourceMCPTrack, IntentSourceManualConfig,
		IntentSourceProjectMembership,
	} {
		if err := catalog.UpsertTrackingIntent(ctx, TrackingIntent{
			IntentID: "i-" + string(kind), CheckoutID: "wt", SourceKind: kind,
			SourceLocator: string(kind), Active: true,
		}); err != nil {
			t.Errorf("intent source %q rejected: %v", kind, err)
		}
	}
	for _, state := range []IntentTransitionState{
		IntentTransitionPending, IntentTransitionRunning, IntentTransitionFailed,
	} {
		transition := IntentTransition{
			TransitionID: "t-" + string(state), CheckoutID: "wt", Cause: "c", State: state,
		}
		if err := catalog.BeginIntentTransition(ctx, transition); err != nil {
			t.Errorf("transition state %q rejected: %v", state, err)
			continue
		}
		if err := catalog.CompleteIntentTransition(ctx, "wt", transition.TransitionID); err != nil {
			t.Errorf("completing %q: %v", state, err)
		}
	}
}

// TestCatalogObservationWriteMovesBothClocks covers the guarded write a
// reconciliation pass makes. The two clock axes are the point: they are stored
// columns, so they have to survive a restart, and they have to move
// independently of one another.
func TestCatalogObservationWriteMovesBothClocks(t *testing.T) {
	store := openCatalogStore(t)
	ctx := context.Background()
	catalog := store.Catalog()
	seedFamilyAndCheckout(t, catalog, "fam", "wt", "inc-1")

	req := UpdateCheckoutObservationRequest{
		CheckoutID:           "wt",
		Incarnation:          "inc-1",
		State:                CheckoutStateAvailabilityGrace,
		RootPath:             "/moved/wt",
		GitDir:               "/moved/wt/.git",
		Locked:               true,
		Prunable:             true,
		HeadRef:              "refs/heads/feature",
		HeadCommit:           "beef",
		HeadTree:             "cafe",
		LastAccessible:       10,
		UnavailableSince:     20,
		AvailabilityDeadline: 30,
		RemovalDetectedAt:    40,
		RemovalDeadline:      50,
		RemovalEvidence:      "evidence_prunable_confirmed",
		LastSeen:             60,
		LastError:            "volume detached",
	}
	if err := catalog.UpdateCheckoutObservation(ctx, req); err != nil {
		t.Fatalf("UpdateCheckoutObservation: %v", err)
	}
	checkout, ok, err := catalog.GetCheckout(ctx, "wt")
	if err != nil || !ok {
		t.Fatalf("GetCheckout = %v %v", ok, err)
	}
	if checkout.State != CheckoutStateAvailabilityGrace || checkout.RootPath != "/moved/wt" {
		t.Fatalf("state columns = %+v", checkout)
	}
	if !checkout.Locked || !checkout.Prunable || checkout.HeadTree != "cafe" {
		t.Fatalf("observed facts = %+v", checkout)
	}
	if checkout.UnavailableSince != 20 || checkout.AvailabilityDeadline != 30 {
		t.Fatalf("availability clock = (%d, %d)", checkout.UnavailableSince, checkout.AvailabilityDeadline)
	}
	if checkout.RemovalDetectedAt != 40 || checkout.RemovalDeadline != 50 {
		t.Fatalf("removal clock = (%d, %d)", checkout.RemovalDetectedAt, checkout.RemovalDeadline)
	}
	if checkout.RemovalEvidence != "evidence_prunable_confirmed" || checkout.LastError != "volume detached" {
		t.Fatalf("evidence columns = %+v", checkout)
	}
	// The identity columns are not the observation's to change.
	if checkout.Incarnation != "inc-1" || checkout.AdminName != "wt" || checkout.FamilyID != "fam" {
		t.Fatalf("an observation re-keyed the row: %+v", checkout)
	}

	stale := req
	stale.Incarnation = "inc-0"
	stale.State = CheckoutStateUnavailable
	if err := catalog.UpdateCheckoutObservation(ctx, stale); !errors.Is(err, ErrCatalogStaleGuard) {
		t.Fatalf("stale observation = %v, want ErrCatalogStaleGuard", err)
	}
	if again, _, _ := catalog.GetCheckout(ctx, "wt"); again.State != CheckoutStateAvailabilityGrace {
		t.Fatalf("a stale observation advanced the state to %q", again.State)
	}

	for name, bad := range map[string]UpdateCheckoutObservationRequest{
		"no checkout id": {Incarnation: "inc-1", State: CheckoutStateReady},
		"no incarnation": {CheckoutID: "wt", State: CheckoutStateReady},
		"unknown state":  {CheckoutID: "wt", Incarnation: "inc-1", State: "invented"},
	} {
		if err := catalog.UpdateCheckoutObservation(ctx, bad); !errors.Is(err, ErrCatalogInvalidValue) {
			t.Errorf("%s = %v, want ErrCatalogInvalidValue", name, err)
		}
	}
}

// TestCatalogObservationLeavesTheModeAxisAlone pins the column split between
// the two writers on a checkout row. A mode transition that commits between an
// observation's read and its write must survive: the incarnation guard does
// not move on a promotion, so the observation write is only safe as long as it
// names no mode column.
func TestCatalogObservationLeavesTheModeAxisAlone(t *testing.T) {
	store := openCatalogStore(t)
	ctx := context.Background()
	catalog := store.Catalog()
	seedFamilyAndCheckout(t, catalog, "fam", "wt", "inc-1")

	// What a pass reads at its start: the checkout is served automatically.
	observed, ok, err := catalog.GetCheckout(ctx, "wt")
	if err != nil || !ok {
		t.Fatalf("GetCheckout = %v %v", ok, err)
	}
	if observed.EffectiveMode != CheckoutModeAutomatic {
		t.Fatalf("seeded mode = %q", observed.EffectiveMode)
	}

	// A promotion commits while the pass is still deciding.
	err = catalog.UpdateCheckoutState(ctx, UpdateCheckoutStateRequest{
		CheckoutID:    "wt",
		Incarnation:   "inc-1",
		State:         CheckoutStateReconciling,
		DesiredMode:   CheckoutModeDedicated,
		EffectiveMode: CheckoutModeDedicated,
	})
	if err != nil {
		t.Fatalf("UpdateCheckoutState: %v", err)
	}

	// The pass writes what it observed, under an incarnation that is still
	// current, so the write lands.
	err = catalog.UpdateCheckoutObservation(ctx, UpdateCheckoutObservationRequest{
		CheckoutID:     "wt",
		Incarnation:    "inc-1",
		State:          CheckoutStateReady,
		RootPath:       observed.RootPath,
		GitDir:         observed.GitDir,
		LastAccessible: 77,
		LastSeen:       77,
	})
	if err != nil {
		t.Fatalf("UpdateCheckoutObservation: %v", err)
	}

	after, _, err := catalog.GetCheckout(ctx, "wt")
	if err != nil {
		t.Fatalf("GetCheckout: %v", err)
	}
	if after.DesiredMode != CheckoutModeDedicated || after.EffectiveMode != CheckoutModeDedicated {
		t.Fatalf("an observation reverted the promotion: %q/%q", after.DesiredMode, after.EffectiveMode)
	}
	if after.State != CheckoutStateReady || after.LastAccessible != 77 {
		t.Fatalf("the observation itself did not land: %+v", after)
	}
}

// TestCatalogAllocateCheckoutRefusesASecondIdentity covers the allocation-time
// backstop. The table's UNIQUE key spans the incarnation, so it happily takes
// a second live row for one (family_id, admin_name); the guarded insert is
// what does not, and that is the only thing standing between two racing
// allocators and a family with two identities for one working copy.
func TestCatalogAllocateCheckoutRefusesASecondIdentity(t *testing.T) {
	store := openCatalogStore(t)
	ctx := context.Background()
	catalog := store.Catalog()
	seedFamilyAndCheckout(t, catalog, "fam", "wt", "inc-1")

	rival := Checkout{
		CheckoutID:    "wt-2",
		Incarnation:   "inc-2",
		FamilyID:      "fam",
		RootPath:      "/tmp/wt",
		GitDir:        "/tmp/wt/.git",
		AdminName:     "wt",
		State:         CheckoutStateReady,
		DesiredMode:   CheckoutModeAutomatic,
		EffectiveMode: CheckoutModeAutomatic,
	}
	if err := catalog.AllocateCheckout(ctx, rival); !errors.Is(err, ErrCatalogStaleGuard) {
		t.Fatalf("second allocation = %v, want ErrCatalogStaleGuard", err)
	}
	rows, err := catalog.ListCheckouts(ctx, "fam")
	if err != nil {
		t.Fatalf("ListCheckouts: %v", err)
	}
	if len(rows) != 1 || rows[0].CheckoutID != "wt" {
		t.Fatalf("family holds %+v, want only the first identity", rows)
	}

	// Another administrative name is another working copy, and the same name
	// in another family is not taken here.
	sibling := rival
	sibling.CheckoutID, sibling.AdminName = "wt-sibling", "sibling"
	if err := catalog.AllocateCheckout(ctx, sibling); err != nil {
		t.Fatalf("allocating a second admin name = %v", err)
	}
	seedFamilyAndCheckout(t, catalog, "other", "other-wt", "inc-1")
	elsewhere := rival
	elsewhere.CheckoutID, elsewhere.FamilyID = "wt-elsewhere", "other"
	if err := catalog.AllocateCheckout(ctx, elsewhere); err != nil {
		t.Fatalf("allocating the same name in another family = %v", err)
	}

	// A name freed by a teardown may be allocated again — that is a path that
	// was removed and recreated.
	if err := catalog.DeleteCheckout(ctx, "wt"); err != nil {
		t.Fatalf("DeleteCheckout: %v", err)
	}
	if err := catalog.AllocateCheckout(ctx, rival); err != nil {
		t.Fatalf("re-allocating a freed name = %v", err)
	}

	nameless := rival
	nameless.CheckoutID, nameless.AdminName = "wt-nameless", ""
	if err := catalog.AllocateCheckout(ctx, nameless); !errors.Is(err, ErrCatalogInvalidValue) {
		t.Fatalf("allocation without an admin name = %v, want ErrCatalogInvalidValue", err)
	}
	invalid := rival
	invalid.CheckoutID, invalid.State = "wt-invalid", "invented"
	if err := catalog.AllocateCheckout(ctx, invalid); !errors.Is(err, ErrCatalogInvalidValue) {
		t.Fatalf("allocation with an unknown state = %v, want ErrCatalogInvalidValue", err)
	}
}

// TestCatalogListsAreScopedAndOrdered covers the three listings a lifecycle
// caller needs to find rows it does not already know the ids of.
func TestCatalogListsAreScopedAndOrdered(t *testing.T) {
	store := openCatalogStore(t)
	ctx := context.Background()
	catalog := store.Catalog()
	seedFamilyAndCheckout(t, catalog, "fam", "wt", "inc-1")
	seedFamilyAndCheckout(t, catalog, "other", "other-wt", "inc-1")

	for _, dedicated := range []DedicatedGraph{
		{GraphID: "g-b", RepoPrefix: "p-b", FamilyID: "fam", State: "graph_ready"},
		{GraphID: "g-a", RepoPrefix: "p-a", FamilyID: "fam", IsPrimaryBase: true,
			OwnerCheckoutID: "wt", ActiveGenerationID: 0, State: "graph_ready"},
		{GraphID: "g-z", RepoPrefix: "p-z", FamilyID: "other", State: "graph_ready"},
	} {
		if err := catalog.UpsertDedicatedGraph(ctx, dedicated); err != nil {
			t.Fatalf("UpsertDedicatedGraph %s: %v", dedicated.GraphID, err)
		}
	}
	graphs, err := catalog.ListDedicatedGraphs(ctx, "fam")
	if err != nil {
		t.Fatalf("ListDedicatedGraphs: %v", err)
	}
	if len(graphs) != 2 || graphs[0].GraphID != "g-a" || graphs[1].GraphID != "g-b" {
		t.Fatalf("ListDedicatedGraphs = %+v", graphs)
	}
	if !graphs[0].IsPrimaryBase || graphs[0].OwnerCheckoutID != "wt" || graphs[0].FamilyID != "fam" {
		t.Fatalf("primary row round trip = %+v", graphs[0])
	}
	if graphs[1].IsPrimaryBase || graphs[1].OwnerCheckoutID != "" {
		t.Fatalf("an unowned non-primary row read back as %+v", graphs[1])
	}
	if empty, err := catalog.ListDedicatedGraphs(ctx, "no-such-family"); err != nil || len(empty) != 0 {
		t.Fatalf("ListDedicatedGraphs on an unknown family = %v, %v", empty, err)
	}

	for _, view := range []RefView{
		{RefViewID: "v-b", GraphID: "g-a", SelectorKind: "branch", SelectorValue: "b",
			EnrichmentProfile: "default", State: RefViewReady, ExactView: true, ActiveRef: "refs/heads/b"},
		{RefViewID: "v-a", GraphID: "g-a", SelectorKind: "branch", SelectorValue: "a",
			EnrichmentProfile: "default", State: RefViewPending},
		{RefViewID: "v-other", GraphID: "g-b", SelectorKind: "branch", SelectorValue: "a",
			EnrichmentProfile: "default", State: RefViewReady},
	} {
		if err := catalog.UpsertRefView(ctx, view); err != nil {
			t.Fatalf("UpsertRefView %s: %v", view.RefViewID, err)
		}
	}
	views, err := catalog.ListRefViews(ctx, "g-a")
	if err != nil {
		t.Fatalf("ListRefViews: %v", err)
	}
	if len(views) != 2 || views[0].RefViewID != "v-a" || views[1].RefViewID != "v-b" {
		t.Fatalf("ListRefViews = %+v", views)
	}
	if views[1].ActiveRef != "refs/heads/b" || !views[1].ExactView || views[1].State != RefViewReady {
		t.Fatalf("ref view round trip through the listing = %+v", views[1])
	}
	// The listing and the single read must agree column for column.
	single, ok, err := catalog.GetRefView(ctx, "v-b")
	if err != nil || !ok {
		t.Fatalf("GetRefView = %v %v", ok, err)
	}
	if single != views[1] {
		t.Fatalf("GetRefView = %+v, listing = %+v", single, views[1])
	}

	for _, entry := range []CleanupEntry{
		{CleanupID: "c-b", OpaqueTargetIDs: "b", Reason: "forget_checkout", Phase: CleanupPhaseDeleting, PrimaryEpoch: 3},
		{CleanupID: "c-a", OpaqueTargetIDs: "a", Reason: "purge_checkout_layers", Phase: CleanupPhaseDone},
	} {
		if err := catalog.UpsertCleanupEntry(ctx, entry); err != nil {
			t.Fatalf("UpsertCleanupEntry %s: %v", entry.CleanupID, err)
		}
	}
	entries, err := catalog.ListCleanupEntries(ctx)
	if err != nil {
		t.Fatalf("ListCleanupEntries: %v", err)
	}
	if len(entries) != 2 || entries[0].CleanupID != "c-a" || entries[1].CleanupID != "c-b" {
		t.Fatalf("ListCleanupEntries = %+v", entries)
	}
	if entries[0].Phase != CleanupPhaseDone || entries[1].PrimaryEpoch != 3 {
		t.Fatalf("cleanup round trip = %+v", entries)
	}
}

// TestCatalogDeletesAreAddressedAndGuarded covers the row-at-a-time deletes a
// cleanup saga walks through, including the refusal that keeps a family from
// being deleted out from under its checkouts.
func TestCatalogDeletesAreAddressedAndGuarded(t *testing.T) {
	store := openCatalogStore(t)
	ctx := context.Background()
	catalog := store.Catalog()
	seedFamilyAndCheckout(t, catalog, "fam", "wt", "inc-1")

	if err := catalog.UpsertDedicatedGraph(ctx, DedicatedGraph{
		GraphID: "g-a", RepoPrefix: "p-a", FamilyID: "fam", State: "graph_ready",
	}); err != nil {
		t.Fatalf("UpsertDedicatedGraph: %v", err)
	}
	if err := catalog.UpsertRefView(ctx, RefView{
		RefViewID: "v-a", GraphID: "g-a", SelectorKind: "branch", SelectorValue: "a",
		EnrichmentProfile: "default", State: RefViewReady,
	}); err != nil {
		t.Fatalf("UpsertRefView: %v", err)
	}
	if err := catalog.UpsertCheckoutRoute(ctx, CheckoutRoute{
		CheckoutID: "wt", GraphID: "g-a", State: RouteActive,
	}); err != nil {
		t.Fatalf("UpsertCheckoutRoute: %v", err)
	}
	if err := catalog.UpsertCleanupEntry(ctx, CleanupEntry{
		CleanupID: "c-a", OpaqueTargetIDs: "a", Reason: "forget_checkout", Phase: CleanupPhasePending,
	}); err != nil {
		t.Fatalf("UpsertCleanupEntry: %v", err)
	}

	// A family cannot go while its checkouts and graphs still reference it.
	if err := catalog.DeleteRepositoryFamily(ctx, "fam"); err == nil {
		t.Fatal("deleting a populated family succeeded")
	}
	if _, ok, _ := catalog.GetRepositoryFamily(ctx, "fam"); !ok {
		t.Fatal("the refused delete removed the family anyway")
	}

	// A route blocks its checkout until it is withdrawn.
	if err := catalog.DeleteCheckout(ctx, "wt"); err == nil {
		t.Fatal("deleting a routed checkout succeeded")
	}

	for _, step := range []struct {
		name   string
		delete func() error
		gone   func() bool
	}{
		{
			name:   "route",
			delete: func() error { return catalog.DeleteCheckoutRoute(ctx, "wt") },
			gone:   func() bool { _, ok, _ := catalog.GetCheckoutRoute(ctx, "wt"); return !ok },
		},
		{
			name:   "ref view",
			delete: func() error { return catalog.DeleteRefView(ctx, "v-a") },
			gone:   func() bool { _, ok, _ := catalog.GetRefView(ctx, "v-a"); return !ok },
		},
		{
			name:   "dedicated graph",
			delete: func() error { return catalog.DeleteDedicatedGraph(ctx, "g-a") },
			gone:   func() bool { _, ok, _ := catalog.GetDedicatedGraph(ctx, "g-a"); return !ok },
		},
		{
			name:   "cleanup entry",
			delete: func() error { return catalog.DeleteCleanupEntry(ctx, "c-a") },
			gone:   func() bool { _, ok, _ := catalog.GetCleanupEntry(ctx, "c-a"); return !ok },
		},
		{
			name:   "checkout",
			delete: func() error { return catalog.DeleteCheckout(ctx, "wt") },
			gone:   func() bool { _, ok, _ := catalog.GetCheckout(ctx, "wt"); return !ok },
		},
		{
			name:   "family",
			delete: func() error { return catalog.DeleteRepositoryFamily(ctx, "fam") },
			gone:   func() bool { _, ok, _ := catalog.GetRepositoryFamily(ctx, "fam"); return !ok },
		},
	} {
		if err := step.delete(); err != nil {
			t.Fatalf("delete %s: %v", step.name, err)
		}
		if !step.gone() {
			t.Fatalf("%s survived its delete", step.name)
		}
		// A second delete reports the row as missing rather than succeeding
		// silently, so a caller can tell the two apart.
		if err := step.delete(); !errors.Is(err, ErrCatalogNotFound) {
			t.Errorf("second delete of %s = %v, want ErrCatalogNotFound", step.name, err)
		}
	}

	for name, delete := range map[string]func() error{
		"route":           func() error { return catalog.DeleteCheckoutRoute(ctx, "") },
		"ref view":        func() error { return catalog.DeleteRefView(ctx, "") },
		"dedicated graph": func() error { return catalog.DeleteDedicatedGraph(ctx, "") },
		"cleanup entry":   func() error { return catalog.DeleteCleanupEntry(ctx, "") },
		"family":          func() error { return catalog.DeleteRepositoryFamily(ctx, "") },
	} {
		if err := delete(); !errors.Is(err, ErrCatalogInvalidValue) {
			t.Errorf("empty %s id = %v, want ErrCatalogInvalidValue", name, err)
		}
	}
}
