package indexer

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser/tsalias"
)

func TestTSFileCandidates(t *testing.T) {
	got := tsFileCandidates("src/components/Widget.ts")
	for _, want := range []string{
		"src/components/Widget.ts",
		"src/components/Widget.tsx",
		"src/components/Widget/index.ts",
		"src/components/Widget/index.tsx",
	} {
		require.Contains(t, got, want)
	}
	require.Nil(t, tsFileCandidates(""))
}

// TestTSFileCandidates_SingleFileComponents confirms `.svelte` /
// `.vue` module specifiers expand to a direct file candidate (the
// stem with the SFC extension), and never to a directory `index`
// barrel — single-file components have no `index.svelte` convention.
func TestTSFileCandidates_SingleFileComponents(t *testing.T) {
	got := tsFileCandidates("src/lib/Button.svelte")
	require.Contains(t, got, "src/lib/Button.svelte")
	require.NotContains(t, got, "src/lib/Button/index.svelte")

	gotVue := tsFileCandidates("src/lib/Card.vue")
	require.Contains(t, gotVue, "src/lib/Card.vue")
	require.NotContains(t, gotVue, "src/lib/Card/index.vue")
}

// TestParseTSReExports_DefaultAsSvelte covers the Svelte component
// barrel form `export { default as Button } from './Button.svelte'`:
// the module specifier resolves to the `.svelte` file (no `.ts`
// appended), and the local exported name `Button` maps to the
// module's `default` export.
func TestParseTSReExports_DefaultAsSvelte(t *testing.T) {
	src := `
export { default as Button } from './Button.svelte';
export { default as Card } from './Card.vue';
`
	res := parseTSReExports(src, "src/lib/index.ts", nil, "")
	require.Len(t, res, 2)

	byFile := map[string]tsReExport{}
	for _, re := range res {
		byFile[re.fromFile] = re
	}
	// The `.svelte` / `.vue` specifier keeps its extension verbatim.
	require.Equal(t, "default", byFile["src/lib/Button.svelte"].names["Button"])
	require.Equal(t, "default", byFile["src/lib/Card.vue"].names["Card"])
}

// TestFollowReExportChain_DefaultAsSvelte drives the full follower over
// a Svelte barrel: a consumer imports `Button`, the barrel re-exports
// it as the default export of a `.svelte` component, and the chain must
// reach that component file.
func TestFollowReExportChain_DefaultAsSvelte(t *testing.T) {
	mi := &MultiIndexer{}
	srcCache := map[string][]byte{
		"src/lib/index.ts":      []byte(`export { default as Button } from './Button.svelte';`),
		"src/lib/Button.svelte": []byte(`<script lang="ts">export let label = "";</script>`),
	}
	reachable := mi.followReExportChainDetailed("src/lib/index.ts", "Button", srcCache).files
	require.True(t, reachable["src/lib/Button.svelte"],
		"the `default as` re-export must reach the .svelte component file")
}

func TestParseTSReExports_StarNamedAndType(t *testing.T) {
	src := `
export * from './widgets';
export { Button, Icon as Glyph } from './ui';
export type { Theme } from './theme';
export * as utils from './util';
export const local = 1;
`
	res := parseTSReExports(src, "src/index.ts", nil, "")
	// star + named + type re-exports — the `* as utils` namespace
	// re-export and the local const are not forwarding statements.
	require.Len(t, res, 3)

	byFile := map[string]tsReExport{}
	for _, re := range res {
		byFile[re.fromFile] = re
	}
	require.True(t, byFile["src/widgets.ts"].star)
	require.Equal(t, "Button", byFile["src/ui.ts"].names["Button"])
	require.Equal(t, "Icon", byFile["src/ui.ts"].names["Glyph"]) // `as` rebind
	require.Equal(t, "Theme", byFile["src/theme.ts"].names["Theme"])
}

func TestFollowReExportChain_BarrelToTerminal(t *testing.T) {
	mi := &MultiIndexer{}
	srcCache := map[string][]byte{
		"src/index.ts":             []byte(`export { Widget } from './components';`),
		"src/components/index.ts":  []byte(`export * from './Widget';`),
		"src/components/Widget.ts": []byte(`export interface Widget { id: string }`),
	}
	reachable := mi.followReExportChainDetailed("src/index.ts", "Widget", srcCache).files
	require.True(t, reachable["src/components/Widget.ts"],
		"re-export chain must reach the terminal module")
}

