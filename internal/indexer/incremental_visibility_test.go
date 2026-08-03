package indexer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/search"
)

// R4 (PR #432 review): the incremental reuse key is source-side shape
// only — it carries no evidence about the USING VISIBILITY the original
// verdict was narrowed under. A using edit changes which same-shape
// extension bind is correct, so reuse (and the prior-unresolved skip)
// must be dropped when the file's visibility stamps changed, and a
// global-using edit must re-resolve the files it is visible to.

const csVisCrate = `namespace W {
    public class Crate {}
}
`

const csVisExtA = `using W;
namespace LibA {
    public static class E1 {
        public static void Foo(this Crate c) {}
    }
}
`

const csVisExtB = `using W;
namespace LibB {
    public static class E2 {
        public static void Foo(this Crate c) {}
    }
}
`

func newCSVisIndexer(t *testing.T, dir string) (graph.Store, *Indexer) {
	t.Helper()
	g := graph.New()
	reg := parser.NewRegistry()
	reg.Register(languages.NewCSharpExtractor())
	idx := New(g, reg, config.IndexConfig{Workers: 1}, zap.NewNop())
	idx.search = search.NewBM25()
	idx.SetRootPath(dir)
	_, err := idx.IndexCtx(testCtx(), dir)
	require.NoError(t, err)
	return g, idx
}

// fooCallEdgeTarget returns the To of the EdgeCalls edge for the
// `c.Foo()` member call leaving fromID, resolved or stubbed.
func fooCallEdgeTarget(t *testing.T, g graph.Store, fromID string) string {
	t.Helper()
	for _, e := range g.GetOutEdges(fromID) {
		if e.Kind == graph.EdgeCalls && strings.Contains(e.To, "Foo") {
			return e.To
		}
	}
	t.Fatalf("no Foo call edge from %s", fromID)
	return ""
}

// TestIncrementalSave_UsingsEditInvalidatesExtensionReuse: swapping
// `using LibA;` for `using LibB;` flips which same-name extension the
// call site can see. The reuse snapshot predates the edit — restoring
// the captured LibA bind would resurrect a target the new visibility
// refuses, so a visibility-stamp change must drop the reuse wholesale
// and let the full resolver re-narrow.
func TestIncrementalSave_UsingsEditInvalidatesExtensionReuse(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "W.cs"), csVisCrate)
	writeFile(t, filepath.Join(dir, "E1.cs"), csVisExtA)
	writeFile(t, filepath.Join(dir, "E2.cs"), csVisExtB)
	callerPath := filepath.Join(dir, "Caller.cs")
	writeFile(t, callerPath, `using W;
using LibA;
namespace App {
    public class R {
        public void M() {
            Crate c = new Crate();
            c.Foo();
        }
    }
}
`)
	g, idx := newCSVisIndexer(t, dir)
	mID := fnNodeID(t, g, "Caller.cs", "M")
	require.Contains(t, fooCallEdgeTarget(t, g, mID), "E1.Foo",
		"baseline: using LibA narrows the call to LibA's extension")

	writeFile(t, callerPath, `using W;
using LibB;
namespace App {
    public class R {
        public void M() {
            Crate c = new Crate();
            c.Foo();
        }
    }
}
`)
	require.NoError(t, idx.IndexFile(callerPath))
	assert.Contains(t, fooCallEdgeTarget(t, g, mID), "E2.Foo",
		"after the using swap only LibB is visible — reuse must not restore the stale LibA bind")
}

