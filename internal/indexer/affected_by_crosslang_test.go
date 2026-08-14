package indexer

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// TestAffectedBy_TypeScriptParamChange_ReresolvesCaller is the
// cross-language proof for the signature delta. TypeScript stamps only a
// name-bearing Meta["signature"] ("function F()") that does not move when
// a parameter is added — so a signature-only delta is blind to it and the
// caller is never re-resolved. The delta now folds in the parameter
// structure the extractor emits (KindParam nodes via EdgeParamOf), so
// adding a parameter to F re-resolves the file that calls it.
func TestAffectedBy_TypeScriptParamChange_ReresolvesCaller(t *testing.T) {
	dir := t.TempDir()
	aPath := filepath.Join(dir, "a.ts")
	bPath := filepath.Join(dir, "b.ts")
	writeFile(t, aPath, "export function F(x: number): number {\n  return x\n}\n")
	writeFile(t, bPath, "import { F } from './a'\n\nexport function Caller(): number {\n  return F(1)\n}\n")

	idx, _ := newSQLiteIndexer(t)
	_, err := idx.Index(dir)
	require.NoError(t, err)
	idx.ResolveAll()

	g := idx.Graph()
	callerID := fnNodeID(t, g, "b.ts", "Caller")
	require.Equal(t, "a.ts::F", callTargetFrom(t, g, callerID),
		"baseline: Caller must bind to the local F")

	// Add a parameter — the name-only "function F()" signature is
	// unchanged; only the parameter structure differs.
	bumpMtime(t, aPath, "export function F(x: number, y: number): number {\n  return x + y\n}\n")
	_, err = idx.IncrementalReindexPaths(dir, []string{aPath})
	require.NoError(t, err)

	assert.Equal(t, "a.ts::F", callTargetFrom(t, g, callerID),
		"the caller must be re-resolved to the fresh F")
}

// TestAffectedBy_TypeScriptBodyOnly_NoFanout is the gate counterpart for
// the structural shape: a TypeScript body edit that leaves the parameter
// list untouched must not fan out — proving the structural shape is body-
// insensitive, not just a coarse "anything changed" trigger.
func TestAffectedBy_TypeScriptBodyOnly_NoFanout(t *testing.T) {
	dir := t.TempDir()
	aPath := filepath.Join(dir, "a.ts")
	bPath := filepath.Join(dir, "b.ts")
	writeFile(t, aPath, "export function F(x: number): number {\n  return x\n}\n")
	writeFile(t, bPath, "import { F } from './a'\n\nexport function Caller(): number {\n  return F(1)\n}\n")

	idx, _ := newSQLiteIndexer(t)
	_, err := idx.Index(dir)
	require.NoError(t, err)
	idx.ResolveAll()
	g := idx.Graph()
	snap := idx.snapshotAffectedBy("a.ts")
	require.NotNil(t, snap)

	bumpMtime(t, aPath, "export function F(x: number): number {\n  return x + 1 + 2 + 3\n}\n")
	_, err = idx.IncrementalReindexPaths(dir, []string{aPath})
	require.NoError(t, err)

	var fresh []*graph.Node
	for _, node := range g.AllNodes() {
		if node != nil && node.FilePath == "a.ts" {
			fresh = append(fresh, node)
		}
	}
	assert.Empty(t, affectedByDelta(g, snap, fresh),
		"a TypeScript body-only edit must not produce an affected-by frontier")
}

// TestAffectedByDelta_LineSuffixedID_NoSpuriousDelta proves the line-
// insensitive identity. Several languages embed a definition's start line
// in its node ID (TS/JS `name@<line>`, the `_L<line>` overload
// disambiguator). A body-only edit ABOVE such a symbol shifts its line and
// rewrites its ID; keying the delta on the raw ID would read that as a
// remove + add and fire on every line-shifting edit. The delta is keyed on
// kind + name instead, so an ID that differs only in its line-suffix —
// with an identical shape — yields no delta.
func TestAffectedByDelta_LineSuffixedID_NoSpuriousDelta(t *testing.T) {
	g := graph.New()
	// Snapshot: an object member whose ID embeds its line.
	old := &graph.Node{
		ID: "a.ts::api.health@4", Kind: graph.KindFunction, Name: "api.health",
		FilePath: "a.ts", Meta: map[string]any{"signature": "api.health()"},
	}
	g.AddNode(old)
	key := stableSymbolKey(old)
	snap := &affectedBySnapshot{
		symbols:    map[string]symbolShape{key: {kind: old.Kind, shape: symbolShapeFor(g, old) + "\n"}},
		refSources: map[string]map[string]struct{}{},
		idsByKey:   map[string][]string{key: {old.ID}},
	}

	// After a body-only edit above it, the same member is re-minted at a
	// new line — new ID, identical name/kind/shape.
	shifted := &graph.Node{
		ID: "a.ts::api.health@9", Kind: graph.KindFunction, Name: "api.health",
		FilePath: "a.ts", Meta: map[string]any{"signature": "api.health()"},
	}
	g2 := graph.New()
	g2.AddNode(shifted)

	delta := affectedByDelta(g2, snap, []*graph.Node{shifted})
	assert.Empty(t, delta,
		"a line-only ID shift with an unchanged shape must not be a delta")

	// Control: a genuine kind change at the same name IS a delta.
	retyped := &graph.Node{
		ID: "a.ts::api.health@9", Kind: graph.KindVariable, Name: "api.health",
		FilePath: "a.ts", Meta: map[string]any{"signature": "api.health()"},
	}
	g3 := graph.New()
	g3.AddNode(retyped)
	delta = affectedByDelta(g3, snap, []*graph.Node{retyped})
	assert.Equal(t, []string{key}, delta,
		"a kind change under the same name must still be a delta")
}

