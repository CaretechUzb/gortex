package semantic

import (
	"context"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
)

// TestManager_EnrichOne_DrainsLegacyProviderAtDeadline verifies that the
// watchdog reports a legacy timeout without detaching its in-process graph
// writer. EnrichAll cannot return (or advance) until that writer stops.
func TestManager_EnrichOne_DrainsLegacyProviderAtDeadline(t *testing.T) {
	t.Setenv("GORTEX_LSP_ENRICH_TIMEOUT", "50ms")

	cfg := Config{
		Enabled: true,
		Providers: []ProviderConfig{
			{Name: "slow-go", Languages: []string{"go"}, Priority: 1, Enabled: true},
		},
	}
	mgr := NewManager(cfg, zap.NewNop())

	release := make(chan struct{})
	var enrichReturned atomic.Bool
	var mutations atomic.Int32
	mgr.RegisterProvider(&mockProvider{
		name:      "slow-go",
		languages: []string{"go"},
		available: true,
		enrichFunc: func(g graph.Store, root string) (*EnrichResult, error) {
			<-release // block well past the 50ms deadline
			mutations.Add(1)
			enrichReturned.Store(true)
			return &EnrichResult{Provider: "slow-go", Language: "go"}, nil
		},
	})

	g := graph.New()
	g.AddNode(&graph.Node{ID: "main.go::main", Kind: graph.KindFunction, Name: "main", FilePath: "main.go", Language: "go"})
	roots := map[string]string{"default": "/tmp/test"}

	resultCh := make(chan []*EnrichResult, 1)
	go func() {
		res, _, _ := mgr.EnrichAll(g, roots, EnrichOptions{})
		resultCh <- res
	}()

	require.Eventually(t, func() bool {
		statuses := mgr.EnrichmentStatuses()
		return len(statuses) == 1 && statuses[0].State == EnrichStateDraining
	}, time.Second, 5*time.Millisecond, "deadline must surface the draining watchdog state")
	select {
	case <-resultCh:
		t.Fatal("EnrichAll returned while the timed-out provider could still mutate")
	default:
	}
	assert.True(t, mgr.EnrichmentActive(), "draining must keep concurrency/memory gates active")

	close(release)
	var results []*EnrichResult
	select {
	case results = <-resultCh:
	case <-time.After(time.Second):
		t.Fatal("EnrichAll did not return after the provider stopped")
	}
	assert.Empty(t, results, "a timed-out legacy provider's terminal result is discarded")
	require.True(t, enrichReturned.Load())
	assert.Equal(t, int32(1), mutations.Load())
	time.Sleep(25 * time.Millisecond)
	assert.Equal(t, int32(1), mutations.Load(), "no provider mutation may occur after EnrichAll returns")
	statuses := mgr.EnrichmentStatuses()
	require.Len(t, statuses, 1)
	assert.Equal(t, EnrichStateAbandoned, statuses[0].State)
	assert.False(t, mgr.EnrichmentActive())
}

// TestManager_EnrichOne_DisabledDeadline verifies the bound can be switched
// off: with GORTEX_LSP_ENRICH_TIMEOUT=off a provider runs to completion even
// if slow.
func TestManager_EnrichOne_DisabledDeadline(t *testing.T) {
	t.Setenv("GORTEX_LSP_ENRICH_TIMEOUT", "off")

	cfg := Config{
		Enabled: true,
		Providers: []ProviderConfig{
			{Name: "go", Languages: []string{"go"}, Priority: 1, Enabled: true},
		},
	}
	mgr := NewManager(cfg, zap.NewNop())
	mgr.RegisterProvider(&mockProvider{
		name:      "go",
		languages: []string{"go"},
		available: true,
		enrichFunc: func(g graph.Store, root string) (*EnrichResult, error) {
			time.Sleep(80 * time.Millisecond)
			return &EnrichResult{Provider: "go", Language: "go", EdgesConfirmed: 7}, nil
		},
	})

	g := graph.New()
	g.AddNode(&graph.Node{ID: "main.go::main", Kind: graph.KindFunction, Name: "main", FilePath: "main.go", Language: "go"})

	results, _, err := mgr.EnrichAll(g, map[string]string{"default": "/tmp/test"}, EnrichOptions{})
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, 7, results[0].EdgesConfirmed)
}

