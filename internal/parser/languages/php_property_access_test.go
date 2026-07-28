package languages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// phpAccessEdges returns every read/write edge in the extraction, keyed by
// "<kind> <target>" with the stamped receiver_type as the value.
func phpAccessEdges(t *testing.T, src string) map[string]string {
	t.Helper()
	res, err := NewPHPExtractor().Extract("acc.php", []byte(src))
	require.NoError(t, err)
	out := map[string]string{}
	for _, e := range res.Edges {
		if e.Kind != graph.EdgeReads && e.Kind != graph.EdgeWrites {
			continue
		}
		rt := ""
		if e.Meta != nil {
			rt, _ = e.Meta["receiver_type"].(string)
		}
		out[string(e.Kind)+" "+e.To] = rt
	}
	return out
}

func TestPHPPropertyAccess_ReadAndWriteOnThis(t *testing.T) {
	got := phpAccessEdges(t, `<?php
class Logger {
    private array $handlers = [];

    public function push($h): void {
        $this->handlers[] = $h;
    }

    public function all(): array {
        return $this->handlers;
    }
}
`)
	rt, ok := got["reads unresolved::*.handlers"]
	require.True(t, ok, "no read edge for $this->handlers, got %v", got)
	assert.Equal(t, "Logger", rt)

	// `$this->handlers[] = $h` mutates the property, so the subscripted
	// assignment is a write of `handlers`, not a read of it.
	rt, ok = got["writes unresolved::*.handlers"]
	require.True(t, ok, "no write edge for $this->handlers[] = ..., got %v", got)
	assert.Equal(t, "Logger", rt)
}

// A member access used as a subscript INDEX is read, not written — only the
// container operand of `$a[...] = v` is the assignment target.
func TestPHPPropertyAccess_SubscriptIndexIsARead(t *testing.T) {
	got := phpAccessEdges(t, `<?php
class Cache {
    private string $key = 'k';

    public function put(array &$store, $v): void {
        $store[$this->key] = $v;
    }
}
`)
	assert.Contains(t, got, "reads unresolved::*.key")
	assert.NotContains(t, got, "writes unresolved::*.key")
}

func TestPHPPropertyAccess_AssignmentIsAWrite(t *testing.T) {
	got := phpAccessEdges(t, `<?php
class Logger {
    private ?Formatter $formatter = null;

    public function setFormatter(Formatter $f): void {
        $this->formatter = $f;
    }
}
`)
	rt, ok := got["writes unresolved::*.formatter"]
	require.True(t, ok, "assignment must emit a write, got %v", got)
	assert.Equal(t, "Logger", rt)
	_, alsoRead := got["reads unresolved::*.formatter"]
	assert.False(t, alsoRead, "an assignment target must not also read")
}

func TestPHPPropertyAccess_CompoundAndUpdateAreWrites(t *testing.T) {
	got := phpAccessEdges(t, `<?php
class Counter {
    private int $hits = 0;
    private string $log = '';

    public function bump(): void {
        $this->hits++;
        $this->log .= 'x';
        unset($this->log);
    }
}
`)
	assert.Contains(t, got, "writes unresolved::*.hits", "++ is a write")
	assert.Contains(t, got, "writes unresolved::*.log", "compound assign / unset is a write")
}

func TestPHPPropertyAccess_TypedPropertyChainTypesTheInnerRead(t *testing.T) {
	got := phpAccessEdges(t, `<?php
class Service {
    private LoggerInterface $logger;

    public function run(): void {
        $this->logger->warning('x');
    }
}
`)
	rt, ok := got["reads unresolved::*.logger"]
	require.True(t, ok, "the receiver of a chained call is itself a property read")
	assert.Equal(t, "Service", rt)
}

func TestPHPPropertyAccess_StaticPropertyAndClassConstant(t *testing.T) {
	got := phpAccessEdges(t, `<?php
class Boot {
    public function run(): void {
        Registry::$instances[] = 1;
        $level = Logger::DEBUG;
    }
}
`)
	rt, ok := got["writes unresolved::*.instances"]
	require.True(t, ok, "Foo::$prop assignment must emit a write, got %v", got)
	assert.Equal(t, "Registry", rt)

	rt, ok = got["reads unresolved::*.DEBUG"]
	require.True(t, ok, "Foo::CONST must emit a read, got %v", got)
	assert.Equal(t, "Logger", rt)
}

// `Foo::class` is the class-name literal, not a constant member — the type
// reference it carries is emitted by the reference-form pass instead.
func TestPHPPropertyAccess_ClassKeywordIsNotAConstantRead(t *testing.T) {
	got := phpAccessEdges(t, `<?php
class Boot {
    public function run(): string { return StreamHandler::class; }
}
`)
	assert.NotContains(t, got, "reads unresolved::*.class")
	assert.Empty(t, got, "Foo::class must emit no member access at all, got %v", got)
}

// A dynamic member names no symbol; emitting one would put an unresolvable
// target like `*.$prop` in the graph.
func TestPHPPropertyAccess_DynamicMembersSkipped(t *testing.T) {
	got := phpAccessEdges(t, `<?php
class Boot {
    public function run(string $k, object $o): void {
        $x = $o->$k;
        $y = $o->{$k . '_id'};
    }
}
`)
	assert.Empty(t, got, "dynamic property fetches must emit nothing, got %v", got)
}

func TestPHPPropertyAccess_NullsafePropertyRead(t *testing.T) {
	got := phpAccessEdges(t, `<?php
class Service {
    private ?Config $config = null;

    public function run(): void {
        $v = $this->config?->timeout;
    }
}
`)
	assert.Contains(t, got, "reads unresolved::*.config")
	rt, ok := got["reads unresolved::*.timeout"]
	require.True(t, ok, "nullsafe property read must emit an edge, got %v", got)
	assert.Equal(t, "Config", rt)
}

// `self::` and `static::` name the enclosing class, so a static property or
// class constant reached through them binds like any other typed access.
// `parent::` deliberately stays untyped — the scope_kind path walks up from the
// enclosing class rather than pinning to it.
func TestPHPPropertyAccess_RelativeScopeTypedByEnclosingClass(t *testing.T) {
	got := phpAccessEdges(t, `<?php
class Registry extends BaseRegistry {
    private static array $items = [];
    public const LIMIT = 10;

    public function add($x): void {
        self::$items[] = $x;
        $n = static::LIMIT;
        $p = parent::DEFAULTS;
    }
}
`)
	assert.Equal(t, "Registry", got["writes unresolved::*.items"], "self::$prop is the enclosing class, got %v", got)
	assert.Equal(t, "Registry", got["reads unresolved::*.LIMIT"], "static::CONST is the enclosing class, got %v", got)
	_, ok := got["reads unresolved::*.DEFAULTS"]
	require.True(t, ok, "parent:: access must still emit an edge, got %v", got)
	assert.Equal(t, "", got["reads unresolved::*.DEFAULTS"], "parent:: must not pin to the enclosing class")
}