// TestAffectedBy_MinifiedSkip_PreservesFactsNoFanout is the parse-failure
// guard. When a file that previously parsed cleanly is overwritten with a
// minified bundle, the incremental path yields a synthetic skip node with
// zero symbols. That must NOT be read as "every symbol was removed": the
// affected-by pass must not fan out, and the caller's persisted reference
// fact must survive — a transient un-parseable save must not durably
// delete reverse-lookup rows that won't be rebuilt until a clean reparse.
func TestAffectedBy_MinifiedSkip_PreservesFactsNoFanout(t *testing.T) {
	dir := t.TempDir()
	aPath := filepath.Join(dir, "a.js")
	bPath := filepath.Join(dir, "b.js")
	writeFile(t, aPath, "export function F(x) {\n  return x\n}\n")
	writeFile(t, bPath, "import { F } from './a.js'\n\nexport function Caller() {\n  return F(1)\n}\n")

	idx, store := newSQLiteIndexer(t)
	_, err := idx.Index(dir)
	require.NoError(t, err)
	idx.ResolveAll()

	g := idx.Graph()
	fID := fnNodeID(t, g, "a.js", "F")
	before, err := store.LoadRefFactsByTargets("", []string{fID})
	require.NoError(t, err)
	require.Contains(t, before, "b.js",
		"baseline: the sidecar must record b.js referencing F")

	// Overwrite a.js with a minified bundle: one long line over the
	// minified-detection floor. extractFile classifies it as a build
	// artifact and returns a synthetic skip node — zero symbols.
	blob := "var x=" + strings.Repeat("1+", 1500) + "1;\n"
	bumpMtime(t, aPath, blob)
	_, err = idx.IncrementalReindexPaths(dir, []string{aPath})
	require.NoError(t, err)

	after, err := store.LoadRefFactsByTargets("", []string{fID})
	require.NoError(t, err)
	assert.Contains(t, after, "b.js",
		"a transient parse-skip must not delete the caller's persisted reference fact")
}

// TestAffectedBy_NoDeltaPath_DefersReferrerLookup proves the cheap no-
// delta path: the pre-evict snapshot must record referrer NODE IDs rather
// than eagerly resolving each to its file, so a body-only edit pays no
// per-referrer node lookup. We assert the snapshot's structure directly —
// refSources holds source node IDs, and resolving them to files is the
// delta path's job, reached only when a delta exists.
func TestAffectedBy_NoDeltaPath_DefersReferrerLookup(t *testing.T) {
	dir := t.TempDir()
	aPath := filepath.Join(dir, "a.go")
	bPath := filepath.Join(dir, "b.go")
	writeFile(t, aPath, "package p\n\nfunc F(x int) int { return x }\n")
	writeFile(t, bPath, "package p\n\nfunc Caller() int { return F(1) }\n")

	g := graph.New()
	idx := newTestIndexer(g)
	_, err := idx.Index(dir)
	require.NoError(t, err)

	snap := idx.snapshotAffectedBy("a.go")
	require.NotNil(t, snap)
	key := "function\x00F"
	require.Contains(t, snap.refSources, key, "F's referrers must be captured")
	// The recorded source is the caller's NODE ID, not its file path —
	// the file is resolved lazily only on the delta path.
	callerID := fnNodeID(t, g, "b.go", "Caller")
	_, ok := snap.refSources[key][callerID]
	assert.True(t, ok, "refSources must hold the referrer node ID, deferring its file lookup")
}