// TestIncrementalSave_AddedUsingBindsPriorUnresolved: the mirror image.
// With neither library imported the two-way ambiguity keeps the call
// unresolved; adding `using LibA;` makes exactly one candidate visible.
// The prior-unresolved skip ("unchanged shape will not bind now either")
// is unsound across a visibility change and must be dropped with it.
func TestIncrementalSave_AddedUsingBindsPriorUnresolved(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "W.cs"), csVisCrate)
	writeFile(t, filepath.Join(dir, "E1.cs"), csVisExtA)
	writeFile(t, filepath.Join(dir, "E2.cs"), csVisExtB)
	callerPath := filepath.Join(dir, "Caller.cs")
	writeFile(t, callerPath, `using W;
namespace App {
    public class R {
        public void M() {
            Crate c = new Crate();
            c.Foo();
        }
    }
}
`)
	g, idx := newCSVisIndexer(t, dir)
	mID := fnNodeID(t, g, "Caller.cs", "M")
	require.True(t, graph.IsUnresolvedTarget(fooCallEdgeTarget(t, g, mID)),
		"baseline: with neither extension namespace visible the two-way tie stays unresolved")

	writeFile(t, callerPath, `using W;
using LibA;
namespace App {
    public class R {
        public void M() {
            Crate c = new Crate();
            c.Foo();
        }
    }
}
`)
	require.NoError(t, idx.IndexFile(callerPath))
	assert.Contains(t, fooCallEdgeTarget(t, g, mID), "E1.Foo",
		"the added using resolves the ambiguity — the prior-unresolved skip must not park the call")
}

// TestIncrementalSave_GlobalUsingEditReresolvesDependents: a global
// using is visible to every file of its compilation unit, so editing
// one is an edit to every dependent file's visibility. Saving ONLY the
// global-usings file must re-resolve the dependents' extension binds
// under the new visibility — nothing else ever touches them.
func TestIncrementalSave_GlobalUsingEditReresolvesDependents(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "W.cs"), csVisCrate)
	writeFile(t, filepath.Join(dir, "E1.cs"), csVisExtA)
	writeFile(t, filepath.Join(dir, "E2.cs"), csVisExtB)
	usingsPath := filepath.Join(dir, "Usings.cs")
	writeFile(t, usingsPath, "global using LibA;\n")
	writeFile(t, filepath.Join(dir, "Caller.cs"), `using W;
namespace App {
    public class R {
        public void M() {
            Crate c = new Crate();
            c.Foo();
        }
    }
}
`)
	g, idx := newCSVisIndexer(t, dir)
	mID := fnNodeID(t, g, "Caller.cs", "M")
	require.Contains(t, fooCallEdgeTarget(t, g, mID), "E1.Foo",
		"baseline: the global using narrows the call to LibA's extension")

	writeFile(t, usingsPath, "global using LibB;\n")
	require.NoError(t, idx.IndexFile(usingsPath))
	assert.Contains(t, fooCallEdgeTarget(t, g, mID), "E2.Foo",
		"the global-using swap changed every dependent's visibility — Caller.cs must re-bind to LibB")
}

// TestIncrementalEvict_GlobalUsingsFileReresolvesDependents: deleting
// the global-usings file removes the visibility every dependent's
// extension bind was narrowed under. Eviction must re-price them —
// with the visibility gone the two-way tie is the correct verdict.
func TestIncrementalEvict_GlobalUsingsFileReresolvesDependents(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "W.cs"), csVisCrate)
	writeFile(t, filepath.Join(dir, "E1.cs"), csVisExtA)
	writeFile(t, filepath.Join(dir, "E2.cs"), csVisExtB)
	usingsPath := filepath.Join(dir, "Usings.cs")
	writeFile(t, usingsPath, "global using LibA;\n")
	writeFile(t, filepath.Join(dir, "Caller.cs"), `using W;
namespace App {
    public class R {
        public void M() {
            Crate c = new Crate();
            c.Foo();
        }
    }
}
`)
	g, idx := newCSVisIndexer(t, dir)
	mID := fnNodeID(t, g, "Caller.cs", "M")
	require.Contains(t, fooCallEdgeTarget(t, g, mID), "E1.Foo",
		"baseline: the global using narrows the call to LibA's extension")

	idx.EvictFile(usingsPath)
	assert.True(t, graph.IsUnresolvedTarget(fooCallEdgeTarget(t, g, mID)),
		"with the global using deleted neither extension namespace is visible — the stale LibA bind must not survive the eviction")
}

