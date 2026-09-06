package semantic

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
)

// A file-scoped enrichment pass (runDeferredEnrich's file-frontier branch)
// dispatches providers directly and never calls setEnrichStatus, so without
// BeginScopedEnrichment the activity hook -- and therefore the READY
// verdict's "enriching…" label -- can never see it. This pins that the
// scoped pass is unioned with any concurrent whole-repo pass, that release
// removes only its own repo, and that release is idempotent.
func TestAFileScopedPassPublishesItsRepoAsEnriching(t *testing.T) {
	m := NewManager(Config{}, zap.NewNop())
	var got [][]string
	m.SetActivityHook(func(repos []string) { got = append(got, repos) })

	release := m.BeginScopedEnrichment("repo")
	require.Equal(t, []string{"repo"}, got[len(got)-1])

	// A concurrent whole-repo pass on a different repo must union, not
	// replace, the scoped repo's entry.
	m.setEnrichStatus("other", "go-types", "go", EnrichStateRunning, 0, nil, "")
	require.Equal(t, []string{"other", "repo"}, got[len(got)-1])

	release()
	require.Equal(t, []string{"other"}, got[len(got)-1])

	// A second release must be a no-op: no further hook call, and the set
	// unchanged.
	callsBeforeSecondRelease := len(got)
	release()
	require.Len(t, got, callsBeforeSecondRelease, "double release must not fire the hook again")
	require.Equal(t, []string{"other"}, got[len(got)-1])
}

// repo == "" must be tolerated as a no-op: a caller that has no repo prefix
// (e.g. single-repo mode's empty prefix) must still get back a callable
// release rather than a nil func or a panic.
func TestBeginScopedEnrichmentWithEmptyRepoIsANoOp(t *testing.T) {
	m := NewManager(Config{}, zap.NewNop())
	var got [][]string
	m.SetActivityHook(func(repos []string) { got = append(got, repos) })

	release := m.BeginScopedEnrichment("")
	require.NotNil(t, release)
	require.Empty(t, got, "an empty repo must not be published as enriching")
	require.NotPanics(t, release)
	require.Empty(t, got)
}

// slowScopedProvider is a batch provider that reports what deadline the manager
// handed it, and — when there is one — waits for it rather than returning.
//
// It is deliberately not a sleep: the point is that the deadline the CALLER
// chose reaches the provider's context, which a fixed sleep would only test by
// coincidence of timing.
type slowScopedProvider struct {
	mu       sync.Mutex
	calls    int
	deadline time.Duration
	bounded  bool
}

func (p *slowScopedProvider) Name() string        { return "slow" }
func (p *slowScopedProvider) Languages() []string { return []string{"go"} }
func (p *slowScopedProvider) Available() bool     { return true }
func (p *slowScopedProvider) Close() error        { return nil }
func (p *slowScopedProvider) Enrich(graph.Store, string) (*EnrichResult, error) {
	return nil, nil
}

func (p *slowScopedProvider) EnrichFile(graph.Store, string, string) (*EnrichResult, error) {
	return nil, nil
}

func (p *slowScopedProvider) EnrichFilesContext(
	ctx context.Context, _ graph.Store, _, _ string, _ []string,
) (*EnrichResult, error) {
	deadline, bounded := ctx.Deadline()
	p.mu.Lock()
	p.calls++
	p.bounded = bounded
	if bounded {
		p.deadline = time.Until(deadline)
	}
	p.mu.Unlock()
	if !bounded {
		return &EnrichResult{Provider: "slow", Language: "go"}, nil
	}
	<-ctx.Done()
	return &EnrichResult{
		Provider: "slow", Language: "go", Partial: true, AbortReason: ctx.Err().Error(),
	}, nil
}

func (p *slowScopedProvider) observed() (calls int, bounded bool, deadline time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls, p.bounded, p.deadline
}

// A file-scoped pass must run under the deadline its CALLER chose, not the one
// semantic.timeout_seconds implies.
//
// The frontier is not the cost driver: a provider asked about five files still
// loads the whole project first, so a worktree copy's repair over a large
// repository burns the save-sized budget on the load and returns Partial having
// covered nothing. EnrichFilesWithDeadline is how such a caller buys the
// whole-repo path's budget for a scoped frontier; 0 buys no bound at all.
func TestEnrichFilesWithDeadlineBoundsThePassItDispatches(t *testing.T) {
	g := graph.New()
	provider := &slowScopedProvider{}
	m := NewManager(Config{Enabled: true}, zap.NewNop())
	m.RegisterProvider(provider)

	result, err := m.EnrichFilesWithDeadline(
		g, "r", t.TempDir(), "go", []string{"r/main.go"}, 50*time.Millisecond)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.Partial,
		"a provider still working when the deadline expires reports a partial pass")
	calls, bounded, deadline := provider.observed()
	require.Equal(t, 1, calls)
	require.True(t, bounded, "the caller's deadline must reach the provider's context")
	require.LessOrEqual(t, deadline, 50*time.Millisecond)

	result, err = m.EnrichFilesWithDeadline(g, "r", t.TempDir(), "go", []string{"r/main.go"}, 0)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.False(t, result.Partial, "deadline 0 must impose no bound")
	calls, bounded, _ = provider.observed()
	require.Equal(t, 2, calls)
	require.False(t, bounded)
}

