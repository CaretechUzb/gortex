package indexer

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
)

// deriveStateGraph embeds *graph.Graph (auto-satisfying graph.Store) and adds
// the DeriveStateStore and RepoIndexStateReader capabilities, standing in for
// the on-disk backend so a test can observe the stamp without opening SQLite.
type deriveStateGraph struct {
	*graph.Graph

	stamped   []graph.DeriveCompletion
	refreshed []string
	at        int64
	calls     int
	indexed   map[string]graph.RepoIndexState
}

func newDeriveStateGraph() *deriveStateGraph {
	return &deriveStateGraph{Graph: graph.New(), indexed: map[string]graph.RepoIndexState{}}
}

func (d *deriveStateGraph) StampDeriveState(completions []graph.DeriveCompletion, at int64) error {
	d.calls++
	d.stamped = append(d.stamped, completions...)
	d.at = at
	return nil
}

// RefreshDeriveState mirrors the real store: it renews only a completion that
// already exists, so a repo the global passes never covered stays absent.
func (d *deriveStateGraph) RefreshDeriveState(prefixes []string, _ int64) (int, error) {
	n := 0
	for _, prefix := range prefixes {
		if _, found, _ := d.GetDeriveState(prefix); found {
			d.refreshed = append(d.refreshed, prefix)
			n++
		}
	}
	return n, nil
}

func (d *deriveStateGraph) GetDeriveState(repoPrefix string) (graph.DeriveState, bool, error) {
	for _, c := range d.stamped {
		if c.RepoPrefix == repoPrefix {
			return graph.DeriveState{RepoPrefix: repoPrefix, DerivedSHA: c.DerivedSHA}, true, nil
		}
	}
	return graph.DeriveState{RepoPrefix: repoPrefix}, false, nil
}

func (d *deriveStateGraph) GetRepoIndexState(repoPrefix string) (graph.RepoIndexState, bool, error) {
	st, ok := d.indexed[repoPrefix]
	return st, ok, nil
}

func (d *deriveStateGraph) prefixes() []string {
	out := make([]string, 0, len(d.stamped))
	for _, c := range d.stamped {
		out = append(out, c.RepoPrefix)
	}
	return out
}

func deriveTestIndexer(g graph.Store, prefixes ...string) *MultiIndexer {
	mi := &MultiIndexer{
		graph:    g,
		repos:    map[string]*RepoMetadata{},
		indexers: map[string]*Indexer{},
		logger:   zap.NewNop(),
	}
	for _, prefix := range prefixes {
		mi.repos[prefix] = &RepoMetadata{RepoPrefix: prefix}
		mi.indexers[prefix] = &Indexer{repoPrefix: prefix}
	}
	return mi
}

// An unscoped run is the historical whole-graph form, so it covers — and may
// therefore stamp — every repo the indexer owns.
func TestDerivedCoverageIsEveryTrackedRepoWhenUnscoped(t *testing.T) {
	mi := deriveTestIndexer(newDeriveStateGraph(), "repoB", "repoA", "repoC")
	require.Equal(t, []string{"repoA", "repoB", "repoC"}, mi.derivedCoverage(nil, true))
}

// The daemon's warm restart arms an explicit scope AND carries a census
// attestation, which makes the passes run whole-graph. Coverage has to follow
// what actually ran: claiming only the changed repos there would leave a repo
// that has never been stamped reading unknown until some later run happened to
// arrive with a nil scope.
func TestDerivedCoverageFollowsFullCoverageNotTheArmedScope(t *testing.T) {
	mi := deriveTestIndexer(newDeriveStateGraph(), "repoA", "repoB", "repoC")
	scope := map[string]struct{}{"repoA": {}}

	require.Equal(t, []string{"repoA", "repoB", "repoC"}, mi.derivedCoverage(scope, true),
		"a census-attested batch derives every repo, whatever scope it armed")
	require.Equal(t, []string{"repoA"}, mi.derivedCoverage(scope, false))
}

