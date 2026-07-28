package languages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

func phpExtract(t *testing.T, path, src string) ([]*graph.Node, []*graph.Edge) {
	t.Helper()
	res, err := NewPHPExtractor().Extract(path, []byte(src))
	require.NoError(t, err)
	return res.Nodes, res.Edges
}

func phpNodeNamed(nodes []*graph.Node, name string) *graph.Node {
	for _, n := range nodes {
		if n.Name == name {
			return n
		}
	}
	return nil
}

func TestPHPNamespace_DeclarationsCarryNamespace(t *testing.T) {
	nodes, _ := phpExtract(t, "src/Models/User.php", `<?php
namespace App\Models;

class User {
    public const ROLE = 'admin';
    private int $id = 0;
    public function name(): string { return ''; }
}

interface Nameable {}

function helper(): void {}
`)
	for _, name := range []string{"User", "Nameable", "helper", "ROLE", "id", "name"} {
		n := phpNodeNamed(nodes, name)
		require.NotNil(t, n, "no node for %s", name)
		assert.Equal(t, `App\Models`, n.Meta["scope_ns"], "scope_ns on %s", name)
	}
}

// Node.QualName backs a UNIQUE SQLite index — a second node with the same
// non-empty qual_name fails the insert outright. PHP mints duplicate
// fully-qualified names in practice, so the namespace rides Meta instead and
// QualName is deliberately left empty.
func TestPHPNamespace_QualNameNeverStamped(t *testing.T) {
	nodes, _ := phpExtract(t, "src/Models/User.php", `<?php
namespace App\Models;

class User {
    public function name(): string { return ''; }
}
function helper(): void {}
`)
	for _, n := range nodes {
		assert.Empty(t, n.QualName, "node %s must not claim a unique qual_name slot", n.ID)
	}
}

func TestPHPNamespace_GlobalNamespaceFileUnchanged(t *testing.T) {
	nodes, _ := phpExtract(t, "legacy.php", `<?php
class Legacy {}
`)
	cls := phpNodeNamed(nodes, "Legacy")
	require.NotNil(t, cls)
	assert.NotContains(t, cls.Meta, "scope_ns")
}

// phpTargetFQN returns the target_fqn stamped on the first edge of the given
// kind pointing at the given bare name.
func phpTargetFQN(edges []*graph.Edge, kind graph.EdgeKind, name string) (string, bool) {
	for _, e := range edges {
		if e.Kind != kind || e.To != "unresolved::"+name {
			continue
		}
		if e.Meta == nil {
			return "", true
		}
		s, _ := e.Meta["target_fqn"].(string)
		return s, true
	}
	return "", false
}

func TestPHPNamespace_ImportedReferenceResolvesToImportedFQN(t *testing.T) {
	_, edges := phpExtract(t, "src/Web/Controller.php", `<?php
namespace App\Web;

use App\Models\User;

class Controller {
    public function show(): void {
        $u = new User();
    }
}
`)
	fqn, found := phpTargetFQN(edges, graph.EdgeInstantiates, "User")
	require.True(t, found, "no instantiates edge for User")
	assert.Equal(t, `App\Models\User`, fqn)
}

// An unimported bare name resolves in the current namespace — PHP's own rule
// for class references.
func TestPHPNamespace_UnimportedReferenceUsesCurrentNamespace(t *testing.T) {
	_, edges := phpExtract(t, "src/Models/Post.php", `<?php
namespace App\Models;

class Post {
    public function author(): void { $u = new User(); }
}
`)
	fqn, found := phpTargetFQN(edges, graph.EdgeInstantiates, "User")
	require.True(t, found)
	assert.Equal(t, `App\Models\User`, fqn)
}

func TestPHPNamespace_AliasedImportResolvesToTarget(t *testing.T) {
	_, edges := phpExtract(t, "src/Web/C.php", `<?php
namespace App\Web;

use App\Models\User as Account;

class C {
    public function f(): void { $a = new Account(); }
}
`)
	fqn, found := phpTargetFQN(edges, graph.EdgeInstantiates, "Account")
	require.True(t, found)
	assert.Equal(t, `App\Models\User`, fqn)
}

func TestPHPNamespace_GroupedImportResolves(t *testing.T) {
	_, edges := phpExtract(t, "src/Web/C.php", `<?php
namespace App\Web;

use App\Models\{User, Post};

class C {
    public function f(): void { $p = new Post(); }
}
`)
	fqn, found := phpTargetFQN(edges, graph.EdgeInstantiates, "Post")
	require.True(t, found)
	assert.Equal(t, `App\Models\Post`, fqn)
}

// A leading `\` is already absolute and must not be re-prefixed.
func TestPHPNamespace_FullyQualifiedInlineReference(t *testing.T) {
	_, edges := phpExtract(t, "src/Web/C.php", `<?php
namespace App\Web;

class C extends \App\Core\Base {}
`)
	fqn, found := phpTargetFQN(edges, graph.EdgeExtends, "Base")
	require.True(t, found)
	assert.Equal(t, `App\Core\Base`, fqn)
}

// A qualified reference whose first segment is imported joins the two.
func TestPHPNamespace_QualifiedReferenceThroughImportedPrefix(t *testing.T) {
	_, edges := phpExtract(t, "src/Web/C.php", `<?php
namespace App\Web;

use App\Models;

class C extends Models\User {}
`)
	fqn, found := phpTargetFQN(edges, graph.EdgeExtends, "User")
	require.True(t, found)
	assert.Equal(t, `App\Models\User`, fqn)
}

// Several braced namespaces in one file each claim their own declarations.
func TestPHPNamespace_MultipleBracedNamespacesInOneFile(t *testing.T) {
	nodes, _ := phpExtract(t, "multi.php", `<?php
namespace App\Alpha {
    class First {}
}
namespace App\Beta {
    class Second {}
}
`)
	first := phpNodeNamed(nodes, "First")
	require.NotNil(t, first)
	assert.Equal(t, `App\Alpha`, first.Meta["scope_ns"])

	second := phpNodeNamed(nodes, "Second")
	require.NotNil(t, second)
	assert.Equal(t, `App\Beta`, second.Meta["scope_ns"])
}

// `use function NS\foo as bar` feeds the resolver's scope_use_aliases contract,
// which translates the alias before searching.
func TestPHPNamespace_UseFunctionAliasStampedOnCallEdges(t *testing.T) {
	_, edges := phpExtract(t, "src/Web/C.php", `<?php
namespace App\Web;

use function App\Support\jsonEncode as toJson;

class C {
    public function f(): void { toJson([]); }
}
`)
	var found bool
	for _, e := range edges {
		if e.Kind != graph.EdgeCalls || e.Meta == nil {
			continue
		}
		if s, _ := e.Meta["scope_use_aliases"].(string); s != "" {
			found = true
			assert.Equal(t, `toJson=>App\Support\jsonEncode`, s)
		}
	}
	assert.True(t, found, "no call edge carried scope_use_aliases")
}

// A `use function` import must not become a class import — the two live in
// separate PHP symbol tables and a class reference of the same name is not it.
func TestPHPNamespace_UseFunctionIsNotAClassImport(t *testing.T) {
	_, edges := phpExtract(t, "src/Web/C.php", `<?php
namespace App\Web;

use function App\Support\User;

class C extends User {}
`)
	fqn, found := phpTargetFQN(edges, graph.EdgeExtends, "User")
	require.True(t, found)
	assert.Equal(t, `App\Web\User`, fqn,
		"a function import must not qualify a class reference")
}
