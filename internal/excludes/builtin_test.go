package excludes

import "testing"

func TestBuiltinExcludesVersionControlMetadata(t *testing.T) {
	t.Parallel()

	matcher := New(Builtin)
	for _, path := range []string{
		".git/objects/pack/pack.data",
		".jj/repo/store/type",
		".hg/store/data/file.i",
		".svn/pristine/00/file.svn-base",
		".bzr/repository/pack-names",
		"_darcs/inventories/000000",
		".pijul/changes/ABC.change",
		".fossil-settings/ignore-glob",
		"nested/.jj/repo/store/type",
	} {
		if !matcher.MatchRel(path) {
			t.Errorf("Builtin should exclude VCS metadata path %q", path)
		}
	}
}

func TestBuiltinDoesNotExcludeSimilarSourcePaths(t *testing.T) {
	t.Parallel()

	matcher := New(Builtin)
	for _, path := range []string{
		"jj/repository.go",
		"pijul/client.go",
		"fossil-settings/parser.go",
		"darcs/model.go",
	} {
		if matcher.MatchRel(path) {
			t.Errorf("Builtin unexpectedly excludes source path %q", path)
		}
	}
}

func TestBuiltinExcludesVisualStudioArtifacts(t *testing.T) {
	t.Parallel()

	matcher := New(Builtin)
	for _, path := range []string{
		".vs/Solution/v17/HierarchyCache.v1.txt",
		".vs/Solution/v18/HierarchyCache.v1.txt",
		"TestResults/run-1/results.trx",
		"App/obj/Debug/net8.0/App.AssemblyInfo.cs",
		"App/obj/Debug/net8.0/App.GlobalUsings.g.cs",
		"App/obj/Debug/net8.0/App.csproj.FileListAbsolute.txt",
		"App/obj/Release/net8.0/App.AssemblyInfo.cs",
		"App/obj/release/net8.0/App.AssemblyInfo.cs",
		"App/obj/project.assets.json",
		"App/obj/project.nuget.cache",
		"App/obj/App.csproj.nuget.g.props",
		"App/obj/App.csproj.nuget.dgspec.json",
		"obj/project.assets.json",
	} {
		if !matcher.MatchRel(path) {
			t.Errorf("Builtin should exclude MSBuild artifact %q", path)
		}
	}
}

// The MSBuild entries name what MSBuild writes rather than blanket-dropping
// `obj/` or `bin/`, both of which are committed source in other ecosystems.
func TestBuiltinDoesNotExcludeFirstPartyObjOrBin(t *testing.T) {
	t.Parallel()

	matcher := New(Builtin)
	for _, path := range []string{
		"cmd/internal/obj/link.go", // Go's own toolchain
		"src/cmd/internal/obj/x86/asm6.go",
		"obj/writer.go",
		"bin/rails",
		"bin/cli.js",
		"bin/setup",
		"App/Controllers/HomeController.cs",
	} {
		if matcher.MatchRel(path) {
			t.Errorf("Builtin unexpectedly excludes first-party path %q", path)
		}
	}
}
