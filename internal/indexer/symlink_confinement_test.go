package indexer

import (
	"os"
	"path/filepath"
	"testing"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/excludes"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
)

// symlinkFixture builds a repo holding one ordinary file, one link that
// escapes the repo, and one link that stays inside it.
type symlinkFixture struct {
	repo     string
	ordinary string
	escaping string
	internal string
}

func newSymlinkFixture(t *testing.T) symlinkFixture {
	t.Helper()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("SECRET\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	repo := t.TempDir()
	ordinary := filepath.Join(repo, "main.go")
	if err := os.WriteFile(ordinary, []byte("package app\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repo, "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	escaping := filepath.Join(repo, "pwn.go")
	if err := os.Symlink(secret, escaping); err != nil {
		t.Fatal(err)
	}
	internal := filepath.Join(repo, "pkg", "linked.go")
	if err := os.Symlink(ordinary, internal); err != nil {
		t.Fatal(err)
	}
	return symlinkFixture{repo: repo, ordinary: ordinary, escaping: escaping, internal: internal}
}

func newSymlinkTestIndexer(t *testing.T, root string) *Indexer {
	t.Helper()
	reg := parser.NewRegistry()
	reg.Register(languages.NewGoExtractor())
	idx := New(graph.New(), reg, config.Default().Index, zap.NewNop())
	idx.storeRootPath(root)
	return idx
}

// TestShouldExcludeRefusesSymlinkOutOfRepo covers the cold-index admission
// gate. shouldExclude is the single predicate every walk and every explicit
// -path incremental site consults, so a link that escapes the repo must be
// refused there and nowhere else needs to know about symlinks.
func TestShouldExcludeRefusesSymlinkOutOfRepo(t *testing.T) {
	fx := newSymlinkFixture(t)
	idx := newSymlinkTestIndexer(t, fx.repo)

	if idx.shouldExclude(fx.escaping, fx.repo, false) != true {
		t.Fatal("a symlink whose target escapes the repo must be excluded")
	}
	if idx.shouldExclude(fx.ordinary, fx.repo, false) != false {
		t.Fatal("an ordinary in-repo file must stay indexed")
	}
	if idx.shouldExclude(fx.internal, fx.repo, false) != false {
		t.Fatal("a symlink that stays inside the repo must stay indexed")
	}
}

// TestShouldExcludeRefusesEscapingAgentConfigSymlink pins the ordering of
// the check. The agent-config branch deliberately re-includes MCP config
// files that the builtin excludes drop, so a symlink test placed after it
// would let `.claude/mcp.json -> ~/.aws/credentials` back in.
func TestShouldExcludeRefusesEscapingAgentConfigSymlink(t *testing.T) {
	outside := t.TempDir()
	secret := filepath.Join(outside, "credentials")
	if err := os.WriteFile(secret, []byte("SECRET\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(repo, ".claude", "mcp.json")
	if err := os.Symlink(secret, link); err != nil {
		t.Fatal(err)
	}

	idx := newSymlinkTestIndexer(t, repo)
	if !idx.shouldExclude(link, repo, false) {
		t.Fatal("an escaping symlink must be refused even on the re-included agent-config path")
	}
}

// TestWatcherIsExcludedRefusesSymlinkOutOfRepo covers the live-edit path.
// The watcher does not consult shouldExclude, so a fix applied only to the
// cold walk would hold at startup and regress the moment the link were
// created or touched under a running daemon.
func TestWatcherIsExcludedRefusesSymlinkOutOfRepo(t *testing.T) {
	fx := newSymlinkFixture(t)
	idx := newSymlinkTestIndexer(t, fx.repo)

	// Built directly rather than via NewWatcher: this asserts on the
	// admission predicate, and standing up real filesystem watching would
	// add goroutines and shutdown sequencing the assertion does not need.
	w := &Watcher{indexer: idx, excludes: excludes.New(excludes.Builtin)}

	if !w.isExcluded(fx.escaping) {
		t.Fatal("the watcher must refuse a symlink whose target escapes the repo")
	}
	if w.isExcluded(fx.ordinary) {
		t.Fatal("the watcher must keep watching ordinary in-repo files")
	}
	if w.isExcluded(fx.internal) {
		t.Fatal("the watcher must keep watching symlinks that stay inside the repo")
	}
}

// TestIndexSkipsSymlinkOutOfRepo is the end-to-end cold-index assertion:
// after a real index pass the escaping link is absent from the corpus that
// every content-serving tool reads, while legitimate files remain.
func TestIndexSkipsSymlinkOutOfRepo(t *testing.T) {
	fx := newSymlinkFixture(t)
	reg := parser.NewRegistry()
	reg.Register(languages.NewGoExtractor())
	idx := New(graph.New(), reg, config.Default().Index, zap.NewNop())
	if _, err := idx.Index(fx.repo); err != nil {
		t.Fatalf("Index: %v", err)
	}

	idx.mtimeMu.RLock()
	defer idx.mtimeMu.RUnlock()
	for rel := range idx.fileMtimes {
		if filepath.ToSlash(rel) == "pwn.go" {
			t.Fatal("the indexed corpus contains a symlink out of the repo")
		}
	}
	if _, ok := idx.fileMtimes["main.go"]; !ok {
		t.Fatalf("the ordinary file dropped out of the corpus: %v", idx.fileMtimes)
	}
}
