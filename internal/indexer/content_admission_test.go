package indexer

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
)

// newAssetTestIndexer registers the code + asset extractors the
// content-admission gate keys off, so a temp repo of documents / data
// files exercises the real walk-time gate.
// newAssetTestRegistry registers the code + asset extractors the content
// admission gates classify against.
func newAssetTestRegistry() *parser.Registry {
	reg := parser.NewRegistry()
	reg.Register(languages.NewGoExtractor())
	reg.Register(languages.NewTextExtractor())
	reg.Register(languages.NewPDFExtractor())
	reg.Register(languages.NewPptxExtractor())
	reg.Register(languages.NewXlsxExtractor())
	reg.Register(languages.NewDataAssetExtractor())
	reg.Register(languages.NewImageAssetExtractor())
	return reg
}

func newAssetTestIndexer(g graph.Store) *Indexer {
	cfg := config.Default().Index
	cfg.Workers = 2
	return New(g, newAssetTestRegistry(), cfg, zap.NewNop())
}

func TestContentAdmissionConfig_EffectiveCaps(t *testing.T) {
	// 0 (absent) → built-in default, capped.
	lim, capped := config.ContentAdmissionConfig{MaxDocumentBytes: 0}.EffectiveMaxDocumentBytes()
	require.True(t, capped)
	require.Equal(t, int64(10<<20), lim)

	// >0 → explicit cap.
	lim, capped = config.ContentAdmissionConfig{MaxDocumentBytes: 4096}.EffectiveMaxDocumentBytes()
	require.True(t, capped)
	require.Equal(t, int64(4096), lim)

	// <0 → no cap.
	_, capped = config.ContentAdmissionConfig{MaxDocumentBytes: -1}.EffectiveMaxDocumentBytes()
	require.False(t, capped)

	// Same tri-state for data.
	_, capped = config.ContentAdmissionConfig{MaxDataBytes: -1}.EffectiveMaxDataBytes()
	require.False(t, capped)
	lim, capped = config.ContentAdmissionConfig{MaxDataBytes: 7}.EffectiveMaxDataBytes()
	require.True(t, capped)
	require.Equal(t, int64(7), lim)

	// Same tri-state for images, which were the only asset class with no cap.
	lim, capped = config.ContentAdmissionConfig{MaxImageBytes: 0}.EffectiveMaxImageBytes()
	require.True(t, capped)
	require.Equal(t, int64(32<<20), lim)
	lim, capped = config.ContentAdmissionConfig{MaxImageBytes: 9}.EffectiveMaxImageBytes()
	require.True(t, capped)
	require.Equal(t, int64(9), lim)
	_, capped = config.ContentAdmissionConfig{MaxImageBytes: -1}.EffectiveMaxImageBytes()
	require.False(t, capped)
}

func TestContentAdmissionGate_Skip(t *testing.T) {
	idx := newAssetTestIndexer(graph.New())

	// Default policy: documents capped at 10 MiB, data skipped.
	idx.config.Content = config.Default().Index.Content
	gate := idx.newContentAdmissionGate()
	require.NotNil(t, gate)

	// Code is never gated.
	_, skip := gate.skip("go", 1<<30)
	require.False(t, skip)

	// A small document is admitted; a huge one is dropped.
	_, skip = gate.skip("text", 1024)
	require.False(t, skip)
	reason, skip := gate.skip("text", 11<<20)
	require.True(t, skip)
	require.Equal(t, skipReasonLargeDocument, reason)

	// Data assets are skipped by default at any size.
	reason, skip = gate.skip("data", 1)
	require.True(t, skip)
	require.Equal(t, skipReasonVectorData, reason)

	// Opting data in admits it, subject to its own cap.
	idx.config.Content.IndexData = true
	idx.config.Content.MaxDataBytes = 1024
	gate = idx.newContentAdmissionGate()
	_, skip = gate.skip("data", 512)
	require.False(t, skip)
	reason, skip = gate.skip("data", 4096)
	require.True(t, skip)
	require.Equal(t, skipReasonLargeData, reason)

	// Images stay admitted by default — a diagram is a first-class node —
	// but the class now carries a ceiling, because minting that node reads
	// and hashes the whole file.
	idx.config.Content = config.Default().Index.Content
	gate = idx.newContentAdmissionGate()
	_, skip = gate.skip("image", 2<<20)
	require.False(t, skip)
	reason, skip = gate.skip("image", 33<<20)
	require.True(t, skip)
	require.Equal(t, skipReasonLargeImage, reason)

	// A negative image cap disables image gating entirely.
	idx.config.Content = config.ContentAdmissionConfig{MaxImageBytes: -1}
	gate = idx.newContentAdmissionGate()
	_, skip = gate.skip("image", 1<<30)
	require.False(t, skip)

	// A negative document cap disables document gating entirely.
	idx.config.Content = config.ContentAdmissionConfig{MaxDocumentBytes: -1}
	gate = idx.newContentAdmissionGate()
	_, skip = gate.skip("text", 1<<30)
	require.False(t, skip)
}

