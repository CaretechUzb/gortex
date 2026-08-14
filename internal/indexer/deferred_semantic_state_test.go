package indexer

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/semantic"
)

func TestDeferredEnrichmentReleasesEveryRepoStateAfterDrain(t *testing.T) {
	// One node per fixture repo: disable the admission floor so the test
	// exercises retain/release mechanics, not enrichment admission.
	t.Setenv("GORTEX_ENRICH_MIN_NODES", "0")
	const repoCount = 9
	g := graph.New()
	provider := &retainingRepoProvider{
		retained: make(map[string]struct{}),
		leased:   make(map[string]struct{}),
	}
	manager := semantic.NewManager(semantic.Config{Enabled: true}, zap.NewNop())
	manager.RegisterProvider(provider)

	mi := &MultiIndexer{
		graph:       g,
		indexers:    make(map[string]*Indexer, repoCount),
		logger:      zap.NewNop(),
		semanticMgr: manager,
	}
	for i := 0; i < repoCount; i++ {
		prefix := fmt.Sprintf("repo-%02d", i)
		root := t.TempDir()
		idx := &Indexer{
			graph:       g,
			rootPath:    root,
			repoPrefix:  prefix,
			logger:      zap.NewNop(),
			semanticMgr: manager,
		}
		idx.pendingEnrich.Store(true)
		mi.indexers[prefix] = idx
		g.AddNode(&graph.Node{
			ID: prefix + "/main.go::F", Kind: graph.KindFunction, Name: "F",
			FilePath: prefix + "/main.go", StartLine: 1, EndLine: 1,
			Language: "go", RepoPrefix: prefix,
		})
	}

	scheduled := mi.RunDeferredPassesAllResult(context.Background()).EnrichScheduled
	if scheduled != repoCount {
		t.Fatalf("scheduled enrichment = %d, want %d", scheduled, repoCount)
	}
	provider.mu.Lock()
	peak := provider.peak
	retained := len(provider.retained)
	leases := len(provider.leased)
	retains := provider.retains
	releases := provider.releases
	unleased := provider.unleased
	provider.mu.Unlock()
	// Contract passes now run only after the whole enrichment pool drains
	// (no contract mutation may overlap enrichment), so every enriched
	// repo's state is retained until its serial contract pass. That state
	// is the compact binding projection — cheap to hold — and the bound is
	// the repo count, not the lane count.
	if peak > repoCount {
		t.Fatalf("peak retained repo states = %d, want <= repo count %d", peak, repoCount)
	}
	if retained != 0 {
		t.Fatalf("retained repo states after deferred batch = %d, want 0", retained)
	}
	if leases != 0 || retains != repoCount || unleased {
		t.Fatalf("deferred leases: active=%d retains=%d unleased_enrich=%v, want 0/%d/false", leases, retains, unleased, repoCount)
	}
	if releases != repoCount {
		t.Fatalf("released repo states = %d, want %d", releases, repoCount)
	}
}

type retainingRepoProvider struct {
	mu       sync.Mutex
	retained map[string]struct{}
	leased   map[string]struct{}
	peak     int
	retains  int
	releases int
	unleased bool
}

func (p *retainingRepoProvider) Name() string        { return "retaining-go" }
func (p *retainingRepoProvider) Languages() []string { return []string{"go"} }
func (p *retainingRepoProvider) Available() bool     { return true }
func (p *retainingRepoProvider) Close() error        { return nil }

func (p *retainingRepoProvider) Enrich(_ graph.Store, repoRoot string) (*semantic.EnrichResult, error) {
	return p.retain(repoRoot), nil
}

func (p *retainingRepoProvider) EnrichRepo(_ graph.Store, _ string, repoRoot string) (*semantic.EnrichResult, error) {
	return p.retain(repoRoot), nil
}

func (p *retainingRepoProvider) EnrichFile(graph.Store, string, string) (*semantic.EnrichResult, error) {
	return nil, nil
}

func (p *retainingRepoProvider) RetainRepoState(repoRoot string) bool {
	p.mu.Lock()
	p.leased[repoRoot] = struct{}{}
	p.retains++
	p.mu.Unlock()
	return true
}

func (p *retainingRepoProvider) ReleaseRepoState(repoRoot string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.leased, repoRoot)
	if _, ok := p.retained[repoRoot]; !ok {
		return false
	}
	delete(p.retained, repoRoot)
	p.releases++
	return true
}

func (p *retainingRepoProvider) retain(repoRoot string) *semantic.EnrichResult {
	p.mu.Lock()
	if _, ok := p.leased[repoRoot]; !ok {
		p.unleased = true
	}
	p.retained[repoRoot] = struct{}{}
	if len(p.retained) > p.peak {
		p.peak = len(p.retained)
	}
	p.mu.Unlock()
	return &semantic.EnrichResult{Provider: p.Name(), Language: "go"}
}
