package churn

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph"
)

func TestEnrichGraph_StampsSymbolAndFile(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	repoDir := initRepo(t)
	writeAndCommit(t, repoDir, "main.go", "package main\n\nfunc Hello() {}\n", "initial")
	// Touch the file twice more so churn_rate is non-trivial.
	writeAndCommit(t, repoDir, "main.go", "package main\n\nfunc Hello() { _ = 1 }\n", "second")
	writeAndCommit(t, repoDir, "main.go", "package main\n\nfunc Hello() { _ = 2 }\n", "third")

	g := graph.New()
	g.AddNode(&graph.Node{
		ID: "main.go", Kind: graph.KindFile, Name: "main.go", FilePath: "main.go",
	})
	g.AddNode(&graph.Node{
		ID:        "main.go::Hello",
		Kind:      graph.KindFunction,
		Name:      "Hello",
		FilePath:  "main.go",
		StartLine: 3, EndLine: 3,
	})

	res, err := EnrichGraph(context.Background(), g, repoDir, Options{
		Branch: currentBranch(t, repoDir),
		Now:    time.Now(),
	})
	if err != nil {
		t.Fatalf("enrich: %v", err)
	}
	if res.Files != 1 || res.Symbols != 1 {
		t.Errorf("res = %+v, want Files=1 Symbols=1", res)
	}
	if res.HeadSHA == "" {
		t.Error("HeadSHA should be set")
	}

	// Churn now persists in the typed sidecar (change A), not Node.Meta.
	byID := map[string]graph.ChurnEnrichment{}
	for _, e := range g.ChurnRows("") {
		byID[e.NodeID] = e
	}

	fileChurn, ok := byID["main.go"]
	if !ok {
		t.Fatalf("file churn row missing from sidecar; rows=%+v", byID)
	}
	if fileChurn.CommitCount != 3 {
		t.Errorf("file commit_count = %d, want 3", fileChurn.CommitCount)
	}
	if fileChurn.ChurnRate == 0 {
		t.Errorf("file churn_rate missing")
	}
	if fileChurn.HeadSHA == "" || fileChurn.Branch == "" {
		t.Errorf("file churn provenance (head_sha/branch) missing: %+v", fileChurn)
	}
	// Meta must NOT carry churn anymore — it moved to the sidecar.
	if _, present := g.GetNode("main.go").Meta["churn"]; present {
		t.Errorf("churn must not remain in Node.Meta after sidecar migration")
	}

	symChurn, ok := byID["main.go::Hello"]
	if !ok {
		t.Fatalf("symbol churn row missing from sidecar")
	}
	if symChurn.CommitCount < 1 {
		t.Errorf("symbol commit_count = %d, want >= 1", symChurn.CommitCount)
	}
	if symChurn.LastAuthor == "" {
		t.Errorf("symbol last_author missing: %+v", symChurn)
	}
}

func TestEnrichGraph_SkipsFilesWithNoHistory(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	repoDir := initRepo(t)
	writeAndCommit(t, repoDir, "main.go", "package main\n\nfunc Hello() {}\n", "initial")

	g := graph.New()
	// Refer to a file that exists on disk but isn't tracked by git.
	if err := os.WriteFile(filepath.Join(repoDir, "untracked.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	g.AddNode(&graph.Node{ID: "untracked.go", Kind: graph.KindFile, FilePath: "untracked.go"})

	res, err := EnrichGraph(context.Background(), g, repoDir, Options{
		Branch: currentBranch(t, repoDir),
	})
	if err != nil {
		t.Fatalf("enrich: %v", err)
	}
	if res.Files != 0 || res.Symbols != 0 {
		t.Errorf("untracked file should yield no stamps, got %+v", res)
	}
}

func TestEnrichGraph_RequiresBranch(t *testing.T) {
	g := graph.New()
	_, err := EnrichGraph(context.Background(), g, "/tmp/anywhere", Options{})
	if err == nil {
		t.Fatal("expected error when Branch is empty")
	}
	if !strings.Contains(err.Error(), "Branch is required") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestEnrichGraph_RejectsUnresolvableBranch(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repoDir := initRepo(t)
	writeAndCommit(t, repoDir, "main.go", "package main\n", "initial")

	g := graph.New()
	_, err := EnrichGraph(context.Background(), g, repoDir, Options{Branch: "does-not-exist"})
	if err == nil {
		t.Fatal("expected error when branch does not resolve")
	}
}

func TestRoundTwo(t *testing.T) {
	cases := []struct {
		in   float64
		want float64
	}{
		{0.0, 0.0},
		{0.125, 0.13},
		{1.0 / 3.0, 0.33},
		{99.999, 100.0},
	}
	for _, c := range cases {
		if got := roundTwo(c.in); got != c.want {
			t.Errorf("roundTwo(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

// --- helpers ---

func initRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "Tester"},
		{"config", "commit.gpgsign", "false"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return dir
}

func writeAndCommit(t *testing.T, dir, rel, body, msg string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, rel), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	add := exec.Command("git", "add", rel)
	add.Dir = dir
	if out, err := add.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v\n%s", err, out)
	}
	commit := exec.Command("git", "commit", "-q", "-m", msg)
	commit.Dir = dir
	commit.Env = append(commit.Environ(),
		"GIT_AUTHOR_NAME=Tester", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Tester", "GIT_COMMITTER_EMAIL=test@example.com")
	if out, err := commit.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v\n%s", err, out)
	}
}

func currentBranch(t *testing.T, dir string) string {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("rev-parse: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// TestStripRepoPrefix_NeedsNoExternalBinaries pins the fix for the
// subprocess fan-out: resolving whether a path exists is a filesystem
// question, and answering it must not depend on anything being on PATH.
// The previous implementation gated on `exec.LookPath("git")` and then
// shelled out to `test -f` once per indexed file, so on a host where
// neither resolves — every Windows box, since there is no `test`
// executable — the prefix was never stripped and the enricher silently
// produced no churn data.
func TestStripRepoPrefix_NeedsNoExternalBinaries(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "internal"), 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "internal", "foo.go")
	if err := os.WriteFile(target, []byte("package internal\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Nothing is resolvable on PATH — the old implementation bailed here.
	t.Setenv("PATH", "")

	if got := stripRepoPrefix("myrepo/internal/foo.go", root); got != "internal/foo.go" {
		t.Errorf("prefixed path: got %q, want %q", got, "internal/foo.go")
	}
	if got := stripRepoPrefix("internal/foo.go", root); got != "internal/foo.go" {
		t.Errorf("repo-relative path: got %q, want %q", got, "internal/foo.go")
	}
	// A path that resolves under neither spelling is returned untouched.
	if got := stripRepoPrefix("myrepo/internal/absent.go", root); got != "myrepo/internal/absent.go" {
		t.Errorf("unresolvable path: got %q, want it unchanged", got)
	}
}

// TestFileExists_MatchesTestF keeps the os.Stat replacement honest about
// the `test -f` semantics it replaced: regular files only, symlinks
// followed, directories and missing paths rejected.
func TestFileExists_MatchesTestF(t *testing.T) {
	dir := t.TempDir()
	regular := filepath.Join(dir, "regular")
	if err := os.WriteFile(regular, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !fileExists(regular) {
		t.Error("regular file reported missing")
	}
	if fileExists(dir) {
		t.Error("directory reported as a regular file")
	}
	if fileExists(filepath.Join(dir, "missing")) {
		t.Error("missing path reported as a regular file")
	}

	link := filepath.Join(dir, "regular-link")
	if err := os.Symlink(regular, link); err == nil && !fileExists(link) {
		t.Error("symlink to a regular file reported missing")
	}
}
