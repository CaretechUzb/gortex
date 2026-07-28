package languages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// phpNodesByName indexes an extraction's nodes by name.
func phpNodesByName(t *testing.T, src string) map[string]*graph.Node {
	t.Helper()
	res, err := NewPHPExtractor().Extract("decl.php", []byte(src))
	require.NoError(t, err)
	out := map[string]*graph.Node{}
	for _, n := range res.Nodes {
		out[n.Name] = n
	}
	return out
}

func TestPHPDeclarations_PromotedConstructorPropertiesAreFields(t *testing.T) {
	res, err := NewPHPExtractor().Extract("decl.php", []byte(`<?php
class Service {
    public function __construct(
        private readonly LoggerInterface $logger,
        protected int $tries = 3,
        $notAProperty = null,
    ) {}
}
`))
	require.NoError(t, err)

	fields := map[string]*graph.Node{}
	for _, n := range res.Nodes {
		if n.Kind == graph.KindField {
			fields[n.Name] = n
		}
	}
	require.Contains(t, fields, "logger", "promoted property must mint a field, got %v", fields)
	assert.Equal(t, "private", fields["logger"].Meta["visibility"])
	assert.Equal(t, true, fields["logger"].Meta["readonly"])
	assert.Equal(t, true, fields["logger"].Meta["promoted"])
	assert.Equal(t, "LoggerInterface", fields["logger"].Meta["field_type"])
	assert.Equal(t, "Service", fields["logger"].Meta["receiver"])

	require.Contains(t, fields, "tries")
	assert.Equal(t, "protected", fields["tries"].Meta["visibility"])

	assert.NotContains(t, fields, "notAProperty",
		"an ordinary parameter is not a promoted property")

	// The field belongs to its class, so find_usages on Service reaches it.
	var memberOf bool
	for _, e := range res.Edges {
		if e.Kind == graph.EdgeMemberOf && e.From == fields["logger"].ID && e.To == "decl.php::Service" {
			memberOf = true
		}
	}
	assert.True(t, memberOf, "promoted property needs a member_of edge to its class")
}

func TestPHPDeclarations_MemberModifiersRecorded(t *testing.T) {
	nodes := phpNodesByName(t, `<?php
abstract class Base {
    public static array $registry = [];
    public readonly string $id;

    abstract public function run(): void;
    final public static function make(): static { return new static(); }
}
`)
	require.Contains(t, nodes, "registry")
	assert.Equal(t, true, nodes["registry"].Meta["static"])
	require.Contains(t, nodes, "id")
	assert.Equal(t, true, nodes["id"].Meta["readonly"])

	require.Contains(t, nodes, "run")
	assert.Equal(t, true, nodes["run"].Meta["abstract"])
	require.Contains(t, nodes, "make")
	assert.Equal(t, true, nodes["make"].Meta["final"])
	assert.Equal(t, true, nodes["make"].Meta["static"])
}

func TestPHPDeclarations_FileScopeConstAndDefine(t *testing.T) {
	nodes := phpNodesByName(t, `<?php
const APP_VERSION = '1.2.3';
define('APP_ROOT', __DIR__);
define($computed, 1);
`)
	require.Contains(t, nodes, "APP_VERSION", "file-scope const must be a symbol, got %v", nodes)
	assert.Equal(t, graph.KindConstant, nodes["APP_VERSION"].Kind)
	require.Contains(t, nodes, "APP_ROOT", "define() must be a symbol")
	assert.Equal(t, graph.KindConstant, nodes["APP_ROOT"].Kind)
}

// A class-body const must stay a class member, not be re-minted at file scope.
func TestPHPDeclarations_ClassConstStaysAMember(t *testing.T) {
	res, err := NewPHPExtractor().Extract("decl.php", []byte(`<?php
class Levels {
    public const DEBUG = 100;
}
`))
	require.NoError(t, err)
	var count int
	for _, n := range res.Nodes {
		if n.Kind == graph.KindConstant && n.Name == "DEBUG" {
			count++
			assert.Equal(t, "Levels", n.Meta["receiver"])
		}
	}
	assert.Equal(t, 1, count, "class constant must be minted exactly once")
}

func TestPHPDeclarations_EnumImplementsClause(t *testing.T) {
	nodes := phpNodesByName(t, `<?php
enum Suit: string implements HasColor, JsonSerializable {
    case Hearts = 'H';
    public function color(): string { return 'red'; }
}
`)
	require.Contains(t, nodes, "Suit")
	ifaces, _ := nodes["Suit"].Meta["scope_interfaces"].(string)
	assert.Contains(t, ifaces, "HasColor")
	assert.Contains(t, ifaces, "JsonSerializable")
	assert.Equal(t, "string", nodes["Suit"].Meta["backing_type"])
}

// PHP 8.2 disjunctive normal form parenthesises its intersection groups; the
// parens must not leak into a target name.
func TestPHPDeclarations_DisjunctiveNormalFormType(t *testing.T) {
	res, err := NewPHPExtractor().Extract("decl.php", []byte(`<?php
class Box {
    private (Countable&Traversable)|null $items = null;
}
`))
	require.NoError(t, err)
	var targets []string
	for _, e := range res.Edges {
		if e.Kind == graph.EdgeTypedAs {
			targets = append(targets, e.To)
		}
	}
	assert.Contains(t, targets, "unresolved::Countable")
	assert.Contains(t, targets, "unresolved::Traversable")
	for _, tgt := range targets {
		assert.NotContains(t, tgt, "(", "a parenthesised group must not leak into %q", tgt)
	}
}
