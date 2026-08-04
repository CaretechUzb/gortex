package resolver

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/zzet/gortex/internal/graph"
)

// seedImportEdge drops in a pending `unresolved::import::<spec>` edge —
// the shape resolveImport consumes.
func seedImportEdge(g graph.Store, fileID, spec string) *graph.Edge {
	e := &graph.Edge{
		From:     fileID,
		To:       "unresolved::import::" + spec,
		Kind:     graph.EdgeImports,
		FilePath: fileID,
		Line:     1,
	}
	g.AddEdge(e)
	return e
}

// TestResolveImport_BareNpmSpecifierDoesNotBindSameNamedDir pins issue #450:
// a bare npm specifier must never bind to a same-repo directory that merely
// shares its name. The `lastDirIndex` cascade keys on the last path component
// of every indexed file's directory, so `import … from 'graphql'` used to also
// land on whatever file sits under some unrelated `*/graphql/` directory.
func TestResolveImport_BareNpmSpecifierDoesNotBindSameNamedDir(t *testing.T) {
	g := graph.New()
	seedFile(g, "src/consumer-graphql.ts", "typescript")
	seedFile(g, "src/feature/graphql/create.mutation.gql", "graphql")
	e := seedImportEdge(g, "src/consumer-graphql.ts", "graphql")

	New(g).ResolveAll()

	assert.Equal(t, "external::graphql", e.To,
		"a bare npm specifier must stay external, not bind to src/feature/graphql/")
}

// TestResolveImport_ScopedBareSpecifierDoesNotBindSameNamedDir covers the
// scoped-package shape: `@acme/utils` last-components to `utils`, which
// matches every `*/utils/` directory in the repo.
func TestResolveImport_ScopedBareSpecifierDoesNotBindSameNamedDir(t *testing.T) {
	g := graph.New()
	seedFile(g, "src/app.tsx", "typescript")
	seedFile(g, "src/shared/utils/format.ts", "typescript")
	e := seedImportEdge(g, "src/app.tsx", "@acme/utils")

	New(g).ResolveAll()

	assert.Equal(t, "external::@acme/utils", e.To,
		"a scoped bare specifier must stay external, not bind to src/shared/utils/")
}

// TestResolveImport_BareSpecifierSubpathDoesNotBindSameNamedDir covers a
// deep specifier (`lodash/merge`) whose last component collides with a repo
// directory.
func TestResolveImport_BareSpecifierSubpathDoesNotBindSameNamedDir(t *testing.T) {
	g := graph.New()
	seedFile(g, "src/app.js", "javascript")
	seedFile(g, "src/state/merge/reducer.js", "javascript")
	e := seedImportEdge(g, "src/app.js", "lodash/merge")

	New(g).ResolveAll()

	assert.Equal(t, "external::lodash/merge", e.To,
		"a bare subpath specifier must stay external, not bind to src/state/merge/")
}

// TestResolveImport_BareSpecifierWithNoCollisionStaysExternal is the control
// from the issue: with no same-named directory the specifier was already
// external, and must stay that way.
func TestResolveImport_BareSpecifierWithNoCollisionStaysExternal(t *testing.T) {
	g := graph.New()
	seedFile(g, "src/consumer-lodash.ts", "typescript")
	e := seedImportEdge(g, "src/consumer-lodash.ts", "lodash")

	New(g).ResolveAll()

	assert.Equal(t, "external::lodash", e.To)
}

// TestResolveImport_JSTSRelativeSpecifierStillResolves guards the fix's
// blast radius: relative specifiers are not bare and keep resolving onto the
// in-repo file they name (issue #136 / #304 behaviour).
func TestResolveImport_JSTSRelativeSpecifierStillResolves(t *testing.T) {
	g := graph.New()
	seedFile(g, "src/app.ts", "typescript")
	seedFile(g, "src/lib/auth.ts", "typescript")
	e := seedImportEdge(g, "src/app.ts", "./lib/auth")

	New(g).ResolveAll()

	assert.Equal(t, "src/lib/auth.ts", e.To, "relative JS/TS imports must keep resolving")
}

// TestResolveImport_JSTSPathAliasStillResolves guards that a tsconfig path
// alias — expanded before the directory cascade — is untouched by the fix.
func TestResolveImport_JSTSPathAliasStillResolves(t *testing.T) {
	g := graph.New()
	seedFile(g, "src/app.ts", "typescript")
	seedFile(g, "src/lib/auth.ts", "typescript")
	e := seedImportEdge(g, "src/app.ts", "@/lib/auth")

	r := New(g)
	r.SetPathAliasResolver(func(callerFile, specifier string) string {
		if len(specifier) > 2 && specifier[:2] == "@/" {
			return "src/" + specifier[2:]
		}
		return ""
	})
	r.ResolveAll()

	assert.Equal(t, "src/lib/auth.ts", e.To, "tsconfig path aliases must keep resolving")
}

// TestResolveImport_NonJSTSCallerKeepsDirectoryCascade pins that the fix is
// scoped to JS/TS callers: a Go/Python-style bare package import of an
// in-repo directory still resolves through the cascade, which is the
// behaviour buildDirIndexes documents ("an import of `logger` matches any
// file under .../logger/").
func TestResolveImport_NonJSTSCallerKeepsDirectoryCascade(t *testing.T) {
	g := graph.New()
	seedFile(g, "app/main.py", "python")
	seedFile(g, "app/logger/handler.py", "python")
	e := seedImportEdge(g, "app/main.py", "logger")

	New(g).ResolveAll()

	assert.Equal(t, "app/logger/handler.py", e.To,
		"non-JS/TS bare imports keep the package-directory cascade")
}

