package languages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// phpCallReceiver returns the receiver_type stamped on the call edge for the
// named method, plus whether such a call edge exists at all.
func phpCallReceiver(t *testing.T, src string, method string) (string, bool) {
	t.Helper()
	res, err := NewPHPExtractor().Extract("recv.php", []byte(src))
	require.NoError(t, err)
	for _, e := range res.Edges {
		if e.Kind != graph.EdgeCalls || e.To != "unresolved::*."+method {
			continue
		}
		if e.Meta == nil {
			return "", true
		}
		rt, _ := e.Meta["receiver_type"].(string)
		return rt, true
	}
	return "", false
}

func TestPHPReceiver_ThisIsEnclosingClass(t *testing.T) {
	rt, found := phpCallReceiver(t, `<?php
class StreamHandler {
    public function write(array $record): void {
        $this->getFormatter();
    }
}
`, "getFormatter")
	require.True(t, found, "no call edge for $this->getFormatter()")
	assert.Equal(t, "StreamHandler", rt)
}

func TestPHPReceiver_TypedParameter(t *testing.T) {
	rt, found := phpCallReceiver(t, `<?php
class Dispatcher {
    public function run(HandlerInterface $handler): void {
        $handler->handle();
    }
}
`, "handle")
	require.True(t, found)
	assert.Equal(t, "HandlerInterface", rt)
}

func TestPHPReceiver_NullableTypedParameterDropsQuestionMark(t *testing.T) {
	rt, found := phpCallReceiver(t, `<?php
function run(?Formatter $f): void {
    $f->format();
}
`, "format")
	require.True(t, found)
	assert.Equal(t, "Formatter", rt)
}

func TestPHPReceiver_NamespacedTypeReducedToSimpleName(t *testing.T) {
	rt, found := phpCallReceiver(t, `<?php
function run(\Monolog\Handler\StreamHandler $h): void {
    $h->close();
}
`, "close")
	require.True(t, found)
	assert.Equal(t, "StreamHandler", rt)
}

func TestPHPReceiver_LocalNewAssignment(t *testing.T) {
	rt, found := phpCallReceiver(t, `<?php
function boot(): void {
    $logger = new Logger('app');
    $logger->pushHandler();
}
`, "pushHandler")
	require.True(t, found)
	assert.Equal(t, "Logger", rt)
}

// A variable reassigned to a different class cannot be typed flow-insensitively;
// guessing one branch would bind the call to the wrong method at the resolved
// tier, so the environment drops the variable entirely.
func TestPHPReceiver_ConflictingNewAssignmentsStayUntyped(t *testing.T) {
	rt, found := phpCallReceiver(t, `<?php
function boot(bool $verbose): void {
    $h = new StreamHandler();
    if ($verbose) {
        $h = new RotatingFileHandler();
    }
    $h->handle();
}
`, "handle")
	require.True(t, found)
	assert.Equal(t, "", rt, "conflicting assignments must not type the receiver")
}

func TestPHPReceiver_TypedProperty(t *testing.T) {
	rt, found := phpCallReceiver(t, `<?php
class Service {
    private LoggerInterface $logger;

    public function run(): void {
        $this->logger->warning('x');
    }
}
`, "warning")
	require.True(t, found)
	assert.Equal(t, "LoggerInterface", rt)
}

// PHP 8 constructor property promotion declares the property in the parameter
// list, so the property type must be read from there too.
func TestPHPReceiver_PromotedConstructorProperty(t *testing.T) {
	rt, found := phpCallReceiver(t, `<?php
class Service {
    public function __construct(
        private readonly LoggerInterface $logger,
    ) {}

    public function run(): void {
        $this->logger->warning('x');
    }
}
`, "warning")
	require.True(t, found)
	assert.Equal(t, "LoggerInterface", rt)
}

func TestPHPReceiver_StaticScopeClass(t *testing.T) {
	rt, found := phpCallReceiver(t, `<?php
class Client {
    public function send(): void {
        Utils::describeType('x');
    }
}
`, "describeType")
	require.True(t, found)
	assert.Equal(t, "Utils", rt)
}

// parent:: / self:: / static:: keep their scope_kind tagging and are bound by
// the hierarchy walk, so they must not also carry a receiver_type that would
// pin them to the enclosing class.
func TestPHPReceiver_SelfAndParentKeepScopeKind(t *testing.T) {
	res, err := NewPHPExtractor().Extract("recv.php", []byte(`<?php
class Child extends Base {
    public function __construct() {
        parent::__construct();
        self::boot();
    }
}
`))
	require.NoError(t, err)
	kinds := map[string]string{}
	for _, e := range res.Edges {
		if e.Kind != graph.EdgeCalls || e.Meta == nil {
			continue
		}
		if sk, ok := e.Meta["scope_kind"].(string); ok {
			kinds[e.To] = sk
		}
		_, hasRecv := e.Meta["receiver_type"]
		assert.False(t, hasRecv, "scoped call %s must not carry receiver_type", e.To)
	}
	assert.Equal(t, "parent", kinds["unresolved::*.__construct"])
	assert.Equal(t, "self", kinds["unresolved::*.boot"])
}

func TestPHPReceiver_NullsafeCallEmitsEdge(t *testing.T) {
	rt, found := phpCallReceiver(t, `<?php
class Service {
    private ?Formatter $formatter = null;

    public function run(array $record): void {
        $this->formatter?->format($record);
    }
}
`, "format")
	require.True(t, found, "nullsafe call must emit a call edge")
	assert.Equal(t, "Formatter", rt)
}