// A scoped run covers exactly its frontier. Stamping the whole workspace here
// would claim a derive for repos whose passes never ran — the fail-open
// direction, and the one that makes a readiness column lie.
func TestDerivedCoverageIsTheFrontierWhenScoped(t *testing.T) {
	mi := deriveTestIndexer(newDeriveStateGraph(), "repoA", "repoB", "repoC")
	scope := map[string]struct{}{"repoC": {}, "repoA": {}}
	require.Equal(t, []string{"repoA", "repoC"}, mi.derivedCoverage(scope, false))
}

// The unprefixed single-repo sentinel has no per-repo state row, so it must
// never reach the stamp as a repo of its own.
func TestDerivedCoverageDropsTheUnprefixedSentinel(t *testing.T) {
	mi := deriveTestIndexer(newDeriveStateGraph(), "", "repoA")
	require.Equal(t, []string{"repoA"}, mi.derivedCoverage(nil, true))
	require.Empty(t, mi.derivedCoverage(map[string]struct{}{"": {}}, false))
}

// The completion contract: a run that returned an error derived some unknown
// prefix of its work, so it records nothing. Stamping it would assert a
// currency it never established.
func TestCompleteDeriveStampsNothingWhenThePassFailed(t *testing.T) {
	g := newDeriveStateGraph()
	mi := deriveTestIndexer(g, "repoA", "repoB")

	mi.completeDerive([]string{"repoA", "repoB"}, false, context.Canceled)
	require.Zero(t, g.calls, "a preempted run must not be recorded as a derive")

	mi.completeDerive(nil, false, nil)
	require.Zero(t, g.calls, "an empty covered set has nothing to assert")

	mi.completeDerive([]string{"repoA"}, false, nil)
	require.Equal(t, 1, g.calls)
	require.Equal(t, []string{"repoA"}, g.prefixes())
}

// The nil-graph early return does no work at all and leaves ctx.Err() nil, so
// it has to be an explicit error. Under the old void signature it was
// indistinguishable from a complete run, and any "stamp unless cancelled" rule
// would have recorded a no-op as a successful derivation.
func TestNilGraphGlobalPassIsAnErrorNotASilentSuccess(t *testing.T) {
	mi := &MultiIndexer{logger: zap.NewNop()}
	covered, err := mi.runGlobalGraphPassesTopologyHeld(context.Background(), nil, false)
	require.ErrorIs(t, err, errDeriveNoGraph)
	require.Empty(t, covered)
}

// A preempted run reports why it stopped and claims no coverage, so the repos
// keep reading partial until the scheduler's re-run completes.
func TestPreemptedGlobalPassClaimsNoCoverage(t *testing.T) {
	g := newDeriveStateGraph()
	mi := deriveTestIndexer(g, "repoA")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	covered, err := mi.runGlobalGraphPassesTopologyHeld(ctx, nil, false)
	require.ErrorIs(t, err, context.Canceled)
	require.Empty(t, covered)

	mi.completeDerive(covered, false, err)
	require.Zero(t, g.calls)
}

// The stamp carries the pass version (so a build whose synthesis has since
// changed semantics re-derives instead of reading current forever) and the
// SHA the passes actually read — taken from repo_index_state rather than a
// fresh rev-parse, which would name a HEAD that may have moved since.
func TestStampDeriveStateRecordsPassVersionAndTheIndexedSHA(t *testing.T) {
	g := newDeriveStateGraph()
	g.indexed["repoA"] = graph.RepoIndexState{RepoPrefix: "repoA", IndexedSHA: "deadbeef"}
	mi := deriveTestIndexer(g, "repoA", "repoB")

	mi.stampDeriveState([]string{"repoA", "repoB"}, true)

	require.Len(t, g.stamped, 2)
	require.Equal(t, "deadbeef", g.stamped[0].DerivedSHA)
	require.Equal(t, int64(derivePassVersion), g.stamped[0].PassVersion)
	require.True(t, g.stamped[0].Scoped)
	// repoB has no index-state row yet; that is provenance-only, so it stamps
	// empty rather than blocking the completion it is attached to.
	require.Empty(t, g.stamped[1].DerivedSHA)
	require.Equal(t, int64(derivePassVersion), g.stamped[1].PassVersion)
	require.Positive(t, g.at)
}