// mockCtxProvider is a mockProvider that also implements ContextEnricher,
// so the Manager dispatches it on the cooperative-cancellation path.
type mockCtxProvider struct {
	mockProvider
	enrichCtxFunc func(ctx context.Context, g graph.Store, repoPrefix, repoRoot string, deadline EnrichDeadlinePolicy) (*EnrichResult, error)
}

type preselectionCtxProvider struct {
	mockCtxProvider
}

func (*preselectionCtxProvider) UsePreselectionDeadline() {}

type closeUnblocksProvider struct {
	mockProvider
	release   chan struct{}
	closeOnce sync.Once
}

func (p *closeUnblocksProvider) Close() error {
	p.closeOnce.Do(func() { close(p.release) })
	return nil
}

func (m *mockCtxProvider) EnrichRepoContext(ctx context.Context, g graph.Store, repoPrefix, repoRoot string, deadline EnrichDeadlinePolicy) (*EnrichResult, error) {
	return m.enrichCtxFunc(ctx, g, repoPrefix, repoRoot, deadline)
}

// TestManager_EnrichOne_ContextProviderPartialIsCounted verifies the
// cooperative deadline path: a ContextEnricher that runs past the
// deadline is cancelled via its context, returns a Partial result, and
// that result is COUNTED (appended to the results, recorded in
// lastResults, surfaced as "partial" in EnrichmentStatuses) instead of
// being discarded like the legacy detach path.
func TestManager_EnrichOne_ContextProviderPartialIsCounted(t *testing.T) {
	t.Setenv("GORTEX_LSP_ENRICH_TIMEOUT", "50ms")

	cfg := Config{
		Enabled: true,
		Providers: []ProviderConfig{
			{Name: "ctx-go", Languages: []string{"go"}, Priority: 1, Enabled: true},
		},
	}
	mgr := NewManager(cfg, zap.NewNop())
	mgr.RegisterProvider(&mockCtxProvider{
		mockProvider: mockProvider{
			name:      "ctx-go",
			languages: []string{"go"},
			available: true,
		},
		enrichCtxFunc: func(ctx context.Context, g graph.Store, repoPrefix, repoRoot string, _ EnrichDeadlinePolicy) (*EnrichResult, error) {
			// Simulate a pass that lands work incrementally and is cut
			// by the deadline: block until cancellation, then report the
			// work already landed.
			<-ctx.Done()
			return &EnrichResult{
				Provider:       "ctx-go",
				Language:       "go",
				EdgesConfirmed: 3,
				EdgesAdded:     2,
				NodesEnriched:  5,
				Partial:        true,
				AbortReason:    ctx.Err().Error(),
			}, nil
		},
	})

	g := graph.New()
	g.AddNode(&graph.Node{ID: "main.go::main", Kind: graph.KindFunction, Name: "main", FilePath: "main.go", Language: "go"})

	done := make(chan []*EnrichResult, 1)
	go func() {
		res, _, _ := mgr.EnrichAll(g, map[string]string{"default": "/tmp/test"}, EnrichOptions{})
		done <- res
	}()

	var results []*EnrichResult
	select {
	case results = <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("EnrichAll did not return after the context deadline")
	}

	require.Len(t, results, 1, "a partial result must be counted, not discarded")
	assert.True(t, results[0].Partial)
	assert.Equal(t, 3, results[0].EdgesConfirmed)
	assert.Equal(t, 5, results[0].NodesEnriched)

	statuses := mgr.EnrichmentStatuses()
	require.Len(t, statuses, 1)
	assert.Equal(t, "default", statuses[0].Repo)
	assert.Equal(t, "ctx-go", statuses[0].Provider)
	assert.Equal(t, EnrichStatePartial, statuses[0].State)
	assert.Equal(t, 3, statuses[0].EdgesConfirmed)
	assert.Equal(t, 5, statuses[0].NodesEnriched)
	assert.Greater(t, statuses[0].DeadlineSeconds, 0.0)
}