// TestContentAdmissionGate_NilInert verifies a nil gate (no asset extractors
// registered) never skips — the all-code-repo fast path.
func TestContentAdmissionGate_NilInert(t *testing.T) {
	var g *contentAdmissionGate
	_, skip := g.skip("text", 1<<30)
	require.False(t, skip)
}

func TestContentSkipNode(t *testing.T) {
	n := contentSkipNode(skippedFile{
		relPath: "data/deck.pptx", lang: "pptx", size: 50 << 20, reason: skipReasonLargeDocument,
	})
	require.Equal(t, graph.KindFile, n.Kind)
	require.Equal(t, "data/deck.pptx", n.ID)
	require.Equal(t, "deck.pptx", n.Name)
	require.Equal(t, "pptx", n.Language)
	require.Equal(t, skipReasonLargeDocument, n.Meta["skip_reason"])
	require.Equal(t, true, n.Meta["skipped_due_to_content"])
	require.Equal(t, int64(50<<20), n.Meta["file_size_bytes"])
}

// TestIndex_DocumentCapSkip verifies a document over the cap becomes a
// large_document skip node while a small one is still ingested as content.
func TestIndex_DocumentCapSkip(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "small.txt"), "a short note")
	writeFile(t, filepath.Join(dir, "big.txt"), strings.Repeat("filler text ", 400)) // ~4.8 KiB
	writeFile(t, filepath.Join(dir, "code.go"), "package main\n\nfunc A() {}\n")

	g := graph.New()
	idx := newAssetTestIndexer(g)
	idx.config.Content.MaxDocumentBytes = 1024 // big.txt is over the cap

	result, err := idx.Index(dir)
	require.NoError(t, err)
	require.Equal(t, 1, result.SkippedFiles)

	big := g.GetNode("big.txt")
	require.NotNil(t, big, "an over-cap document must still leave a skip node")
	require.Equal(t, skipReasonLargeDocument, big.Meta["skip_reason"])
	require.Equal(t, true, big.Meta["skipped_due_to_content"])

	// The small document was admitted (no skip reason).
	small := g.GetNode("small.txt")
	require.NotNil(t, small)
	require.Nil(t, small.Meta["skip_reason"])
}

// TestIndex_DataAssetSkippedByDefault verifies a vector/data artifact is
// dropped by default and admitted as a metadata node when opted in.
func TestIndex_DataAssetSkippedByDefault(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "embeddings.npy"), "NUMPY-array-placeholder-bytes")

	// Default: skipped with vector_data.
	g := graph.New()
	idx := newAssetTestIndexer(g)
	result, err := idx.Index(dir)
	require.NoError(t, err)
	require.Equal(t, 1, result.SkippedFiles)
	n := g.GetNode("embeddings.npy")
	require.NotNil(t, n)
	require.Equal(t, skipReasonVectorData, n.Meta["skip_reason"])

	// Opt in: admitted as a data metadata node, no skip reason.
	g2 := graph.New()
	idx2 := newAssetTestIndexer(g2)
	idx2.config.Content.IndexData = true
	_, err = idx2.Index(dir)
	require.NoError(t, err)
	n2 := g2.GetNode("embeddings.npy")
	require.NotNil(t, n2)
	require.Nil(t, n2.Meta["skip_reason"])
	require.Equal(t, "data", n2.Meta["data_class"])
}

// TestIndex_SkipUntrackedAssets verifies that, with the opt-in flag on,
// untracked document/data assets are dropped while tracked assets and
// untracked CODE are still indexed; and that the default (flag off) admits
// untracked documents.
func TestIndex_SkipUntrackedAssets(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available in PATH")
	}
	dir := t.TempDir()
	runGit(t, dir, "init", "-q", "-b", "main")
	runGit(t, dir, "config", "user.email", "t@example.com")
	runGit(t, dir, "config", "user.name", "T")
	runGit(t, dir, "config", "commit.gpgsign", "false")

	// Tracked: a code file and a small document.
	writeFile(t, filepath.Join(dir, "code.go"), "package main\n\nfunc A() {}\n")
	writeFile(t, filepath.Join(dir, "tracked.txt"), "committed note")
	runGit(t, dir, "add", ".")
	runGit(t, dir, "commit", "-q", "-m", "init")

	// Untracked working-tree files: a document, a data asset, and code.
	writeFile(t, filepath.Join(dir, "untracked.txt"), "scratch rag asset")
	writeFile(t, filepath.Join(dir, "vectors.npy"), "NUMPY-placeholder-bytes")
	writeFile(t, filepath.Join(dir, "new.go"), "package main\n\nfunc B() {}\n")

	// Flag ON: untracked assets dropped, tracked asset + untracked code kept.
	g := graph.New()
	idx := newAssetTestIndexer(g)
	idx.config.SkipUntrackedAssets = true
	if _, err := idx.IndexCtx(testCtx(), dir); err != nil {
		t.Fatalf("index: %v", err)
	}
	for _, p := range []string{"untracked.txt", "vectors.npy"} {
		n := g.GetNode(p)
		require.NotNil(t, n, p)
		require.Equal(t, skipReasonUntrackedAsset, n.Meta["skip_reason"], p)
	}
	tracked := g.GetNode("tracked.txt")
	require.NotNil(t, tracked)
	require.Nil(t, tracked.Meta["skip_reason"], "tracked document must be admitted")
	newGo := g.GetNode("new.go")
	require.NotNil(t, newGo, "untracked CODE must still be indexed")
	require.Nil(t, newGo.Meta["skip_reason"])

	// Flag OFF (default): the untracked document is admitted as content.
	g2 := graph.New()
	idx2 := newAssetTestIndexer(g2)
	if _, err := idx2.IndexCtx(testCtx(), dir); err != nil {
		t.Fatalf("index: %v", err)
	}
	un := g2.GetNode("untracked.txt")
	require.NotNil(t, un)
	require.Nil(t, un.Meta["skip_reason"], "with the flag off, an untracked document is admitted")
}

