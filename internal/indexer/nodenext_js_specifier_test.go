package indexer

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// importEdgeCount returns how many EdgeImports land directly on symbolID —
// the per-binding import edges find_usages reports as usages.
func importEdgeCount(t *testing.T, g graph.Store, symbolID string) int {
	t.Helper()
	n := 0
	for _, e := range g.GetInEdges(symbolID) {
		if e.Kind == graph.EdgeImports {
			n++
		}
	}
	return n
}

// TestNodeNextEmittedJSSpecifier_CallGraph covers issue #304: under
// `moduleResolution: node16` / `nodenext` a TypeScript module is imported
// by the name of the file the compiler EMITS (`./alpha.ts` is imported as
// `./alpha.js`), which is mandatory for ESM TypeScript. Probing that
// specifier only verbatim never matched the source on disk, so the import
// edge stayed unresolved and the cross-package guard reverted every call
// routed through it to `unresolved::*`.
//
// The reported symptom was "`../x` breaks, `./x` works", but the parent
// traversal is a confound: a same-directory call survives the guard
// anyway, because buildImportClosure seeds every file's own directory.
// The extension is the real variable — hence the extension-crossed matrix
// below, with a no-extension parent-directory row as the control that
// already passed before the fix.
func TestNodeNextEmittedJSSpecifier_CallGraph(t *testing.T) {
	cases := []struct {
		name       string
		calleeFile string
		callerFile string
		spec       string
	}{
		// Controls: no extension. These bound before the fix, in both
		// the same directory and a parent one.
		{"sameDir/noExt", "src/alpha.ts", "src/main.ts", "./alpha"},
		{"parentDir/noExt", "src/alpha.ts", "src/sub/beta.ts", "../alpha"},

		// The regression: an emitted-JS extension, at every traversal
		// depth the issue reported.
		{"sameDir/jsExt", "src/alpha.ts", "src/main.ts", "./alpha.js"},
		{"parentDir/jsExt", "src/alpha.ts", "src/sub/beta.ts", "../alpha.js"},
		{"grandparentDir/jsExt", "src/snapshot/readers/alpha.ts",
			"src/historical/handlers/beta.ts", "../../snapshot/readers/alpha.js"},

		// Every emitted extension maps to the source that compiles to it.
		{"parentDir/jsExt→tsx", "src/alpha.tsx", "src/sub/beta.ts", "../alpha.js"},
		{"parentDir/jsxExt→tsx", "src/alpha.tsx", "src/sub/beta.ts", "../alpha.jsx"},
		{"parentDir/mjsExt→mts", "src/alpha.mts", "src/sub/beta.ts", "../alpha.mjs"},
		{"parentDir/cjsExt→cts", "src/alpha.cts", "src/sub/beta.ts", "../alpha.cjs"},

		// A directory barrel named by its emitted entry point.
		{"parentDir/barrelIndexJs", "src/lib/index.ts", "src/sub/beta.ts", "../lib/index.js"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeFileMk(t, filepath.Join(dir, "package.json"), "{\"type\":\"module\"}\n")
			writeFileMk(t, filepath.Join(dir, "tsconfig.json"),
				"{\"compilerOptions\":{\"module\":\"NodeNext\",\"moduleResolution\":\"NodeNext\"}}\n")
			writeFileMk(t, filepath.Join(dir, filepath.FromSlash(tc.calleeFile)),
				"export function readAlpha(x: number): number { return x * 2 }\n")
			writeFileMk(t, filepath.Join(dir, filepath.FromSlash(tc.callerFile)),
				"import { readAlpha } from '"+tc.spec+"'\n"+
					"export function handleBeta(): number { return readAlpha(21) }\n")

			idx, _ := newSQLiteIndexer(t)
			_, err := idx.Index(dir)
			require.NoError(t, err)
			idx.ResolveAll()

			g := idx.Graph()
			want := tc.calleeFile + "::readAlpha"

			require.Equal(t, want, callTargetForName(t, g, tc.callerFile, "handleBeta"),
				"call through %q must bind to the TypeScript source that emits it", tc.spec)
			require.Positive(t, importEdgeCount(t, g, want),
				"per-binding import edge through %q must land on the symbol so "+
					"find_usages / get_callers report the importer", tc.spec)
		})
	}
}

// TestNodeNextEmittedJSSpecifier_PrefersRealFile pins the one deliberate
// deviation from tsc's resolution order. tsc resolves `./alpha.js` to a
// co-located `alpha.ts` even when `alpha.js` itself exists; the graph
// probes the verbatim path first, because binding to a JavaScript file
// that is really indexed beats binding to a sibling that may only carry
// types. The `.d.ts` fallbacks are ordered last for the same reason.
func TestNodeNextEmittedJSSpecifier_PrefersRealFile(t *testing.T) {
	dir := t.TempDir()
	writeFileMk(t, filepath.Join(dir, "src", "alpha.js"),
		"export function readAlpha(x) { return x * 2 }\n")
	writeFileMk(t, filepath.Join(dir, "src", "alpha.d.ts"),
		"export declare function readAlpha(x: number): number\n")
	writeFileMk(t, filepath.Join(dir, "src", "sub", "beta.ts"),
		"import { readAlpha } from '../alpha.js'\n"+
			"export function handleBeta(): number { return readAlpha(21) }\n")

	idx, _ := newSQLiteIndexer(t)
	_, err := idx.Index(dir)
	require.NoError(t, err)
	idx.ResolveAll()

	require.Equal(t, "src/alpha.js::readAlpha",
		callTargetForName(t, idx.Graph(), "src/sub/beta.ts", "handleBeta"),
		"an indexed .js implementation must outrank its .d.ts declaration")
}
