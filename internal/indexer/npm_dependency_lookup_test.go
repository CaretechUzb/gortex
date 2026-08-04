package indexer

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeManifest(t *testing.T, root, dir, body string) {
	t.Helper()
	abs := filepath.Join(root, filepath.FromSlash(dir))
	require.NoError(t, os.MkdirAll(abs, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(abs, "package.json"), []byte(body), 0o644))
}

// TestDeclaresExternalDependency_RegistryVsWorkspace pins the discriminator
// that keeps pnpm monorepos working: a registry range is external, a
// `workspace:*` member is not.
func TestDeclaresExternalDependency_RegistryVsWorkspace(t *testing.T) {
	root := t.TempDir()
	writeManifest(t, root, ".", `{
		"dependencies": {
			"graphql": "^16.8.0",
			"logger": "workspace:*",
			"ui": "link:../ui",
			"legacy": "file:../legacy",
			"vendored": "portal:../vendored",
			"rel": "../sibling",
			"forked": "github:acme/forked",
			"tagged": "latest"
		},
		"devDependencies": { "zod": "3.22.4" }
	}`)

	x := newNpmAliasIndex(map[string]string{"": root})
	require.NotNil(t, x)

	external := []string{"graphql", "zod", "forked", "tagged"}
	for _, pkg := range external {
		assert.True(t, x.DeclaresExternalDependency("src/app.ts", pkg),
			"%q should be external", pkg)
	}
	inRepo := []string{"logger", "ui", "legacy", "vendored", "rel"}
	for _, pkg := range inRepo {
		assert.False(t, x.DeclaresExternalDependency("src/app.ts", pkg),
			"%q should resolve in-repo", pkg)
	}
}

// TestDeclaresExternalDependency_UndeclaredIsNotEvidence pins that a name no
// manifest declares reports false — absence is not evidence, and the
// resolver's entry-point rule still applies.
func TestDeclaresExternalDependency_UndeclaredIsNotEvidence(t *testing.T) {
	root := t.TempDir()
	writeManifest(t, root, ".", `{"dependencies": {"graphql": "^16.8.0"}}`)

	x := newNpmAliasIndex(map[string]string{"": root})
	require.NotNil(t, x)

	assert.False(t, x.DeclaresExternalDependency("src/app.ts", "never-declared"))
}

// TestDeclaresExternalDependency_NearestManifestWins pins npm resolution
// order: a nearer `workspace:*` shadows a farther registry range.
func TestDeclaresExternalDependency_NearestManifestWins(t *testing.T) {
	root := t.TempDir()
	writeManifest(t, root, ".", `{"dependencies": {"logger": "^2.0.0"}}`)
	writeManifest(t, root, "packages/app", `{"dependencies": {"logger": "workspace:*"}}`)

	x := newNpmAliasIndex(map[string]string{"": root})
	require.NotNil(t, x)

	assert.False(t, x.DeclaresExternalDependency("packages/app/src/main.ts", "logger"),
		"the nearer workspace: entry wins")
	assert.True(t, x.DeclaresExternalDependency("tools/cli/main.ts", "logger"),
		"outside that member the root registry range applies")
}

// TestDeclaresExternalDependency_ScopedAndSubpath pins that the package
// portion is what gets looked up, not the whole specifier.
func TestDeclaresExternalDependency_ScopedAndSubpath(t *testing.T) {
	root := t.TempDir()
	writeManifest(t, root, ".", `{"dependencies": {"@acme/utils": "^1.0.0", "lodash": "^4.17.21"}}`)

	x := newNpmAliasIndex(map[string]string{"": root})
	require.NotNil(t, x)

	assert.True(t, x.DeclaresExternalDependency("src/app.ts", "@acme/utils"))
	assert.True(t, x.DeclaresExternalDependency("src/app.ts", "@acme/utils/format"))
	assert.True(t, x.DeclaresExternalDependency("src/app.ts", "lodash/merge"))
}

// TestDeclaresExternalDependency_NonJSTSAndPathSpecifiers pins the cheap
// rejections: only JS/TS callers with bare specifiers are answered.
func TestDeclaresExternalDependency_NonJSTSAndPathSpecifiers(t *testing.T) {
	root := t.TempDir()
	writeManifest(t, root, ".", `{"dependencies": {"graphql": "^16.8.0"}}`)

	x := newNpmAliasIndex(map[string]string{"": root})
	require.NotNil(t, x)

	assert.False(t, x.DeclaresExternalDependency("app/main.py", "graphql"), "non-JS/TS caller")
	assert.False(t, x.DeclaresExternalDependency("src/app.ts", "./graphql"), "relative specifier")
	assert.False(t, x.DeclaresExternalDependency("src/app.ts", "/graphql"), "absolute specifier")
	assert.False(t, x.DeclaresExternalDependency("", "graphql"))
	assert.False(t, x.DeclaresExternalDependency("src/app.ts", ""))
}

// TestDeclaresExternalDependency_NilIndexIsSafe pins the nil receiver — the
// resolver treats a nil lookup as "no signal".
func TestDeclaresExternalDependency_NilIndexIsSafe(t *testing.T) {
	var x *npmAliasIndex
	assert.False(t, x.DeclaresExternalDependency("src/app.ts", "graphql"))
}

func TestNpmSpecResolvesInRepo(t *testing.T) {
	inRepo := []string{"workspace:*", "workspace:^1.0.0", "link:../ui", "portal:../ui", "file:../legacy", "./local", "../sibling", "/abs/path"}
	for _, v := range inRepo {
		assert.True(t, npmSpecResolvesInRepo(v), "%q", v)
	}
	external := []string{"^16.8.0", "~3.4.1", ">=2 <3", "latest", "*", "npm:@acme/real@1.0.0", "github:acme/forked", "git+ssh://git@host/x.git", "https://example.com/x.tgz", "catalog:", ""}
	for _, v := range external {
		assert.False(t, npmSpecResolvesInRepo(v), "%q", v)
	}
}