func TestManager_PreselectionContextProviderKeepsNodeScaledDeadline(t *testing.T) {
	t.Setenv("GORTEX_LSP_ENRICH_TIMEOUT", "")
	mgr := NewManager(Config{Enabled: true}, zap.NewNop())
	var observed time.Duration
	provider := &preselectionCtxProvider{mockCtxProvider: mockCtxProvider{
		mockProvider: mockProvider{name: "scip-go", languages: []string{"go"}, available: true},
		enrichCtxFunc: func(ctx context.Context, _ graph.Store, _, _ string, _ EnrichDeadlinePolicy) (*EnrichResult, error) {
			deadline, ok := ctx.Deadline()
			if ok {
				observed = time.Until(deadline)
			}
			return &EnrichResult{Provider: "scip-go", Language: "go"}, nil
		},
	}}

	// 3,001 enrichable nodes had a 12m00.04s deadline before SCIP became
	// context-aware. A fixed 120s inner timeout would silently cut this work.
	nodeCount := 3001
	want := scaleEnrichTimeout(nodeCount)
	partial := make(map[string]bool)
	results := mgr.runEnrichOne(graph.New(), "repo", t.TempDir(), "go", provider, nodeCount,
		RepoEnrichState{}, nil, nil, partial)
	require.Len(t, results, 1)
	assert.Greater(t, observed, 120*time.Second)
	assert.InDelta(t, want.Seconds(), observed.Seconds(), 0.25)
	assert.False(t, partial["repo"])
}

// TestManager_EnrichOne_ContextProviderWedgedPastGraceIsDrained verifies that
// even a ContextEnricher which ignores cancellation cannot outlive the pass.
func TestManager_EnrichOne_ContextProviderWedgedPastGraceIsDrained(t *testing.T) {
	t.Setenv("GORTEX_LSP_ENRICH_TIMEOUT", "20ms")
	oldGrace := enrichCancelGrace
	enrichCancelGrace = 30 * time.Millisecond
	t.Cleanup(func() { enrichCancelGrace = oldGrace })

	cfg := Config{
		Enabled: true,
		Providers: []ProviderConfig{
			{Name: "wedged-go", Languages: []string{"go"}, Priority: 1, Enabled: true},
		},
	}
	mgr := NewManager(cfg, zap.NewNop())

	release := make(chan struct{})
	mgr.RegisterProvider(&mockCtxProvider{
		mockProvider: mockProvider{
			name:      "wedged-go",
			languages: []string{"go"},
			available: true,
		},
		enrichCtxFunc: func(ctx context.Context, g graph.Store, repoPrefix, repoRoot string, _ EnrichDeadlinePolicy) (*EnrichResult, error) {
			<-release // ignores ctx entirely
			return &EnrichResult{Provider: "wedged-go", Language: "go"}, nil
		},
	})

	g := graph.New()
	g.AddNode(&graph.Node{ID: "main.go::main", Kind: graph.KindFunction, Name: "main", FilePath: "main.go", Language: "go"})

	done := make(chan []*EnrichResult, 1)
	go func() {
		res, _, _ := mgr.EnrichAll(g, map[string]string{"default": "/tmp/test"}, EnrichOptions{})
		done <- res
	}()

	require.Eventually(t, func() bool {
		statuses := mgr.EnrichmentStatuses()
		return len(statuses) == 1 && statuses[0].State == EnrichStateDraining
	}, time.Second, 5*time.Millisecond)
	select {
	case <-done:
		t.Fatal("EnrichAll returned while a context provider ignored cancellation")
	default:
	}
	close(release)
	select {
	case res := <-done:
		assert.Empty(t, res, "a provider wedged past deadline+grace is discarded after it stops")
	case <-time.After(time.Second):
		t.Fatal("EnrichAll did not finish after the wedged provider was released")
	}
	statuses := mgr.EnrichmentStatuses()
	require.Len(t, statuses, 1)
	assert.Equal(t, EnrichStateAbandoned, statuses[0].State)
}

