// This file is package semantic_test rather than package semantic on purpose:
// internal/semantic/tstypes imports internal/semantic, so only the EXTERNAL
// test package can dispatch a real tstypes provider through the real Manager.
// The routing this pins has no in-package stand-in — a hand-written fake would
// only prove that a fake implements the interface it was written to implement.
package semantic_test

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/semantic"
	"github.com/zzet/gortex/internal/semantic/tstypes"
)

const pyDispatchSvc = `class Svc:
    def run(self):
        pass
`

func pyDispatchCaller(fn string) string {
	return `from app.svc import Svc


def ` + fn + `():
    s = Svc()
    s.run()
`
}

// indexPythonFixture writes and indexes a small python corpus with the real
// extractors, so the graph carries the node-ID and unresolved-edge conventions
// the daemon's index produces.
func indexPythonFixture(t *testing.T, files map[string]string) (*graph.Graph, string) {
	t.Helper()
	dir := t.TempDir()
	g := graph.New()
	reg := parser.NewRegistry()
	languages.RegisterAll(reg)
	for rel, content := range files {
		abs := filepath.Join(dir, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(abs), 0o755))
		require.NoError(t, os.WriteFile(abs, []byte(content), 0o644))
		lang, ok := reg.DetectLanguage(rel)
		require.Truef(t, ok, "no language for %s", rel)
		ext, ok := reg.GetByLanguage(lang)
		require.Truef(t, ok, "no extractor for %s", lang)
		res, err := ext.Extract(rel, []byte(content))
		require.NoError(t, err)
		if res.Tree != nil {
			res.Tree.Close()
		}
		g.AddBatch(res.Nodes, res.Edges)
	}
	return g, dir
}

// enrichedBy reports whether any out-edge of the named function carries the
// provider's stamp.
func enrichedBy(t *testing.T, g *graph.Graph, fn, provider string) bool {
	t.Helper()
	var node *graph.Node
	for _, candidate := range g.FindNodesByName(fn) {
		if candidate.Kind == graph.KindFunction {
			node = candidate
			break
		}
	}
	require.NotNilf(t, node, "no function node named %q", fn)
	for _, e := range g.GetOutEdges(node.ID) {
		if e.Meta != nil && e.Meta["semantic_source"] == provider {
			return true
		}
	}
	return false
}

// tstypesRouteSpy records which rung of EnrichFilesContext's dispatch ladder a
// real tstypes provider lands on, then delegates to it unchanged.
type tstypesRouteSpy struct {
	*tstypes.Provider
	mu        sync.Mutex
	batchCall int
	repoCall  int
	sawFiles  []string
}

func (s *tstypesRouteSpy) EnrichFilesContext(
	ctx context.Context, g graph.Store, repoPrefix, repoRoot string, filePaths []string,
) (*semantic.EnrichResult, error) {
	s.mu.Lock()
	s.batchCall++
	s.sawFiles = append([]string(nil), filePaths...)
	s.mu.Unlock()
	return s.Provider.EnrichFilesContext(ctx, g, repoPrefix, repoRoot, filePaths)
}

func (s *tstypesRouteSpy) EnrichRepoContext(
	ctx context.Context, g graph.Store, repoPrefix, repoRoot string, deadline semantic.EnrichDeadlinePolicy,
) (*semantic.EnrichResult, error) {
	s.mu.Lock()
	s.repoCall++
	s.mu.Unlock()
	return s.Provider.EnrichRepoContext(ctx, g, repoPrefix, repoRoot, deadline)
}

func (s *tstypesRouteSpy) observed() (batch, repo int, files []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.batchCall, s.repoCall, s.sawFiles
}

// repoOnlyProvider is a tstypes provider with its batch faces HIDDEN — the
// shape every tstypes provider had before EnrichFilesContext existed. It is the
// negative control for the test below: same corpus, same frontier, and the
// ladder's whole-repository rung enriches everything.
type repoOnlyProvider struct {
	inner    *tstypes.Provider
	repoCall int
}