func TestFollowReExportChain_RenamedExport(t *testing.T) {
	mi := &MultiIndexer{}
	srcCache := map[string][]byte{
		"pkg/index.ts": []byte(`export { Internal as Public } from './impl';`),
		"pkg/impl.ts":  []byte(`export class Internal {}`),
	}
	reachable := mi.followReExportChainDetailed("pkg/index.ts", "Public", srcCache).files
	require.True(t, reachable["pkg/impl.ts"], "must follow the `as` rename to the source module")
}

func TestFollowReExportChain_CircularTerminates(t *testing.T) {
	mi := &MultiIndexer{}
	srcCache := map[string][]byte{
		"a.ts": []byte(`export * from './b';`),
		"b.ts": []byte(`export * from './a';`),
	}
	reachable := mi.followReExportChainDetailed("a.ts", "X", srcCache).files
	require.True(t, reachable["a.ts"])
	require.True(t, reachable["b.ts"])
}

// TestResolveTSModulePath_AliasResolvesWithPrefix wires an alias map
// (matching what tsalias.Load would produce for a Next.js-style
// `@/*` config) into resolveTSModulePath and verifies the result is
// re-prefixed with the repo prefix so the caller can match it
// against the graph node's FilePath.
func TestResolveTSModulePath_AliasResolvesWithPrefix(t *testing.T) {
	m := &tsalias.Map{
		Entries: []tsalias.Alias{
			{AliasPrefix: "@/", TargetPrefix: "src/", HasWildcard: true},
		},
	}
	got := resolveTSModulePath("@/lib/api", "myrepo/src/components", m, "myrepo")
	want := "myrepo/src/lib/api.ts"
	if got != want {
		t.Errorf("resolveTSModulePath alias = %q, want %q", got, want)
	}
}

// TestResolveTSModulePath_AliasNoMatchFallsThrough ensures that a bare
// specifier with no matching alias entry still returns "" so the
// caller leaves the bare-name reference in place.
func TestResolveTSModulePath_AliasNoMatchFallsThrough(t *testing.T) {
	m := &tsalias.Map{
		Entries: []tsalias.Alias{
			{AliasPrefix: "@/", TargetPrefix: "src/", HasWildcard: true},
		},
	}
	if got := resolveTSModulePath("react", "myrepo/src", m, "myrepo"); got != "" {
		t.Errorf("unmatched bare specifier should resolve to \"\", got %q", got)
	}
}

// TestParseTSImports_AliasResolvesNamedImport drives the full path
// from import-statement scanning through alias resolution.
func TestParseTSImports_AliasResolvesNamedImport(t *testing.T) {
	src := `
import { Foo } from '@/lib/schema'
`
	m := &tsalias.Map{
		Entries: []tsalias.Alias{
			{AliasPrefix: "@/", TargetPrefix: "src/", HasWildcard: true},
		},
	}
	got := parseTSImports(src, "web/src/app/page.tsx", m, "web")
	want := map[string]string{"Foo": "web/src/lib/schema.ts"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v\nwant %+v", got, want)
	}
}

// TestParseTSImports_ResolvesNamedImports covers the common shape:
// `import type { Foo, Bar as Baz } from './schema'`. Each named
// binding maps to the resolved file path; the alias rebind uses
// the local-binding name (`Baz`), not the source name (`Bar`).
func TestParseTSImports_ResolvesNamedImports(t *testing.T) {
	src := `
import type { Foo, Bar as Baz } from './schema'
import { Quux } from '../shared/types'
`
	got := parseTSImports(src, "web/src/lib/api.ts", nil, "")
	want := map[string]string{
		"Foo":  "web/src/lib/schema.ts",
		"Baz":  "web/src/lib/schema.ts",
		"Quux": "web/src/shared/types.ts",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v\nwant %+v", got, want)
	}
}

// TestParseTSImports_SkipsBareSpecifiers ensures third-party and
// path-aliased imports (`react`, `@/lib/foo`) don't leak into the
// resolution map — they don't correspond to a graph file in the
// local repo and would mislead the disambiguator if they did.
func TestParseTSImports_SkipsBareSpecifiers(t *testing.T) {
	src := `
import React from 'react'
import { useState } from 'react'
import { foo } from '@/lib/foo'
`
	got := parseTSImports(src, "web/src/lib/api.ts", nil, "")
	if len(got) != 0 {
		t.Errorf("expected empty map for bare specifiers, got %+v", got)
	}
}