// A backend without durable per-repo state simply records nothing, exactly as
// it records no index state and no file-mtime ledger.
func TestStampDeriveStateSkipsABackendWithoutDurableState(t *testing.T) {
	mi := deriveTestIndexer(graph.New(), "repoA")
	require.NotPanics(t, func() { mi.stampDeriveState([]string{"repoA"}, false) })
}

// The incremental derive keeps a derived repo current but must never claim one
// the global passes have not covered. Pinned at the indexer seam too, because
// this is where the wrong method call would be made.
func TestRefreshDeriveStateOnlyRenewsWhatTheGlobalPassesEstablished(t *testing.T) {
	g := newDeriveStateGraph()
	mi := deriveTestIndexer(g, "derived", "warmup")

	mi.stampDeriveState([]string{"derived"}, false)
	mi.refreshDeriveState([]string{"derived", "warmup"})

	require.Equal(t, []string{"derived"}, g.refreshed)
	_, found, err := g.GetDeriveState("warmup")
	require.NoError(t, err)
	require.False(t, found, "a never-derived repo must stay never-derived")
}

// fakeMarker records the marker calls a run makes, in order.
type fakeMarker struct {
	opened     [][]string
	configHash []string
	closes     int
	enriching  [][]string
}

func (f *fakeMarker) DeriveBegan(scope []string, configHash string) {
	f.opened = append(f.opened, scope)
	f.configHash = append(f.configHash, configHash)
}
func (f *fakeMarker) DeriveEnded() { f.closes++ }
func (f *fakeMarker) EnrichingChanged(r []string) {
	f.enriching = append(f.enriching, r)
}

// A preempted run is the case that matters: it returns from the middle of the
// pass, so the marker has to close on a defer. Left open, the column would say
// "deriving…" for that repo until the daemon exits — a false all-clear over a
// derive that never finished.
func TestTheDeriveMarkerClosesEvenWhenTheRunIsPreempted(t *testing.T) {
	marker := &fakeMarker{}
	mi := deriveTestIndexer(newDeriveStateGraph(), "repoA", "repoB")
	mi.SetRuntimeMarker(marker)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := mi.runGlobalGraphPassesTopologyHeld(ctx, nil, false)
	require.ErrorIs(t, err, context.Canceled)

	require.Equal(t, [][]string{{"repoA", "repoB"}}, marker.opened,
		"the run must publish the repos it covers, not merely that it is running")
	require.Equal(t, 1, marker.closes, "a preempted run must still close its marker")
}

// A run that never starts publishes nothing — the nil-graph return happens
// before the marker opens, so there is no window in which a reader sees a
// derive that is not running.
func TestANilGraphRunPublishesNoMarker(t *testing.T) {
	marker := &fakeMarker{}
	mi := &MultiIndexer{logger: zap.NewNop(), runtimeMarker: marker}
	_, err := mi.runGlobalGraphPassesTopologyHeld(context.Background(), nil, false)
	require.ErrorIs(t, err, errDeriveNoGraph)
	require.Empty(t, marker.opened)
	require.Zero(t, marker.closes)
}

// The cold multi-repo index runs whole-graph passes while holding only the
// batch gate's READ side, so it may claim just the repos whose mutation lane
// it holds. Anything wider would assert a currency a concurrent watcher write
// could already have broken.
func TestIntersectPrefixesNarrowsAWholeGraphClaim(t *testing.T) {
	require.Equal(t, []string{"repoA", "repoC"},
		intersectPrefixes([]string{"repoA", "repoB", "repoC"}, []string{"repoC", "repoA"}))
	require.Nil(t, intersectPrefixes([]string{"repoA"}, nil))
	require.Nil(t, intersectPrefixes(nil, []string{"repoA"}))
}
