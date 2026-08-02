package goanalysis

import (
	"go/ast"
	"go/token"
	"go/types"
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/tools/go/packages"
)

func TestReleaseGoPackageCompilerStateKeepsExternalMetadata(t *testing.T) {
	module := &packages.Module{Path: "example.com/mod", Version: "v1.2.3"}
	fset := token.NewFileSet()
	pkg := &packages.Package{
		ID:              "example.com/mod/p",
		Name:            "p",
		PkgPath:         "example.com/mod/p",
		Dir:             "/repo/p",
		GoFiles:         []string{"p.go"},
		CompiledGoFiles: []string{"p.go"},
		OtherFiles:      []string{"p.s"},
		EmbedFiles:      []string{"asset.txt"},
		EmbedPatterns:   []string{"*.txt"},
		IgnoredFiles:    []string{"ignored.go"},
		Imports:         map[string]*packages.Package{"fmt": {PkgPath: "fmt"}},
		Module:          module,
		Types:           types.NewPackage("example.com/mod/p", "p"),
		Fset:            fset,
		Syntax:          []*ast.File{{Name: ast.NewIdent("p")}},
		TypesInfo:       &types.Info{Uses: map[*ast.Ident]types.Object{}},
		TypesSizes:      types.SizesFor("gc", "amd64"),
	}

	releaseGoPackageCompilerState(pkg)

	assert.Equal(t, "example.com/mod/p", pkg.ID)
	assert.Equal(t, "p", pkg.Name)
	assert.Equal(t, "example.com/mod/p", pkg.PkgPath)
	assert.Equal(t, "/repo/p", pkg.Dir)
	assert.Same(t, module, pkg.Module)
	assert.Nil(t, pkg.GoFiles)
	assert.Nil(t, pkg.CompiledGoFiles)
	assert.Nil(t, pkg.OtherFiles)
	assert.Nil(t, pkg.EmbedFiles)
	assert.Nil(t, pkg.EmbedPatterns)
	assert.Nil(t, pkg.IgnoredFiles)
	assert.Nil(t, pkg.Imports)
	assert.Nil(t, pkg.Types)
	assert.Nil(t, pkg.Fset)
	assert.Nil(t, pkg.Syntax)
	assert.Nil(t, pkg.TypesInfo)
	assert.Nil(t, pkg.TypesSizes)
}

func TestProjectedExternalUseInternsStringOnlyIdentity(t *testing.T) {
	typesPkg := types.NewPackage("example.com/dep", "dep")
	fn := types.NewFunc(token.NoPos, typesPkg, "Run", types.NewSignatureType(nil, nil, nil, nil, nil, false))
	externals := &externalsAttribution{
		repoPrefix:  "repo",
		pkgByPath:   map[string]*packages.Package{"example.com/dep": {PkgPath: "example.com/dep"}},
		extByObj:    map[types.Object]string{fn: "ext::go:example.com/dep::Run"},
		useByNodeID: make(map[string]*externalUseIdentity),
	}

	first := externals.projectedUse(fn, "ext::go:example.com/dep::Run")
	second := externals.projectedUse(fn, "ext::go:example.com/dep::Run")

	require.NotNil(t, first)
	assert.Same(t, first, second)
	assert.Equal(t, "Run", first.name)
	externals.releaseCompilerReferences()
	assert.Nil(t, externals.extByObj)
	assert.Nil(t, externals.pkgByPath)
	assert.Same(t, first, externals.useByNodeID["ext::go:example.com/dep::Run"])
	assert.Equal(t, [3]string{
		"repo::stdlib::example.com/dep::Run",
		"dep::example.com/dep::Run",
		"unresolved::extern::example.com/dep::Run",
	}, first.stubTargets)

	compilerObject := reflect.TypeOf((*types.Object)(nil)).Elem()
	planType := reflect.TypeOf(resolvedGoUse{})
	for i := 0; i < planType.NumField(); i++ {
		field := planType.Field(i)
		assert.False(t, field.Type.Implements(compilerObject), "field %s retains types.Object", field.Name)
	}
}
