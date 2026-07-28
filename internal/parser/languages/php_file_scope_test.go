package languages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// phpEdgesFrom returns every edge of the given kind grouped by source node id.
func phpEdgesFrom(t *testing.T, src string, kind graph.EdgeKind) map[string][]string {
	t.Helper()
	res, err := NewPHPExtractor().Extract("scope.php", []byte(src))
	require.NoError(t, err)
	out := map[string][]string{}
	for _, e := range res.Edges {
		if e.Kind == kind {
			out[e.From] = append(out[e.From], e.To)
		}
	}
	return out
}

func TestPHPFileScope_TopLevelCallsAttributedToFile(t *testing.T) {
	calls := phpEdgesFrom(t, `<?php
$app = new Application();
$app->setName('demo');
bootstrap();
`, graph.EdgeCalls)
	require.Contains(t, calls, "scope.php", "top-level calls must be attributed to the file node")
	assert.Contains(t, calls["scope.php"], "unresolved::*.setName")
	assert.Contains(t, calls["scope.php"], "unresolved::*.bootstrap")
}

// A `$v = new Foo` binding made at file scope types the receiver of a later
// top-level call exactly as it would inside a function.
func TestPHPFileScope_TopLevelNewBindingTypesReceiver(t *testing.T) {
	res, err := NewPHPExtractor().Extract("scope.php", []byte(`<?php
$app = new Application();
$app->run();
`))
	require.NoError(t, err)
	var found bool
	for _, e := range res.Edges {
		if e.Kind == graph.EdgeCalls && e.To == "unresolved::*.run" {
			found = true
			require.NotNil(t, e.Meta)
			assert.Equal(t, "Application", e.Meta["receiver_type"])
		}
	}
	assert.True(t, found, "no call edge for $app->run()")
}

// A file whose whole body is `return function (...) {...}` — the shape every
// symfony/console style fixture uses — has no symbol to own the closure, so
// its calls belong to the file.
func TestPHPFileScope_ReturnedClosureBodyIsWalked(t *testing.T) {
	calls := phpEdgesFrom(t, `<?php
return function (InputInterface $input, OutputInterface $output) {
    $io = new SymfonyStyle($input, $output);
    $io->caution('boom');
    $output->writeln('done');
};
`, graph.EdgeCalls)
	assert.Contains(t, calls["scope.php"], "unresolved::*.caution")
	assert.Contains(t, calls["scope.php"], "unresolved::*.writeln")
}

// The file-scope walk must stop at every declaration, otherwise a method body
// is counted once for its method and again for the file.
func TestPHPFileScope_DeclarationBodiesNotDoubleCounted(t *testing.T) {
	calls := phpEdgesFrom(t, `<?php
class Service {
    public function run(): void { $this->tick(); }
}
function helper(): void { sideEffect(); }
kickOff();
`, graph.EdgeCalls)

	assert.Equal(t, []string{"unresolved::*.tick"}, calls["scope.php::Service.run"])
	assert.Equal(t, []string{"unresolved::*.sideEffect"}, calls["scope.php::helper"])
	assert.Equal(t, []string{"unresolved::*.kickOff"}, calls["scope.php"],
		"the file node must own only the top-level call")
}

func TestPHPAnonymousClass_MintsTypeAndMembers(t *testing.T) {
	res, err := NewPHPExtractor().Extract("scope.php", []byte(`<?php
$h = new class extends AbstractHandler implements HandlerInterface {
    public function handle(array $record): bool {
        return $this->isHandling($record);
    }
};
`))
	require.NoError(t, err)

	var cls, method *graph.Node
	for _, n := range res.Nodes {
		switch {
		case n.Kind == graph.KindType && n.Meta != nil && n.Meta["anonymous"] == true:
			cls = n
		case n.Kind == graph.KindMethod && n.Name == "handle":
			method = n
		}
	}
	require.NotNil(t, cls, "anonymous class must mint a type node")
	assert.Equal(t, "AbstractHandler", cls.Meta["scope_parent"])
	assert.Equal(t, "HandlerInterface", cls.Meta["scope_interfaces"])

	require.NotNil(t, method, "anonymous class methods must be symbols")
	assert.Equal(t, cls.Name, method.Meta["receiver"],
		"the method's receiver names its anonymous class, so receiver-directed resolution stays precise")

	// The call inside the anonymous method belongs to that method, not to the
	// file or to whatever expression the class was written in.
	calls := map[string][]string{}
	for _, e := range res.Edges {
		if e.Kind == graph.EdgeCalls {
			calls[e.From] = append(calls[e.From], e.To)
		}
	}
	assert.Contains(t, calls[method.ID], "unresolved::*.isHandling")
	assert.NotContains(t, calls["scope.php"], "unresolved::*.isHandling")
}

// Two anonymous classes in one file must not collapse into one node — their
// members' receivers have to stay distinguishable.
func TestPHPAnonymousClass_DistinctPerLine(t *testing.T) {
	res, err := NewPHPExtractor().Extract("scope.php", []byte(`<?php
$a = new class { public function run(): void {} };
$b = new class { public function run(): void {} };
`))
	require.NoError(t, err)
	names := map[string]bool{}
	for _, n := range res.Nodes {
		if n.Kind == graph.KindType {
			names[n.Name] = true
		}
	}
	assert.Len(t, names, 2, "each anonymous class needs its own identity, got %v", names)
}