// TestParseTSImports_DefaultAndNamespace checks `import Foo from`
// (default) and `import * as NS from` bindings.
func TestParseTSImports_DefaultAndNamespace(t *testing.T) {
	src := `
import Default from './a'
import * as NS from './b'
`
	got := parseTSImports(src, "x/y.ts", nil, "")
	if got["Default"] != "x/a.ts" {
		t.Errorf("default import: got %q, want %q", got["Default"], "x/a.ts")
	}
	if got["NS"] != "x/b.ts" {
		t.Errorf("namespace import: got %q, want %q", got["NS"], "x/b.ts")
	}
}

// TestSplitTSImportClause_AliasAndType normalises the `type` keyword
// and `as <local>` rebind in named-import bodies.
func TestSplitTSImportClause_AliasAndType(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"Foo, Bar", []string{"Foo", "Bar"}},
		{"type Foo, Bar as Baz", []string{"Foo", "Baz"}},
		{"  Foo  ,  type Bar  ", []string{"Foo", "Bar"}},
		{"", nil},
	}
	for _, c := range cases {
		got := splitTSImportClause(c.in)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("splitTSImportClause(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// TestResolveTSModulePath_AddsTSExtension confirms that a relative
// specifier without an extension picks up `.ts` so a candidate
// living in `<dir>/file.ts` is matchable. Specifiers with an
// explicit extension are left alone.
func TestResolveTSModulePath_AddsTSExtension(t *testing.T) {
	cases := []struct {
		mod, dir, want string
	}{
		{"./schema", "web/src/lib", "web/src/lib/schema.ts"},
		{"./schema.ts", "web/src/lib", "web/src/lib/schema.ts"},
		{"./schema.tsx", "web/src/lib", "web/src/lib/schema.tsx"},
		{"../api", "web/src/lib/inner", "web/src/lib/api.ts"},
		{"react", "web/src/lib", ""},
		{"@/lib/foo", "web/src/lib", ""},
	}
	for _, c := range cases {
		got := resolveTSModulePath(c.mod, c.dir, nil, "")
		if got != c.want {
			t.Errorf("resolveTSModulePath(%q, %q) = %q, want %q", c.mod, c.dir, got, c.want)
		}
	}
}

// TestIsImportResolvableLang only flags TS / JS family extensions —
// Go / Python / etc. are skipped because their import semantics
// differ enough that this resolver would mis-attribute candidates.
func TestIsImportResolvableLang(t *testing.T) {
	pos := []string{"a.ts", "b.tsx", "c.js", "d.jsx", "e.mts", "f.cjs", "g.rs"}
	neg := []string{"a.go", "b.py", "c.rb", "d.java", "e", "f.kt"}
	for _, p := range pos {
		if !isImportResolvableLang(p) {
			t.Errorf("%q: want true, got false", p)
		}
	}
	for _, p := range neg {
		if isImportResolvableLang(p) {
			t.Errorf("%q: want false, got true", p)
		}
	}
}

// TestFollowReExportChain_WildcardMultiHop drives a wildcard
// (`export *`) re-export chain two hops deep: the entry barrel stars
// a mid barrel, which stars the terminal module that actually defines
// the symbol. The follower must reach the terminal file.
func TestFollowReExportChain_WildcardMultiHop(t *testing.T) {
	mi := &MultiIndexer{}
	srcCache := map[string][]byte{
		"pkg/index.ts":     []byte(`export * from './mid';`),
		"pkg/mid.ts":       []byte(`export * from './terminal';`),
		"pkg/terminal.ts":  []byte(`export class Engine {}`),
		"pkg/unrelated.ts": []byte(`export class Engine {}`),
	}
	reachable := mi.followReExportChainDetailed("pkg/index.ts", "Engine", srcCache).files
	require.True(t, reachable["pkg/terminal.ts"],
		"multi-hop `export *` chain must reach the terminal module")
	require.False(t, reachable["pkg/unrelated.ts"],
		"a same-named symbol in an unrelated file must not be reached")
}

// TestFollowReExportChain_WildcardThroughNamed mixes the two TS forms:
// a named re-export of a symbol whose source module then `export *`s
// the terminal definition module.
func TestFollowReExportChain_WildcardThroughNamed(t *testing.T) {
	mi := &MultiIndexer{}
	srcCache := map[string][]byte{
		"src/index.ts":  []byte(`export { Theme } from './theme';`),
		"src/theme.ts":  []byte(`export * from './tokens';`),
		"src/tokens.ts": []byte(`export interface Theme { name: string }`),
	}
	reachable := mi.followReExportChainDetailed("src/index.ts", "Theme", srcCache).files
	require.True(t, reachable["src/tokens.ts"],
		"named → wildcard re-export chain must reach the definition module")
}

// TestParseRustReExports covers every `pub use` shape the re-export
// parser recognises, and asserts a plain (private) `use` is never
// treated as a re-export.
func TestParseRustReExports(t *testing.T) {
	src := `
pub use crate::engine::Renderer;
pub use crate::shapes::{Circle, Square as Box};
pub use crate::prelude::*;
pub(crate) use crate::util::Helper;
use crate::internal::Secret;
`
	res := parseRustReExports(src, "src/lib.rs")
	byFile := map[string]reExportEdge{}
	for _, re := range res {
		byFile[re.fromFile] = re
	}
	// Single, list, glob, and pub(crate) forms forward; the private
	// `use crate::internal::Secret` does not.
	require.Equal(t, "Renderer", byFile["src/engine.rs"].names["Renderer"])
	require.Equal(t, "Circle", byFile["src/shapes.rs"].names["Circle"])
	require.Equal(t, "Square", byFile["src/shapes.rs"].names["Box"]) // `as` rebind
	require.True(t, byFile["src/prelude.rs"].star)
	require.Equal(t, "Helper", byFile["src/util.rs"].names["Helper"])
	_, hasSecret := byFile["src/internal.rs"]
	require.False(t, hasSecret, "a private `use` must not be parsed as a re-export")
}

// TestFollowReExportChain_RustPubUseMultiHop drives a `pub use`
// re-export chain two hops deep through three Rust modules: the crate
// root re-exports a name from the `api` module, which re-exports it
// from the `domain` module that actually defines it.
func TestFollowReExportChain_RustPubUseMultiHop(t *testing.T) {
	mi := &MultiIndexer{}
	srcCache := map[string][]byte{
		"crate/src/lib.rs":       []byte(`pub use crate::api::User;`),
		"crate/src/api.rs":       []byte(`pub use crate::domain::User;`),
		"crate/src/domain.rs":    []byte(`pub struct User { id: u64 }`),
		"crate/src/unrelated.rs": []byte(`pub struct User;`),
	}
	reachable := mi.followReExportChainDetailed("crate/src/lib.rs", "User", srcCache).files
	require.True(t, reachable["crate/src/domain.rs"],
		"multi-hop `pub use` chain must reach the defining module")
	require.False(t, reachable["crate/src/unrelated.rs"],
		"a same-named type in an unrelated module must not be reached")
}

// TestFollowReExportChain_RustGlobAndModDir exercises a `pub use
// mod::*` glob re-export and the `mod.rs` directory-module layout.
func TestFollowReExportChain_RustGlobAndModDir(t *testing.T) {
	mi := &MultiIndexer{}
	srcCache := map[string][]byte{
		"src/lib.rs":           []byte(`pub use crate::shapes::*;`),
		"src/shapes/mod.rs":    []byte(`pub use self::circle::Circle;`),
		"src/shapes/circle.rs": []byte(`pub struct Circle { r: f64 }`),
	}
	reachable := mi.followReExportChainDetailed("src/lib.rs", "Circle", srcCache).files
	require.True(t, reachable["src/shapes/circle.rs"],
		"glob re-export into a mod.rs directory module must reach the leaf")
}

// TestFollowReExportChain_RustPrivateUseNotForwarded is the negative
// case: a module that imports a symbol with a plain (non-`pub`) `use`
// does not re-export it, so the chain must not reach the symbol's
// real definition through that module.
func TestFollowReExportChain_RustPrivateUseNotForwarded(t *testing.T) {
	mi := &MultiIndexer{}
	srcCache := map[string][]byte{
		// `api` only privately `use`s Token — it is not re-exported.
		"src/lib.rs":    []byte(`pub use crate::api::Token;`),
		"src/api.rs":    []byte(`use crate::secret::Token;`),
		"src/secret.rs": []byte(`pub struct Token;`),
	}
	reachable := mi.followReExportChainDetailed("src/lib.rs", "Token", srcCache).files
	require.False(t, reachable["src/secret.rs"],
		"a privately-`use`d symbol must not resolve through the re-export chain")
}

// TestResolveBareTypeViaImports_ThroughReExports is the end-to-end
// table: a consumer file imports a type name that collides with a
// same-named decoy elsewhere in the graph, and only the genuine
// definition is reachable through the wildcard / `pub use` re-export
// chain. resolveBareTypeViaImports must return the ID of that genuine
// definition node — or "" when the chain does not forward the name.
func TestResolveBareTypeViaImports_ThroughReExports(t *testing.T) {
	cases := []struct {
		name     string
		consumer string            // consumer file path
		typeName string            // bare type name being resolved
		files    map[string][]byte // re-export fixture sources
		nodes    map[string]string // candidate node ID -> FilePath
		wantID   string            // expected resolved node ID ("" = unresolved)
	}{
		{
			name:     "ts wildcard multi-hop resolves to original definition",
			consumer: "web/src/app.ts",
			typeName: "Widget",
			files: map[string][]byte{
				"web/src/app.ts":       []byte(`import { Widget } from './ui';`),
				"web/src/ui.ts":        []byte(`export * from './ui/index';`),
				"web/src/ui/index.ts":  []byte(`export * from './widget';`),
				"web/src/ui/widget.ts": []byte(`export interface Widget { id: string }`),
			},
			nodes: map[string]string{
				"web/src/ui/widget.ts::Widget":  "web/src/ui/widget.ts",
				"web/src/legacy/old.ts::Widget": "web/src/legacy/old.ts",
			},
			wantID: "web/src/ui/widget.ts::Widget",
		},
		{
			name:     "js wildcard re-export resolves to original definition",
			consumer: "app/src/main.js",
			typeName: "Parser",
			files: map[string][]byte{
				"app/src/main.js": []byte(`import { Parser } from './lib';`),
				"app/src/lib.js":  []byte(`export * from './core';`),
				"app/src/core.js": []byte(`export class Parser {}`),
			},
			nodes: map[string]string{
				"app/src/core.js::Parser":      "app/src/core.js",
				"app/src/v1/legacy.js::Parser": "app/src/v1/legacy.js",
			},
			wantID: "app/src/core.js::Parser",
		},
		{
			name:     "rust pub use multi-hop resolves to original definition",
			consumer: "crate/src/main.rs",
			typeName: "Account",
			files: map[string][]byte{
				"crate/src/main.rs":  []byte(`use crate::api::Account;`),
				"crate/src/api.rs":   []byte(`pub use crate::model::Account;`),
				"crate/src/model.rs": []byte(`pub use crate::store::Account;`),
				"crate/src/store.rs": []byte(`pub struct Account { id: u64 }`),
			},
			nodes: map[string]string{
				"crate/src/store.rs::Account": "crate/src/store.rs",
				"crate/src/other.rs::Account": "crate/src/other.rs",
			},
			wantID: "crate/src/store.rs::Account",
		},
		{
			name:     "rust private use does not resolve through",
			consumer: "crate/src/main.rs",
			typeName: "Hidden",
			files: map[string][]byte{
				"crate/src/main.rs":     []byte(`use crate::api::Hidden;`),
				"crate/src/api.rs":      []byte(`use crate::internal::Hidden;`),
				"crate/src/internal.rs": []byte(`pub struct Hidden;`),
			},
			nodes: map[string]string{
				"crate/src/internal.rs::Hidden": "crate/src/internal.rs",
				"crate/src/decoy.rs::Hidden":    "crate/src/decoy.rs",
			},
			wantID: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := graph.New()
			for id, fp := range tc.nodes {
				g.AddNode(&graph.Node{
					ID: id, Kind: graph.KindType, Name: tc.typeName,
					FilePath: fp, StartLine: 1,
				})
			}
			mi := &MultiIndexer{}
			srcCache := map[string][]byte{}
			for k, v := range tc.files {
				srcCache[k] = v
			}
			importCache := map[string]map[string]string{}
			got := mi.resolveBareTypeViaImports(tc.consumer, tc.typeName, g, srcCache, importCache)
			require.Equal(t, tc.wantID, got)
		})
	}
}