// bucketByKey finds an intake bucket by its extension key.
func bucketByKey(buckets []IntakeBucket, key string) (IntakeBucket, bool) {
	for _, b := range buckets {
		if b.Key == key {
			return b, true
		}
	}
	return IntakeBucket{}, false
}

// TestDryRunIntake_ReflectsContentGate verifies the dry-run manifest reports
// oversized documents and data assets as skipped with the same reasons the
// real index walk uses — so the preflight matches the indexer (#120).
func TestDryRunIntake_ReflectsContentGate(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "code.go"), "package main\n\nfunc A() {}\n")
	writeFile(t, filepath.Join(dir, "small.txt"), "short")
	writeFile(t, filepath.Join(dir, "big.txt"), strings.Repeat("filler text ", 400))
	writeFile(t, filepath.Join(dir, "vectors.npy"), "NUMPY-placeholder-bytes")

	cfg := config.Default().Index
	cfg.Content.MaxDocumentBytes = 1024

	m, err := DryRunIntake(testCtx(), dir, cfg, newAssetTestRegistry(), zap.NewNop())
	require.NoError(t, err)

	txt, ok := bucketByKey(m.TopSkippedExtensionsByBytes, "txt")
	require.True(t, ok, "oversized document must be reported skipped")
	require.Equal(t, skipReasonLargeDocument, txt.Reason)
	npy, ok := bucketByKey(m.TopSkippedExtensionsByBytes, "npy")
	require.True(t, ok, "data asset must be reported skipped")
	require.Equal(t, skipReasonVectorData, npy.Reason)

	// Code and the small document stay admitted.
	_, ok = bucketByKey(m.TopAdmittedExtensionsByBytes, "go")
	require.True(t, ok)
	smallTxt, ok := bucketByKey(m.TopAdmittedExtensionsByBytes, "txt")
	require.True(t, ok)
	require.Equal(t, 1, smallTxt.Files, "only the small document is admitted")
}

// TestDryRunIntake_ReflectsUntrackedGate verifies the dry-run manifest applies
// the opt-in untracked-asset gate too.
func TestDryRunIntake_ReflectsUntrackedGate(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available in PATH")
	}
	dir := t.TempDir()
	runGit(t, dir, "init", "-q", "-b", "main")
	runGit(t, dir, "config", "user.email", "t@example.com")
	runGit(t, dir, "config", "user.name", "T")
	runGit(t, dir, "config", "commit.gpgsign", "false")
	writeFile(t, filepath.Join(dir, "code.go"), "package main\n\nfunc A() {}\n")
	runGit(t, dir, "add", ".")
	runGit(t, dir, "commit", "-q", "-m", "init")
	writeFile(t, filepath.Join(dir, "untracked.txt"), "scratch asset")

	cfg := config.Default().Index
	cfg.SkipUntrackedAssets = true

	m, err := DryRunIntake(testCtx(), dir, cfg, newAssetTestRegistry(), zap.NewNop())
	require.NoError(t, err)
	txt, ok := bucketByKey(m.TopSkippedExtensionsByBytes, "txt")
	require.True(t, ok)
	require.Equal(t, skipReasonUntrackedAsset, txt.Reason)
}

// TestGitTrackedSet_NonGitDirInert verifies gitTrackedSet reports failure
// (gate inert) outside a git repo.
func TestGitTrackedSet_NonGitDirInert(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available in PATH")
	}
	_, ok := gitTrackedSet(testCtx(), t.TempDir())
	require.False(t, ok)
}

// TestIndexFile_ContentSkip verifies the incremental (watcher) path applies
// the same gate, so a document the cold walk would skip doesn't get parsed
// back in on re-index.
func TestIndexFile_ContentSkip(t *testing.T) {
	dir := t.TempDir()
	g := graph.New()
	idx := newAssetTestIndexer(g)
	idx.config.Content.MaxDocumentBytes = 1024
	if _, err := idx.Index(dir); err != nil {
		t.Fatalf("index: %v", err)
	}

	big := filepath.Join(dir, "report.txt")
	writeFile(t, big, strings.Repeat("report line ", 400))
	require.NoError(t, idx.indexFile(big, false))

	n := g.GetNode("report.txt")
	require.NotNil(t, n)
	require.Equal(t, skipReasonLargeDocument, n.Meta["skip_reason"])
}