// TestManager_EnrichmentStatuses_AbandonedAndCompleted verifies the
// health surface for a timed-out legacy pass (drained then abandoned) and the
// happy path. The second provider must not overlap the first writer.
func TestManager_EnrichmentStatuses_AbandonedAndCompleted(t *testing.T) {
	t.Setenv("GORTEX_LSP_ENRICH_TIMEOUT", "50ms")

	cfg := Config{
		Enabled: true,
		Providers: []ProviderConfig{
			{Name: "slow-go", Languages: []string{"go"}, Priority: 1, Enabled: true},
			{Name: "fast-py", Languages: []string{"python"}, Priority: 1, Enabled: true},
		},
	}
	mgr := NewManager(cfg, zap.NewNop())

	release := make(chan struct{})
	fastStarted := make(chan struct{})
	mgr.RegisterProvider(&mockProvider{
		name:      "slow-go",
		languages: []string{"go"},
		available: true,
		enrichFunc: func(g graph.Store, root string) (*EnrichResult, error) {
			<-release
			return &EnrichResult{Provider: "slow-go", Language: "go"}, nil
		},
	})
	mgr.RegisterProvider(&mockProvider{
		name:      "fast-py",
		languages: []string{"python"},
		available: true,
		enrichFunc: func(g graph.Store, root string) (*EnrichResult, error) {
			close(fastStarted)
			return &EnrichResult{Provider: "fast-py", Language: "python", EdgesAdded: 1}, nil
		},
	})

	g := graph.New()
	g.AddNode(&graph.Node{ID: "main.go::main", Kind: graph.KindFunction, Name: "main", FilePath: "main.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "app.py::run", Kind: graph.KindFunction, Name: "run", FilePath: "app.py", Language: "python"})

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _, _ = mgr.EnrichAll(g, map[string]string{"default": "/tmp/test"}, EnrichOptions{})
	}()
	require.Eventually(t, func() bool {
		for _, status := range mgr.EnrichmentStatuses() {
			if status.Provider == "slow-go" && status.State == EnrichStateDraining {
				return true
			}
		}
		return false
	}, time.Second, 5*time.Millisecond)
	select {
	case <-fastStarted:
		t.Fatal("the next provider overlapped a timed-out legacy writer")
	default:
	}
	close(release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("EnrichAll did not finish after the slow provider stopped")
	}

	byProvider := map[string]EnrichmentStatus{}
	for _, st := range mgr.EnrichmentStatuses() {
		byProvider[st.Provider] = st
	}
	require.Contains(t, byProvider, "slow-go")
	require.Contains(t, byProvider, "fast-py")
	assert.Equal(t, EnrichStateAbandoned, byProvider["slow-go"].State)
	assert.NotEmpty(t, byProvider["slow-go"].Detail)
	assert.Equal(t, EnrichStateCompleted, byProvider["fast-py"].State)
	assert.Equal(t, 1, byProvider["fast-py"].EdgesAdded)
}

func TestManager_CloseCancelsContextProviderAndWaitsForPass(t *testing.T) {
	t.Setenv("GORTEX_LSP_ENRICH_TIMEOUT", "off")
	mgr := NewManager(Config{
		Enabled: true,
		Providers: []ProviderConfig{
			{Name: "ctx-go", Languages: []string{"go"}, Priority: 1, Enabled: true},
		},
	}, zap.NewNop())
	started := make(chan struct{})
	returned := make(chan struct{})
	mgr.RegisterProvider(&mockCtxProvider{
		mockProvider: mockProvider{name: "ctx-go", languages: []string{"go"}, available: true},
		enrichCtxFunc: func(ctx context.Context, _ graph.Store, _, _ string, _ EnrichDeadlinePolicy) (*EnrichResult, error) {
			close(started)
			<-ctx.Done()
			close(returned)
			return &EnrichResult{Provider: "ctx-go", Language: "go", Partial: true, AbortReason: ctx.Err().Error()}, nil
		},
	})
	g := graph.New()
	g.AddNode(&graph.Node{ID: "main.go::main", Kind: graph.KindFunction, FilePath: "main.go", Language: "go"})
	repoRoot := t.TempDir()
	enrichDone := make(chan struct{})
	go func() {
		defer close(enrichDone)
		_, _, _ = mgr.EnrichAll(g, map[string]string{"default": repoRoot}, EnrichOptions{})
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("context provider did not start")
	}
	require.NoError(t, mgr.Close())
	select {
	case <-returned:
	default:
		t.Fatal("Close returned before the context provider stopped")
	}
	select {
	case <-enrichDone:
	default:
		t.Fatal("Close returned before the complete manager-owned pass stopped")
	}
}

func TestManager_CloseStopsLegacyProviderBeforeReturning(t *testing.T) {
	t.Setenv("GORTEX_LSP_ENRICH_TIMEOUT", "off")
	release := make(chan struct{})
	started := make(chan struct{})
	provider := &closeUnblocksProvider{
		mockProvider: mockProvider{
			name: "legacy-go", languages: []string{"go"}, available: true,
			enrichFunc: func(_ graph.Store, _ string) (*EnrichResult, error) {
				close(started)
				<-release
				return &EnrichResult{Provider: "legacy-go", Language: "go"}, nil
			},
		},
		release: release,
	}
	mgr := NewManager(Config{
		Enabled: true,
		Providers: []ProviderConfig{
			{Name: "legacy-go", Languages: []string{"go"}, Priority: 1, Enabled: true},
		},
	}, zap.NewNop())
	mgr.RegisterProvider(provider)
	g := graph.New()
	g.AddNode(&graph.Node{ID: "main.go::main", Kind: graph.KindFunction, FilePath: "main.go", Language: "go"})
	repoRoot := t.TempDir()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _, _ = mgr.EnrichAll(g, map[string]string{"default": repoRoot}, EnrichOptions{})
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("legacy provider did not start")
	}
	require.NoError(t, mgr.Close())
	// The pass signals completion from a deferred close in another goroutine,
	// so a non-blocking poll here asserts that goroutine was scheduled before
	// this statement — a timing property nothing guarantees, and one a loaded
	// runner loses. Wait bounded instead: the bug this guards against is Close
	// returning while the pass runs on, which no amount of waiting hides.
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the legacy provider and pass were still running after Close returned")
	}
}

