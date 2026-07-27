package mcp

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/zzet/gortex/internal/graph"
)

// TestReExportBarrel_EmittedJSSpecifier covers the barrel half of issue
// #304: under `moduleResolution: node16` / `nodenext` a re-export names
// the file the compiler EMITS (`export { persist } from './impl.js'`),
// which is mandatory for ESM TypeScript. Probing only `<stem>.<ext>` turns
// that into `./impl.js.ts` and never matches, so the barrel binding fails
// to canonicalise and find_usages on it reports nothing.
func TestReExportBarrel_EmittedJSSpecifier(t *testing.T) {
	newGraph := func(declFile, spec string) graph.Store {
		g := graph.New()
		addFile := func(id string) {
			g.AddNode(&graph.Node{ID: id, Kind: graph.KindFile, Name: id,
				FilePath: id, Language: "typescript"})
		}
		addFile(declFile)
		g.AddNode(&graph.Node{ID: declFile + "::persist", Kind: graph.KindFunction,
			Name: "persist", FilePath: declFile, Language: "typescript"})
		addFile("src/index.ts")
		g.AddEdge(&graph.Edge{From: "src/index.ts",
			To: "unresolved::import::" + spec + "::persist", Kind: graph.EdgeReExports,
			FilePath: "src/index.ts", Line: 1})
		return g
	}

	cases := []struct {
		name     string
		declFile string
		spec     string
	}{
		{"noExt_control", "src/impl.ts", "./impl"},
		{"jsExt", "src/impl.ts", "./impl.js"},
		{"jsExt_tsxTarget", "src/impl.tsx", "./impl.js"},
		{"mjsExt", "src/impl.mts", "./impl.mjs"},
		{"cjsExt", "src/impl.cts", "./impl.cjs"},
		{"parentDir_jsExt", "impl.ts", "../impl.js"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := newGraph(tc.declFile, tc.spec)
			assert.Equal(t, tc.declFile+"::persist",
				reExportBindingCanonical(g, "src/index.ts::persist", 0),
				"barrel re-export through %q must canonicalise to the TypeScript source", tc.spec)
		})
	}
}

// TestReExportBarrel_PrefersRealFile pins the same deliberate ordering the
// resolver uses: a `.js` file that really exists in the graph outranks the
// TypeScript counterparts probed for an emitted-JS specifier.
func TestReExportBarrel_PrefersRealFile(t *testing.T) {
	g := graph.New()
	addFile := func(id string) {
		g.AddNode(&graph.Node{ID: id, Kind: graph.KindFile, Name: id,
			FilePath: id, Language: "javascript"})
	}
	for _, f := range []string{"src/impl.js", "src/impl.d.ts"} {
		addFile(f)
		g.AddNode(&graph.Node{ID: f + "::persist", Kind: graph.KindFunction,
			Name: "persist", FilePath: f, Language: "javascript"})
	}
	addFile("src/index.ts")
	g.AddEdge(&graph.Edge{From: "src/index.ts",
		To: "unresolved::import::./impl.js::persist", Kind: graph.EdgeReExports,
		FilePath: "src/index.ts", Line: 1})

	assert.Equal(t, "src/impl.js::persist",
		reExportBindingCanonical(g, "src/index.ts::persist", 0),
		"an indexed .js implementation must outrank its .d.ts declaration")
}
