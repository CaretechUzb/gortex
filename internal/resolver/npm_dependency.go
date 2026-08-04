package resolver

// NpmDependencyLookup reports whether a JS/TS importer declares a bare
// module specifier as a dependency that resolves OUTSIDE the repository —
// a registry semver range, a dist-tag, an `npm:` alias, a git/github spec,
// a tarball URL. It returns false for a specifier the importer does not
// declare at all, and for one declared as an in-repo dependency
// (`workspace:*`, `link:`, `portal:`, `file:`, a relative path), because
// those genuinely do name a directory in the tree.
//
// callerFile is the importing file's graph path (repo-prefixed in
// multi-repo mode); specifier is the module specifier with any
// `::<export>` payload already stripped.
//
// Like NpmAliasResolver and PathAliasResolver it lives in the resolver
// package so the resolver carries no compile-time dependency on the
// indexer or the filesystem: the indexer builds a concrete implementation
// (reading package.json from disk) and injects it via
// SetNpmDependencyLookup.
type NpmDependencyLookup func(callerFile, specifier string) bool

// SetNpmDependencyLookup installs an external-npm-dependency oracle. Pass
// nil to detach. Must be called before ResolveAll / ResolveFile — the
// resolver caches no dependency state across passes.
func (r *Resolver) SetNpmDependencyLookup(fn NpmDependencyLookup) { r.npmDep = fn }

// SetNpmDependencyLookup installs the oracle on the cross-repo resolver.
// Same contract as the Resolver method.
func (cr *CrossRepoResolver) SetNpmDependencyLookup(fn NpmDependencyLookup) { cr.npmDep = fn }

// declaresExternalNpmDep reports whether importPath's module specifier is
// declared by callerFile as a dependency resolving outside the repository.
//
// This is the strongest available evidence that a bare specifier is a
// node_modules package rather than an in-repo directory, and it is exactly
// the evidence isJSTSDirEntryPoint cannot supply: a repo that both depends
// on npm `graphql` AND contains a `src/feature/graphql/index.ts` clears the
// entry-point bar while still being a false binding (issue #450). When the
// manifest says the package comes from the registry, no in-repo directory
// is a candidate at all.
//
// A nil lookup (no indexer wired, or no usable repo root) reports false, so
// the entry-point rule remains the only gate — the pre-feature behaviour.
func declaresExternalNpmDep(fn NpmDependencyLookup, callerFile, importPath string) bool {
	if fn == nil || callerFile == "" {
		return false
	}
	spec, _ := splitImportSpecSymbol(importPath)
	if spec == "" {
		return false
	}
	return fn(callerFile, spec)
}