func TestManager_DeadlineDoesNotGrowProviderGoroutines(t *testing.T) {
	t.Setenv("GORTEX_LSP_ENRICH_TIMEOUT", "1ms")
	mgr := NewManager(Config{
		Enabled: true,
		Providers: []ProviderConfig{
			{Name: "slow-go", Languages: []string{"go"}, Priority: 1, Enabled: true},
		},
	}, zap.NewNop())
	mgr.RegisterProvider(&mockProvider{
		name: "slow-go", languages: []string{"go"}, available: true,
		enrichFunc: func(_ graph.Store, _ string) (*EnrichResult, error) {
			time.Sleep(3 * time.Millisecond)
			return &EnrichResult{Provider: "slow-go", Language: "go"}, nil
		},
	})
	g := graph.New()
	g.AddNode(&graph.Node{ID: "main.go::main", Kind: graph.KindFunction, FilePath: "main.go", Language: "go"})
	repoRoot := t.TempDir()
	baseline := runtime.NumGoroutine()
	for range 40 {
		_, _, err := mgr.EnrichAll(g, map[string]string{"default": repoRoot}, EnrichOptions{})
		require.NoError(t, err)
	}
	require.Eventually(t, func() bool {
		runtime.GC()
		return runtime.NumGoroutine() <= baseline+4
	}, time.Second, 10*time.Millisecond, "deadline handling must drain, not accumulate provider goroutines")
}

// TestScaleEnrichTimeout is the table for the size-scaled per-repo
// deadline: floor for small repos, linear per-node growth, hard ceiling.
func TestScaleEnrichTimeout(t *testing.T) {
	cases := []struct {
		name      string
		nodeCount int
		want      time.Duration
	}{
		{"empty repo gets the floor", 0, 10 * time.Minute},
		{"negative count clamps to the floor", -5, 10 * time.Minute},
		{"small repo stays near the floor", 1000, 10*time.Minute + 40*time.Second},
		{"medium repo scales linearly", 30_000, 30 * time.Minute},
		{"prometheus-sized repo fits under the ceiling", 93_584, 10*time.Minute + time.Duration(93_584)*40*time.Millisecond},
		{"monorepo hits the ceiling", 1_000_000, 90 * time.Minute},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, scaleEnrichTimeout(tc.nodeCount))
		})
	}
}

// TestEnrichOuterCeiling verifies the generous outer bound the Manager
// holds over a ContextEnricher: the hard ceiling when unset (NOT the
// whole-repo scaled value — lazy budgeting reclaims that headroom), the
// env override verbatim, the off switch, and a garbage fallback.
func TestEnrichOuterCeiling(t *testing.T) {
	t.Run("unset is the hard ceiling", func(t *testing.T) {
		t.Setenv("GORTEX_LSP_ENRICH_TIMEOUT", "")
		assert.Equal(t, maxEnrichRepoTimeout, enrichOuterCeiling())
	})
	t.Run("explicit override wins verbatim", func(t *testing.T) {
		t.Setenv("GORTEX_LSP_ENRICH_TIMEOUT", "5m")
		assert.Equal(t, 5*time.Minute, enrichOuterCeiling())
	})
	t.Run("off disables the bound", func(t *testing.T) {
		t.Setenv("GORTEX_LSP_ENRICH_TIMEOUT", "off")
		assert.Equal(t, time.Duration(0), enrichOuterCeiling())
	})
	t.Run("garbage falls back to the ceiling", func(t *testing.T) {
		t.Setenv("GORTEX_LSP_ENRICH_TIMEOUT", "not-a-duration")
		assert.Equal(t, maxEnrichRepoTimeout, enrichOuterCeiling())
	})
}