func TestPHPReceiver_NullsafeChainEmitsEveryHop(t *testing.T) {
	res, err := NewPHPExtractor().Extract("recv.php", []byte(`<?php
class Service {
    public function run(): void {
        $this->client?->request()?->getBody();
    }
}
`))
	require.NoError(t, err)
	got := map[string]bool{}
	for _, e := range res.Edges {
		if e.Kind == graph.EdgeCalls {
			got[e.To] = true
		}
	}
	assert.True(t, got["unresolved::*.request"], "missing ?->request() edge")
	assert.True(t, got["unresolved::*.getBody"], "missing ?->getBody() edge")
}

// A closure parameter shadows an outer variable of the same name; typing the
// call from the outer binding would attribute it to the wrong class.
func TestPHPReceiver_ClosureParameterShadowsOuterVariable(t *testing.T) {
	rt, found := phpCallReceiver(t, `<?php
function boot(): void {
    $h = new StreamHandler();
    $fn = function (RotatingFileHandler $h) {
        $h->close();
    };
}
`, "close")
	require.True(t, found)
	assert.Equal(t, "RotatingFileHandler", rt)
}

// A closure inherits $this from the enclosing method (PHP binds it unless the
// closure is declared static), so calls inside it still type against the class.
func TestPHPReceiver_ClosureInheritsThis(t *testing.T) {
	rt, found := phpCallReceiver(t, `<?php
class Runner {
    public function run(): void {
        $fn = function () {
            $this->tick();
        };
    }
}
`, "tick")
	require.True(t, found)
	assert.Equal(t, "Runner", rt)
}

func TestPHPReceiver_StaticClosureDropsThis(t *testing.T) {
	rt, found := phpCallReceiver(t, `<?php
class Runner {
    public function run(): void {
        $fn = static function () {
            $this->tick();
        };
    }
}
`, "tick")
	require.True(t, found)
	assert.Equal(t, "", rt, "a static closure does not bind $this")
}

func TestPHPReceiver_CatchClauseTypesVariable(t *testing.T) {
	rt, found := phpCallReceiver(t, `<?php
function boot(): void {
    try {
        risky();
    } catch (ConnectionException $e) {
        $e->getContext();
    }
}
`, "getContext")
	require.True(t, found)
	assert.Equal(t, "ConnectionException", rt)
}

func TestPHPReceiver_DocVarComment(t *testing.T) {
	rt, found := phpCallReceiver(t, `<?php
function boot(array $all): void {
    /** @var StreamHandler $h */
    $h = $all[0];
    $h->close();
}
`, "close")
	require.True(t, found)
	assert.Equal(t, "StreamHandler", rt)
}

// A union names ALTERNATIVES — the value is one of them, so picking a branch
// would bind the call to the wrong class at the resolved tier.
func TestPHPReceiver_UnionStaysUntyped(t *testing.T) {
	rt, found := phpCallReceiver(t, `<?php
function run(StreamHandler|NullHandler $h): void { $h->close(); }
`, "close")
	require.True(t, found)
	assert.Equal(t, "", rt)
}

// An intersection is the opposite: the value is EVERY branch at once, so any
// branch is a true statement about it. `Foo&MockObject` is the idiomatic type
// of a mocked collaborator in a modern PHP test suite, and leaving it untyped
// gave up the whole call. If the method lives on a different branch the
// hierarchy walk from this one finds nothing and the call stays unresolved —
// never a wrong bind.
func TestPHPReceiver_IntersectionTakesFirstBranch(t *testing.T) {
	rt, found := phpCallReceiver(t, `<?php
function run(PushoverHandler&MockObject $h): void { $h->handle(); }
`, "handle")
	require.True(t, found)
	assert.Equal(t, "PushoverHandler", rt)
}

// A builtin branch names no graph node, so the first bindable one is taken.
func TestPHPReceiver_IntersectionSkipsBuiltinBranch(t *testing.T) {
	rt, found := phpCallReceiver(t, `<?php
function run(iterable&Traversable $h): void { $h->rewind(); }
`, "rewind")
	require.True(t, found)
	assert.Equal(t, "Traversable", rt)
}

// A nullable union is still a union.
func TestPHPReceiver_NullableUnionStaysUntyped(t *testing.T) {
	rt, found := phpCallReceiver(t, `<?php
function run(StreamHandler|NullHandler|null $h): void { $h->close(); }
`, "close")
	require.True(t, found)
	assert.Equal(t, "", rt)
}

// A builtin scalar type names no graph node, so it must not be stamped.
func TestPHPReceiver_BuiltinTypesNotStamped(t *testing.T) {
	rt, found := phpCallReceiver(t, `<?php
function run(string $s): void { $s->format(); }
`, "format")
	require.True(t, found)
	assert.Equal(t, "", rt)
}

// A variadic parameter is an array of the declared type, not an instance, so
// typing it would misbind calls on the array itself.
func TestPHPReceiver_VariadicParameterNotStamped(t *testing.T) {
	rt, found := phpCallReceiver(t, `<?php
function run(Handler ...$handlers): void { $handlers->close(); }
`, "close")
	require.True(t, found)
	assert.Equal(t, "", rt)
}

func TestPHPReceiver_NewExpressionReceiver(t *testing.T) {
	rt, found := phpCallReceiver(t, `<?php
function run(): void {
    (new StreamHandler())->close();
}
`, "close")
	require.True(t, found)
	assert.Equal(t, "StreamHandler", rt)
}

// `new $cls` / `new class {}` name no static class, so nothing is stamped.
func TestPHPReceiver_DynamicNewStaysUntyped(t *testing.T) {
	rt, found := phpCallReceiver(t, `<?php
function run(string $cls): void {
    $h = new $cls();
    $h->close();
}
`, "close")
	require.True(t, found)
	assert.Equal(t, "", rt)
}
