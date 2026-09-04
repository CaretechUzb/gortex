package semantic

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/graph"
)

// budgetBurningProvider models the shape measured on `addons` python-types:
// every pass runs out its deadline, covers nothing, and lands no edge — while
// holding the store-wide resolve mutex for the whole apply.
type budgetBurningProvider struct {
	name      string
	languages []string
	calls     int
	// result overrides the futile outcome so a test can show a pass that made
	// progress, or one interrupted rather than timed out, is NOT suppressed.
	result *EnrichResult
}

func (b *budgetBurningProvider) Name() string        { return b.name }
func (b *budgetBurningProvider) Languages() []string { return b.languages }
func (b *budgetBurningProvider) Available() bool     { return true }
func (b *budgetBurningProvider) Close() error        { return nil }

func (b *budgetBurningProvider) Enrich(g graph.Store, repoRoot string) (*EnrichResult, error) {
	return nil, nil
}

func (b *budgetBurningProvider) EnrichFile(g graph.Store, repoRoot, filePath string) (*EnrichResult, error) {
	return nil, nil
}

func (b *budgetBurningProvider) EnrichRepoContext(ctx context.Context, g graph.Store, repoPrefix, repoRoot string, deadline EnrichDeadlinePolicy) (*EnrichResult, error) {
	b.calls++
	if b.result != nil {
		out := *b.result
		out.Provider, out.Language = b.name, b.languages[0]
		return &out, nil
	}
	return &EnrichResult{
		Provider:     b.name,
		Language:     b.languages[0],
		Partial:      true,
		AbortReason:  context.DeadlineExceeded.Error(),
		BoundReason:  EnrichBoundBudget,
		SymbolsTotal: 106431,
	}, nil
}

const futileSHA = "038bba933ed048313868ce0fb8b8ccde08b6c32f"

func futileRepoState() RepoEnrichState { return RepoEnrichState{SHA: futileSHA} }

// TestAPassThatBurnsItsBudgetForNothingIsNotRepeated is the regression gate for
// the 18-minute dead pass.
//
// Measured twice on `addons` python-types at the same revision: 1,495s of
// budget, ~1,088s of it inside the apply holding the store-wide resolve mutex,
// partial, coverage 0, zero edges. The deferred-enrichment pool runs on warmup,
// after a copy-track and from the janitor sweep, and each trigger used to pay
// that cost again for a result already known.
func TestAPassThatBurnsItsBudgetForNothingIsNotRepeated(t *testing.T) {
	core, logs := observer.New(zap.WarnLevel)
	mgr := NewManager(Config{Enabled: true}, zap.New(core))
	p := &budgetBurningProvider{name: "python-types", languages: []string{"python"}}
	g := graph.New()

	partial := map[string]bool{}
	results := mgr.runEnrichOne(g, "addons", "/tmp/addons", "python", p, 10, futileRepoState(), nil, nil, partial)
	require.Len(t, results, 1, "the first pass must actually run")
	require.Equal(t, 1, p.calls)
	require.True(t, partial["addons"])

	results = mgr.runEnrichOne(g, "addons", "/tmp/addons", "python", p, 10, futileRepoState(), nil, results, partial)
	assert.Equal(t, 1, p.calls, "a second trigger at the same revision must not re-run the pass")
	assert.Len(t, results, 1, "a skipped pass appends no result")
	assert.True(t, partial["addons"], "the repo stays honestly partial; it is not stamped complete")

	require.Len(t,
		logs.FilterMessage("semantic enrichment skipped: previous pass exhausted its budget with zero coverage").All(), 1,
		"the skip must say why, at a level an operator sees")
}

// TestANewRevisionEarnsAFreshAttempt keeps the record from becoming permanent.
// The content is what made the pass impossible; different content is a
// different question.
func TestANewRevisionEarnsAFreshAttempt(t *testing.T) {
	mgr := NewManager(Config{Enabled: true}, zap.NewNop())
	p := &budgetBurningProvider{name: "python-types", languages: []string{"python"}}
	g := graph.New()

	mgr.runEnrichOne(g, "addons", "/tmp/addons", "python", p, 10, futileRepoState(), nil, nil, map[string]bool{})
	require.Equal(t, 1, p.calls)

	mgr.runEnrichOne(g, "addons", "/tmp/addons", "python", p, 10,
		RepoEnrichState{SHA: "c9e2bbf500bcf7d22784b9665e51470ddb2f6892"}, nil, nil, map[string]bool{})
	assert.Equal(t, 2, p.calls, "a different revision must be attempted")
}

// TestAPartialPassThatMadeProgressIsRetried is the boundary that keeps the guard
// narrow. A pass cut by its deadline AFTER landing edges is the ordinary
// large-repo case, and suppressing it would freeze that repo half-enriched.
func TestAPartialPassThatMadeProgressIsRetried(t *testing.T) {
	mgr := NewManager(Config{Enabled: true}, zap.NewNop())
	p := &budgetBurningProvider{
		name: "python-types", languages: []string{"python"},
		result: &EnrichResult{
			Partial:      true,
			AbortReason:  context.DeadlineExceeded.Error(),
			BoundReason:  EnrichBoundBudget,
			SymbolsTotal: 1000,
			EdgesAdded:   7,
		},
	}
	g := graph.New()

	for i := 0; i < 2; i++ {
		mgr.runEnrichOne(g, "addons", "/tmp/addons", "python", p, 10, futileRepoState(), nil, nil, map[string]bool{})
	}
	assert.Equal(t, 2, p.calls, "a pass that landed edges must be retried")
}

// TestAnInterruptedPassIsRetried is the other boundary: a pass stopped by a
// closing manager or a cancelled parent has shown nothing about the work, only
// that it was interrupted.
func TestAnInterruptedPassIsRetried(t *testing.T) {
	mgr := NewManager(Config{Enabled: true}, zap.NewNop())
	p := &budgetBurningProvider{
		name: "python-types", languages: []string{"python"},
		result: &EnrichResult{
			Partial:      true,
			AbortReason:  context.Canceled.Error(),
			BoundReason:  EnrichBoundBudget,
			SymbolsTotal: 1000,
		},
	}
	g := graph.New()

	for i := 0; i < 2; i++ {
		mgr.runEnrichOne(g, "addons", "/tmp/addons", "python", p, 10, futileRepoState(), nil, nil, map[string]bool{})
	}
	assert.Equal(t, 2, p.calls, "an interrupted pass must be retried")
}

// TestAFutilePassWithoutARevisionIsNotRecorded: with no sha there is nothing to
// key the record on, so "the same attempt" is not a statement that can be made
// and every trigger runs.
func TestAFutilePassWithoutARevisionIsNotRecorded(t *testing.T) {
	mgr := NewManager(Config{Enabled: true}, zap.NewNop())
	p := &budgetBurningProvider{name: "python-types", languages: []string{"python"}}
	g := graph.New()

	for i := 0; i < 2; i++ {
		mgr.runEnrichOne(g, "addons", "/tmp/addons", "python", p, 10, RepoEnrichState{}, nil, nil, map[string]bool{})
	}
	assert.Equal(t, 2, p.calls)
}