// TestManager_EnrichOne_RecordsLazyDeadlineOnStatus verifies the lazy
// budget: a ContextEnricher derives its per-repo deadline from the
// candidate count it reports (not the whole-repo node count), and the
// Manager surfaces that value on the enrichment status. A small candidate
// set lands near the floor, well under the ceiling; a large cold set
// retains headroom toward the 90-minute ceiling.
func TestManager_EnrichOne_RecordsLazyDeadlineOnStatus(t *testing.T) {
	// No override: the lazy policy scales with the reported candidate count.
	t.Setenv("GORTEX_LSP_ENRICH_TIMEOUT", "")

	run := func(t *testing.T, candidates int) EnrichmentStatus {
		t.Helper()
		cfg := Config{
			Enabled: true,
			Providers: []ProviderConfig{
				{Name: "ctx-go", Languages: []string{"go"}, Priority: 1, Enabled: true},
			},
		}
		mgr := NewManager(cfg, zap.NewNop())
		mgr.RegisterProvider(&mockCtxProvider{
			mockProvider: mockProvider{name: "ctx-go", languages: []string{"go"}, available: true},
			enrichCtxFunc: func(ctx context.Context, g graph.Store, repoPrefix, repoRoot string, deadline EnrichDeadlinePolicy) (*EnrichResult, error) {
				// Size the window from the (simulated) post-filter candidate
				// count and report it, exactly as the real provider does.
				d := deadline(candidates)
				return &EnrichResult{
					Provider:        "ctx-go",
					Language:        "go",
					HoverCandidates: candidates,
					BudgetSeconds:   d.Seconds(),
				}, nil
			},
		})
		g := graph.New()
		g.AddNode(&graph.Node{ID: "main.go::main", Kind: graph.KindFunction, Name: "main", FilePath: "main.go", Language: "go"})
		_, _, err := mgr.EnrichAll(g, map[string]string{"default": "/tmp/test"}, EnrichOptions{})
		require.NoError(t, err)
		statuses := mgr.EnrichmentStatuses()
		require.Len(t, statuses, 1)
		return statuses[0]
	}

	t.Run("few candidates land near the floor, under the ceiling", func(t *testing.T) {
		st := run(t, 200)
		assert.InDelta(t, scaleEnrichTimeout(200).Seconds(), st.DeadlineSeconds, 0.001)
		assert.Less(t, st.DeadlineSeconds, maxEnrichRepoTimeout.Seconds())
	})
	t.Run("a large cold candidate set keeps headroom toward the ceiling", func(t *testing.T) {
		st := run(t, 10_000_000)
		assert.Equal(t, maxEnrichRepoTimeout.Seconds(), st.DeadlineSeconds)
	})
}

// TestEnrichRepoTimeout_EnvResolution verifies the env override wins
// verbatim over the scaled default, the off switch disables the bound,
// and garbage falls back to the scaled value.
func TestEnrichRepoTimeout_EnvResolution(t *testing.T) {
	t.Run("unset scales with node count", func(t *testing.T) {
		t.Setenv("GORTEX_LSP_ENRICH_TIMEOUT", "")
		assert.Equal(t, scaleEnrichTimeout(50_000), enrichRepoTimeout(50_000))
	})
	t.Run("explicit override wins verbatim", func(t *testing.T) {
		t.Setenv("GORTEX_LSP_ENRICH_TIMEOUT", "5m")
		assert.Equal(t, 5*time.Minute, enrichRepoTimeout(1_000_000))
	})
	t.Run("off disables the bound", func(t *testing.T) {
		t.Setenv("GORTEX_LSP_ENRICH_TIMEOUT", "off")
		assert.Equal(t, time.Duration(0), enrichRepoTimeout(1_000_000))
	})
	t.Run("garbage falls back to the scaled default", func(t *testing.T) {
		t.Setenv("GORTEX_LSP_ENRICH_TIMEOUT", "not-a-duration")
		assert.Equal(t, scaleEnrichTimeout(123), enrichRepoTimeout(123))
	})
}
