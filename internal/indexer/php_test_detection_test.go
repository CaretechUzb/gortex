package indexer

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
)

func TestTestRole_PHPUnitMethodNaming(t *testing.T) {
	assert.Equal(t, "test", TestRole("testHandlesEmptyRecord", "php"))
	// The prefix must be followed by an uppercase letter, so ordinary methods
	// that merely start with the letters are not swept in. A method named
	// exactly `test` is left out for the same reason Go leaves `Test` out; in
	// a test file it is classified by file membership anyway.
	assert.Empty(t, TestRole("test", "php"))
	assert.Empty(t, TestRole("testing", "php"))
	assert.Empty(t, TestRole("testable", "php"))
	assert.Empty(t, TestRole("setUp", "php"))
	assert.Empty(t, TestRole("handle", "php"))
}

func TestAnnotationTestRole_PHPUnitAttributes(t *testing.T) {
	assert.Equal(t, "test", AnnotationTestRole("php", "Test"))
	assert.Equal(t, "test", AnnotationTestRole("php", `PHPUnit\Framework\Attributes\Test`))
	assert.Equal(t, "test", AnnotationTestRole("php", "TestWith"))

	// Metadata attributes describe a test rather than declaring one.
	assert.Empty(t, AnnotationTestRole("php", "DataProvider"))
	assert.Empty(t, AnnotationTestRole("php", "TestDox"))
	assert.Empty(t, AnnotationTestRole("php", "CoversClass"))
}

func TestAnnotationTestRunner_PHP(t *testing.T) {
	assert.Equal(t, "phpunit", AnnotationTestRunner("php"))
}

// newPHPTestIndexer registers only the PHP extractor.
func newPHPTestIndexer(g graph.Store) *Indexer {
	reg := parser.NewRegistry()
	reg.Register(languages.NewPHPExtractor())
	cfg := config.Default().Index
	cfg.Workers = 2
	return New(g, reg, cfg, zap.NewNop())
}

func phpTestFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "tests"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "src"), 0o755))

	writeFile(t, filepath.Join(dir, "src", "Calculator.php"), `<?php
namespace App;

class Calculator {
    public function add(int $a, int $b): int { return $a + $b; }
}
`)
	writeFile(t, filepath.Join(dir, "tests", "CalculatorTest.php"), `<?php
namespace App\Tests;

use PHPUnit\Framework\TestCase;
use PHPUnit\Framework\Attributes\Test;

class CalculatorTest extends TestCase
{
    public function testAddsTwoNumbers(): void {}

    #[Test]
    public function itAddsNegatives(): void {}

    /**
     * @test
     */
    public function addsZero(): void {}

    public function helperNotATest(): void {}
}
`)
	return dir
}

func TestPHPTestDetection_EndToEnd(t *testing.T) {
	dir := phpTestFixture(t)
	g := graph.New()
	idx := newPHPTestIndexer(g)
	_, err := idx.Index(dir)
	require.NoError(t, err)

	byName := map[string]*graph.Node{}
	var testFile *graph.Node
	for _, n := range g.AllNodes() {
		if n.Kind == graph.KindMethod {
			byName[n.Name] = n
		}
		if n.Kind == graph.KindFile && filepath.Base(n.FilePath) == "CalculatorTest.php" {
			testFile = n
		}
	}

	require.NotNil(t, testFile, "no file node for CalculatorTest.php")
	assert.Equal(t, true, testFile.Meta["is_test_file"], "*Test.php is a test file")
	assert.Equal(t, "phpunit", testFile.Meta["test_runner"],
		"a file importing PHPUnit\\Framework runs under phpunit")

	for _, name := range []string{"testAddsTwoNumbers", "itAddsNegatives", "addsZero"} {
		n := byName[name]
		require.NotNil(t, n, "no method node for %s", name)
		assert.Equal(t, "test", n.Meta["test_role"], "%s must classify as a test: %v", name, n.Meta)
	}

	// A helper inside a test class is still swept in by file membership —
	// that is the existing cross-language contract, not a PHP quirk — but the
	// production class's method must not be.
	add := byName["add"]
	require.NotNil(t, add)
	assert.NotContains(t, add.Meta, "test_role", "a production method is not a test")
}

// A `#[Test]` method in a file whose name carries no Test suffix is still a
// test — the attribute is the signal, and it selects phpunit as the runner.
func TestPHPTestDetection_AttributeOutsideTestFile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "Suite.php"), `<?php
namespace App;

use PHPUnit\Framework\Attributes\Test;

class Suite
{
    #[Test]
    public function checksInvariant(): void {}

    public function ordinary(): void {}
}
`)
	g := graph.New()
	idx := newPHPTestIndexer(g)
	_, err := idx.Index(dir)
	require.NoError(t, err)

	var marked, plain *graph.Node
	for _, n := range g.AllNodes() {
		switch n.Name {
		case "checksInvariant":
			marked = n
		case "ordinary":
			plain = n
		}
	}
	require.NotNil(t, marked)
	assert.Equal(t, "test", marked.Meta["test_role"])
	assert.Equal(t, "phpunit", marked.Meta["test_runner"])

	require.NotNil(t, plain)
	assert.NotContains(t, plain.Meta, "test_role")
}

func TestPHPTestDetection_PestRunner(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "tests"), 0o755))
	writeFile(t, filepath.Join(dir, "tests", "ExampleTest.php"), `<?php
namespace Tests;

use Pest\TestSuite;

class ExampleTest {
    public function testSomething(): void {}
}
`)
	g := graph.New()
	idx := newPHPTestIndexer(g)
	_, err := idx.Index(dir)
	require.NoError(t, err)

	for _, n := range g.AllNodes() {
		if n.Kind == graph.KindFile && filepath.Base(n.FilePath) == "ExampleTest.php" {
			assert.Equal(t, "pest", n.Meta["test_runner"],
				"an imported Pest namespace picks pest over the phpunit default")
			return
		}
	}
	t.Fatal("no file node for ExampleTest.php")
}