// TestIncrementalBatchDelete_GlobalUsingsFileReresolvesDependents: the
// BATCHED deletion path — the one fsnotify deletes and git branch
// switches actually take (IncrementalReindexPaths over a file gone from
// disk) — must re-price global-using dependents exactly like the
// single-file eviction API does.
func TestIncrementalBatchDelete_GlobalUsingsFileReresolvesDependents(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "W.cs"), csVisCrate)
	writeFile(t, filepath.Join(dir, "E1.cs"), csVisExtA)
	writeFile(t, filepath.Join(dir, "E2.cs"), csVisExtB)
	usingsPath := filepath.Join(dir, "Usings.cs")
	writeFile(t, usingsPath, "global using LibA;\n")
	writeFile(t, filepath.Join(dir, "Caller.cs"), `using W;
namespace App {
    public class R {
        public void M() {
            Crate c = new Crate();
            c.Foo();
        }
    }
}
`)
	g, idx := newCSVisIndexer(t, dir)
	mID := fnNodeID(t, g, "Caller.cs", "M")
	require.Contains(t, fooCallEdgeTarget(t, g, mID), "E1.Foo",
		"baseline: the global using narrows the call to LibA's extension")

	require.NoError(t, os.Remove(usingsPath))
	_, err := idx.IncrementalReindexPaths(dir, []string{usingsPath})
	require.NoError(t, err)
	assert.True(t, graph.IsUnresolvedTarget(fooCallEdgeTarget(t, g, mID)),
		"the batched deletion path removed the unit's visibility — the stale LibA bind must not survive")
}

// TestIncrementalDeferredFallback_GlobalUsingEditReresolvesDependents:
// a file that cannot enter the prepared structural batch falls back to
// indexFile(resolve=false) under a deferred batch — the deferred
// catch-up re-resolves only the file's own frontier, so the unit-wide
// dependents re-price must still run on this exceptional path.
func TestIncrementalDeferredFallback_GlobalUsingEditReresolvesDependents(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "W.cs"), csVisCrate)
	writeFile(t, filepath.Join(dir, "E1.cs"), csVisExtA)
	writeFile(t, filepath.Join(dir, "E2.cs"), csVisExtB)
	usingsPath := filepath.Join(dir, "Usings.cs")
	writeFile(t, usingsPath, "global using LibA;\n")
	writeFile(t, filepath.Join(dir, "Caller.cs"), `using W;
namespace App {
    public class R {
        public void M() {
            Crate c = new Crate();
            c.Foo();
        }
    }
}
`)
	g, idx := newCSVisIndexer(t, dir)
	mID := fnNodeID(t, g, "Caller.cs", "M")
	require.Contains(t, fooCallEdgeTarget(t, g, mID), "E1.Foo",
		"baseline: the global using narrows the call to LibA's extension")

	writeFile(t, usingsPath, "global using LibB;\n")
	require.NoError(t, idx.indexFile(usingsPath, false,
		&reparsePendingEnrichmentBatch{deferResolverCatchup: true}))
	assert.Contains(t, fooCallEdgeTarget(t, g, mID), "E2.Foo",
		"the fallback path changed unit-wide visibility — dependents must re-bind to E2")
}

// TestIncrementalSave_GlobalUsingStaticEditReresolvesDependents: the
// static form of a global using is compilation-scoped too — swapping
// its target class must re-price every dependent's extension binds.
func TestIncrementalSave_GlobalUsingStaticEditReresolvesDependents(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "W.cs"), csVisCrate)
	writeFile(t, filepath.Join(dir, "E1.cs"), csVisExtA)
	writeFile(t, filepath.Join(dir, "E2.cs"), csVisExtB)
	usingsPath := filepath.Join(dir, "Usings.cs")
	writeFile(t, usingsPath, "global using static LibA.E1;\n")
	writeFile(t, filepath.Join(dir, "Caller.cs"), `using W;
namespace App {
    public class R {
        public void M() {
            Crate c = new Crate();
            c.Foo();
        }
    }
}
`)
	g, idx := newCSVisIndexer(t, dir)
	mID := fnNodeID(t, g, "Caller.cs", "M")
	require.Contains(t, fooCallEdgeTarget(t, g, mID), "E1.Foo",
		"baseline: the global using static admits E1's extensions unit-wide")

	writeFile(t, usingsPath, "global using static LibB.E2;\n")
	require.NoError(t, idx.IndexFile(usingsPath))
	assert.Contains(t, fooCallEdgeTarget(t, g, mID), "E2.Foo",
		"the global-using-static swap changed every dependent's visibility — Caller.cs must re-bind to E2")
}