func (p *repoOnlyProvider) Name() string        { return p.inner.Name() }
func (p *repoOnlyProvider) Languages() []string { return p.inner.Languages() }
func (p *repoOnlyProvider) Available() bool     { return true }
func (p *repoOnlyProvider) Close() error        { return nil }
func (p *repoOnlyProvider) Supplemental() bool  { return true }

func (p *repoOnlyProvider) Enrich(g graph.Store, repoRoot string) (*semantic.EnrichResult, error) {
	return p.inner.Enrich(g, repoRoot)
}

func (p *repoOnlyProvider) EnrichFile(g graph.Store, repoRoot, filePath string) (*semantic.EnrichResult, error) {
	return p.inner.EnrichFile(g, repoRoot, filePath)
}

func (p *repoOnlyProvider) EnrichRepoContext(
	ctx context.Context, g graph.Store, repoPrefix, repoRoot string, deadline semantic.EnrichDeadlinePolicy,
) (*semantic.EnrichResult, error) {
	p.repoCall++
	return p.inner.EnrichRepoContext(ctx, g, repoPrefix, repoRoot, deadline)
}

// A tstypes frontier must reach the batch rung of EnrichFilesContext's ladder.
//
// The rungs below it enrich the WHOLE repository and discard the frontier, so
// which rung a provider lands on is not a detail: measured on a 9.8k-file
// python workspace, a worktree-copy repair over 82 files ran the full
// python-types pass — 488 s alone, 89 min under contention — because tstypes
// implemented neither batch face.
func TestAManagerRoutesATstypesFrontierToTheBatchPath(t *testing.T) {
	g, dir := indexPythonFixture(t, map[string]string{
		"app/svc.py": pyDispatchSvc,
		"app/a.py":   pyDispatchCaller("a"),
		"app/b.py":   pyDispatchCaller("b"),
	})

	spy := &tstypesRouteSpy{Provider: tstypes.NewProvider(tstypes.PythonSpec(), zap.NewNop())}
	m := semantic.NewManager(semantic.Config{Enabled: true}, zap.NewNop())
	defer m.Close()
	m.RegisterProvider(spy)

	result, err := m.EnrichFilesContext(
		context.Background(), g, "", dir, "python", []string{"app/a.py"})
	require.NoError(t, err)
	require.NotNil(t, result)
	require.False(t, result.Partial)

	batch, repo, files := spy.observed()
	assert.Equal(t, 1, batch, "the frontier must reach EnrichFilesContext")
	assert.Zero(t, repo, "the whole-repository rung must not be reached")
	assert.Equal(t, []string{"app/a.py"}, files)

	assert.True(t, enrichedBy(t, g, "a", "python-types"), "the frontier file was enriched")
	assert.False(t, enrichedBy(t, g, "b", "python-types"),
		"app/b.py was not in the frontier and must not have been enriched")
}

// The negative control for the test above, and the reason the batch faces
// exist: with them hidden, the identical frontier lands on the ladder's
// whole-repository rung and enriches a file the caller never named.
func TestAProviderWithoutABatchFaceStillFallsThroughToTheWholeRepository(t *testing.T) {
	g, dir := indexPythonFixture(t, map[string]string{
		"app/svc.py": pyDispatchSvc,
		"app/a.py":   pyDispatchCaller("a"),
		"app/b.py":   pyDispatchCaller("b"),
	})

	provider := &repoOnlyProvider{inner: tstypes.NewProvider(tstypes.PythonSpec(), zap.NewNop())}
	m := semantic.NewManager(semantic.Config{Enabled: true}, zap.NewNop())
	defer m.Close()
	m.RegisterProvider(provider)

	// Two files so the ladder cannot take its single-file EnrichFile rung.
	_, err := m.EnrichFilesContext(
		context.Background(), g, "", dir, "python", []string{"app/a.py", "app/svc.py"})
	require.NoError(t, err)

	assert.Equal(t, 1, provider.repoCall)
	assert.True(t, enrichedBy(t, g, "b", "python-types"),
		"the whole-repository rung enriches app/b.py, which was never in the frontier")
}