// EnrichFiles keeps its documented default — the configured semantic timeout —
// now that it routes through EnrichFilesWithDeadline. A watcher save must not
// silently acquire the repo-scaled budget.
func TestEnrichFilesStillRunsUnderTheConfiguredSemanticTimeout(t *testing.T) {
	require.Equal(t, 2*time.Second,
		NewManager(Config{Enabled: true, TimeoutSeconds: 2}, zap.NewNop()).ScopedEnrichTimeout())
	require.Zero(t, NewManager(Config{Enabled: true}, zap.NewNop()).ScopedEnrichTimeout(),
		"an unset timeout has always meant unbounded here; routing through "+
			"EnrichFilesWithDeadline must not turn it into an instant deadline")

	g := graph.New()
	provider := &slowScopedProvider{}
	m := NewManager(Config{Enabled: true, TimeoutSeconds: 1}, zap.NewNop())
	m.RegisterProvider(provider)

	result, err := m.EnrichFiles(g, "r", t.TempDir(), "go", []string{"r/main.go"})
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.Partial)
	_, bounded, deadline := provider.observed()
	require.True(t, bounded)
	require.LessOrEqual(t, deadline, time.Second)
}

// RepoEnrichDeadline is the whole-repo path's own sizing, exported rather than
// re-derived: a scoped caller that computed its own would drift from
// enrichRepoTimeout the moment either was tuned.
func TestRepoEnrichDeadlineIsTheWholeRepoScaling(t *testing.T) {
	m := NewManager(Config{Enabled: true}, zap.NewNop())
	for _, nodeCount := range []int{0, 1, 1_000, 50_000, 10_000_000} {
		require.Equal(t, enrichRepoTimeout(nodeCount), m.RepoEnrichDeadline(nodeCount),
			"node count %d", nodeCount)
	}
	require.Equal(t, maxEnrichRepoTimeout, m.RepoEnrichDeadline(10_000_000),
		"the ceiling still applies — a monorepo may not pin the pass for hours")

	t.Setenv("GORTEX_LSP_ENRICH_TIMEOUT", "3m")
	require.Equal(t, 3*time.Minute, m.RepoEnrichDeadline(50_000),
		"the env override wins verbatim, exactly as it does for the whole-repo pass")
}

// CopyRepairDeadline bounds the repo-scaled deadline near the save (scoped)
// deadline. The measurement behind it -- an 82-file python repair running 89
// minutes where the whole-repo pass it stands in for takes ~8 min -- was a
// scoped dispatch falling through to EnrichRepoContext because tstypes
// implemented no batch face; that root cause is fixed at the provider. The cap
// stays as a bound for providers that genuinely cannot be scoped (LSP- and
// compiler-backed ones must load the whole project either way), for which the
// repo scaling alone mostly delays the partial→full fallback instead of
// bounding it.
func TestCopyRepairDeadlineIsBoundedNearTheSaveDeadline(t *testing.T) {
	m := NewManager(Config{Enabled: true, TimeoutSeconds: 120}, zap.NewNop())
	require.Equal(t, 120*time.Second, m.ScopedEnrichTimeout())

	// A huge repo's uncapped scaling would be maxEnrichRepoTimeout (90m),
	// far past 5x the 120s save deadline -- the cap wins.
	require.Equal(t, 600*time.Second, m.CopyRepairDeadline(10_000_000),
		"a huge repo's scaled deadline is capped at 5x the save deadline")

	// With a larger save deadline the cap (5x) sits above the repo scaling's
	// floor for a tiny repo (defaultEnrichRepoTimeout == 10m == 600s), so the
	// uncapped repo value wins instead of the cap.
	mBigSave := NewManager(Config{Enabled: true, TimeoutSeconds: 200}, zap.NewNop())
	require.Equal(t, mBigSave.RepoEnrichDeadline(0), mBigSave.CopyRepairDeadline(0),
		"the repo scaling, below the cap here, is returned uncapped")
	require.Less(t, mBigSave.CopyRepairDeadline(0), 5*mBigSave.ScopedEnrichTimeout())

	// An unbounded (0 / unset) save deadline means the operator opted out of
	// bounds entirely -- the repo scaling stays the only bound.
	mUnbounded := NewManager(Config{Enabled: true, TimeoutSeconds: 0}, zap.NewNop())
	require.Zero(t, mUnbounded.ScopedEnrichTimeout())
	require.Equal(t, mUnbounded.RepoEnrichDeadline(10_000_000), mUnbounded.CopyRepairDeadline(10_000_000),
		"an unbounded save deadline leaves the repo scaling as the only bound")
}