// TestResolveImport_BareSpecifierBindsDirectoryEntryPoint pins the surviving
// half of the rule: a bare specifier naming an in-repo package directory
// still resolves — but only through that directory's entry point, the way
// Node and tsc load a directory-shaped module.
func TestResolveImport_BareSpecifierBindsDirectoryEntryPoint(t *testing.T) {
	g := graph.New()
	seedFile(g, "src/app.ts", "typescript")
	seedFile(g, "packages/logger/index.ts", "typescript")
	e := seedImportEdge(g, "src/app.ts", "logger")

	New(g).ResolveAll()

	assert.Equal(t, "packages/logger/index.ts", e.To,
		"a bare specifier still binds a package directory via its entry point")
}

// TestResolveImport_BareSpecifierSkipsNonEntryPointSiblings pins that the
// entry point wins over an arbitrary sibling regardless of iteration order —
// the sibling is no longer an eligible candidate at all.
func TestResolveImport_BareSpecifierSkipsNonEntryPointSiblings(t *testing.T) {
	g := graph.New()
	seedFile(g, "src/app.ts", "typescript")
	seedFile(g, "packages/logger/aaa-first.ts", "typescript")
	seedFile(g, "packages/logger/index.ts", "typescript")
	e := seedImportEdge(g, "src/app.ts", "logger")

	New(g).ResolveAll()

	assert.Equal(t, "packages/logger/index.ts", e.To,
		"only the directory entry point is an eligible candidate")
}

// TestResolveImport_DeclaredExternalDepRefusesEntryPoint closes the residual
// case the entry-point rule alone cannot reach: a repo that both depends on
// npm `graphql` AND contains `src/feature/graphql/index.ts`. The manifest
// says the package comes from the registry, so no in-repo directory is a
// candidate.
func TestResolveImport_DeclaredExternalDepRefusesEntryPoint(t *testing.T) {
	g := graph.New()
	seedFile(g, "src/app.ts", "typescript")
	seedFile(g, "src/feature/graphql/index.ts", "typescript")
	e := seedImportEdge(g, "src/app.ts", "graphql")

	r := New(g)
	r.SetNpmDependencyLookup(func(callerFile, specifier string) bool {
		return specifier == "graphql" // declared "^16.8.0"
	})
	r.ResolveAll()

	assert.Equal(t, "external::graphql", e.To,
		"a declared registry dependency must not bind an in-repo entry point")
}

// TestResolveImport_WorkspaceProtocolDepStillBinds is the other half: a
// dependency declared as `workspace:*` genuinely names an in-repo package,
// so the lookup reports false and the entry-point binding survives. This is
// what keeps pnpm monorepos working — their members are not covered by the
// root manifest's `workspaces` globs.
func TestResolveImport_WorkspaceProtocolDepStillBinds(t *testing.T) {
	g := graph.New()
	seedFile(g, "src/app.ts", "typescript")
	seedFile(g, "packages/logger/index.ts", "typescript")
	e := seedImportEdge(g, "src/app.ts", "logger")

	r := New(g)
	r.SetNpmDependencyLookup(func(callerFile, specifier string) bool {
		return false // declared "workspace:*" → resolves in-repo
	})
	r.ResolveAll()

	assert.Equal(t, "packages/logger/index.ts", e.To,
		"a workspace-protocol dependency must still bind its in-repo entry point")
}

// TestResolveImport_NilDependencyLookupKeepsEntryPointRule pins that without
// an injected lookup (no indexer wired) behaviour falls back to the
// entry-point rule alone.
func TestResolveImport_NilDependencyLookupKeepsEntryPointRule(t *testing.T) {
	g := graph.New()
	seedFile(g, "src/app.ts", "typescript")
	seedFile(g, "packages/logger/index.ts", "typescript")
	e := seedImportEdge(g, "src/app.ts", "logger")

	New(g).ResolveAll()

	assert.Equal(t, "packages/logger/index.ts", e.To)
}

func TestIsJSTSDirEntryPoint(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"packages/logger/index.ts", true},
		{"packages/logger/index.tsx", true},
		{"packages/logger/index.js", true},
		{"packages/logger/index.mjs", true},
		{"packages/logger/index.cjs", true},
		{"packages/logger/index.d.ts", true},
		{"index.ts", true},
		{"packages/logger/Index.TS", true}, // case-insensitive
		{"packages/logger/handler.ts", false},
		{"packages/logger/index.go", false},
		{"packages/logger/index.gql", false},
		{"packages/logger/reindex.ts", false},
		{"packages/logger/index", false},
		{"", false},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, isJSTSDirEntryPoint(c.path), "path=%q", c.path)
	}
}

func TestIsJSTSBareSpecifier(t *testing.T) {
	cases := []struct {
		caller string
		spec   string
		want   bool
	}{
		{"src/app.ts", "graphql", true},
		{"src/app.tsx", "@acme/utils", true},
		{"src/app.js", "lodash/merge", true},
		{"src/app.mjs", "node:fs", true},
		{"src/app.ts", "graphql::parse", true}, // per-binding edge payload
		{"src/app.ts", "./lib/auth", false},
		{"src/app.ts", "../lib/auth", false},
		{"src/app.ts", ".", false},
		{"src/app.ts", "..", false},
		{"src/app.ts", "/abs/path", false},
		{"src/app.ts", "", false},
		{"app/main.py", "graphql", false}, // non-JS/TS caller
		{"main.go", "logger", false},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, isJSTSBareSpecifier(c.caller, c.spec),
			"caller=%q spec=%q", c.caller, c.spec)
	}
}
