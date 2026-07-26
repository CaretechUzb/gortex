package indexer

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// TestIncrementalReindexPaths_EmptyPathsFallsBackToWholeRoot verifies
// that passing no paths makes IncrementalReindexPaths behave exactly
// like a whole-root IncrementalReindex: a changed file anywhere in the
// tree is picked up.
func TestIncrementalReindexPaths_EmptyPathsFallsBackToWholeRoot(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "pkg"), 0o755))
	writeFile(t, filepath.Join(dir, "main.go"), "package main\n\nfunc Main() {}\n")
	writeFile(t, filepath.Join(dir, "pkg", "util.go"), "package pkg\n\nfunc Util() {}\n")

	g := graph.New()
	idx := newTestIndexer(g)
	_, err := idx.Index(dir)
	require.NoError(t, err)

	bumpMtime(t, filepath.Join(dir, "pkg", "util.go"),
		"package pkg\n\nfunc Util() {}\n\nfunc UtilTwo() {}\n")

	res, err := idx.IncrementalReindexPaths(dir, nil)
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.Equal(t, 1, res.StaleFileCount,
		"empty paths must scan the whole root and pick up the changed file")
	assert.NotEmpty(t, g.FindNodesByName("UtilTwo"),
		"the whole-root pass must re-index the changed file")
}

// TestIncrementalReindexPaths_ScopesToDirectory checks that a directory
// path scopes the pass: a stale file inside the scoped directory is
// re-indexed, while a stale file outside it is left untouched.
func TestIncrementalReindexPaths_ScopesToDirectory(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "in"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "out"), 0o755))
	writeFile(t, filepath.Join(dir, "in", "a.go"), "package in\n\nfunc A() {}\n")
	writeFile(t, filepath.Join(dir, "out", "b.go"), "package out\n\nfunc B() {}\n")

	g := graph.New()
	idx := newTestIndexer(g)
	_, err := idx.Index(dir)
	require.NoError(t, err)

	// Make BOTH files stale, then scope the reindex to the "in" dir.
	bumpMtime(t, filepath.Join(dir, "in", "a.go"),
		"package in\n\nfunc A() {}\n\nfunc AScoped() {}\n")
	bumpMtime(t, filepath.Join(dir, "out", "b.go"),
		"package out\n\nfunc B() {}\n\nfunc BUnscoped() {}\n")

	res, err := idx.IncrementalReindexPaths(dir, []string{filepath.Join(dir, "in")})
	require.NoError(t, err)
	assert.Equal(t, 1, res.StaleFileCount,
		"only the file inside the scoped directory should be re-indexed")
	assert.NotEmpty(t, g.FindNodesByName("AScoped"),
		"the scoped file's new symbol must be in the graph")
	assert.Empty(t, g.FindNodesByName("BUnscoped"),
		"a file outside the scoped directory must NOT be re-indexed")
}

// TestIncrementalReindexPaths_RelativePathScope verifies that scoped
// paths may be supplied relative to the repository root, not just
// absolute.
func TestIncrementalReindexPaths_RelativePathScope(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "sub"), 0o755))
	writeFile(t, filepath.Join(dir, "sub", "c.go"), "package sub\n\nfunc C() {}\n")
	writeFile(t, filepath.Join(dir, "root.go"), "package main\n\nfunc Root() {}\n")

	g := graph.New()
	idx := newTestIndexer(g)
	_, err := idx.Index(dir)
	require.NoError(t, err)

	bumpMtime(t, filepath.Join(dir, "sub", "c.go"),
		"package sub\n\nfunc C() {}\n\nfunc CRel() {}\n")
	bumpMtime(t, filepath.Join(dir, "root.go"),
		"package main\n\nfunc Root() {}\n\nfunc RootRel() {}\n")

	// "sub" is repo-root-relative.
	res, err := idx.IncrementalReindexPaths(dir, []string{"sub"})
	require.NoError(t, err)
	assert.Equal(t, 1, res.StaleFileCount)
	assert.NotEmpty(t, g.FindNodesByName("CRel"))
	assert.Empty(t, g.FindNodesByName("RootRel"),
		"the root-level file is outside the scoped relative path")
}

// TestIncrementalReindexPaths_ScopesToSingleFile checks that a single
// file path scopes the pass to exactly that file.
func TestIncrementalReindexPaths_ScopesToSingleFile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "one.go"), "package main\n\nfunc One() {}\n")
	writeFile(t, filepath.Join(dir, "two.go"), "package main\n\nfunc Two() {}\n")

	g := graph.New()
	idx := newTestIndexer(g)
	_, err := idx.Index(dir)
	require.NoError(t, err)

	bumpMtime(t, filepath.Join(dir, "one.go"),
		"package main\n\nfunc One() {}\n\nfunc OneEdited() {}\n")
	bumpMtime(t, filepath.Join(dir, "two.go"),
		"package main\n\nfunc Two() {}\n\nfunc TwoEdited() {}\n")

	res, err := idx.IncrementalReindexPaths(dir, []string{filepath.Join(dir, "one.go")})
	require.NoError(t, err)
	assert.Equal(t, 1, res.StaleFileCount)
	assert.Equal(t, 2, res.FileCount,
		"FileCount describes the repo — both files — not the one-file scope; "+
			"the scope size is StaleFileCount")
	assert.NotEmpty(t, g.FindNodesByName("OneEdited"))
	assert.Empty(t, g.FindNodesByName("TwoEdited"),
		"the unscoped file must not be re-indexed")
}

// TestIncrementalReindexPaths_EvictsDeletedFileInScope verifies that a
// file deleted from disk under a scoped path is evicted, while a
// deletion outside the scope is left alone.
func TestIncrementalReindexPaths_EvictsDeletedFileInScope(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "in"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "out"), 0o755))
	inGone := filepath.Join(dir, "in", "gone.go")
	outGone := filepath.Join(dir, "out", "gone.go")
	writeFile(t, inGone, "package in\n\nfunc InGone() {}\n")
	writeFile(t, outGone, "package out\n\nfunc OutGone() {}\n")

	g := graph.New()
	idx := newTestIndexer(g)
	_, err := idx.Index(dir)
	require.NoError(t, err)
	require.NotEmpty(t, g.FindNodesByName("InGone"))
	require.NotEmpty(t, g.FindNodesByName("OutGone"))

	require.NoError(t, os.Remove(inGone))
	require.NoError(t, os.Remove(outGone))

	_, err = idx.IncrementalReindexPaths(dir, []string{filepath.Join(dir, "in")})
	require.NoError(t, err)

	assert.Empty(t, g.FindNodesByName("InGone"),
		"a file deleted under the scoped path must be evicted")
	assert.NotEmpty(t, g.FindNodesByName("OutGone"),
		"a deletion outside the scoped path must NOT be evicted by a scoped pass")
}

func TestIncrementalDiscoverPaths_PreservesDeletedTrackedFile(t *testing.T) {
	dir := t.TempDir()
	gone := filepath.Join(dir, "gone.go")
	writeFile(t, gone, "package main\n\nfunc Original() {}\n")

	g := graph.New()
	idx := newTestIndexer(g)
	_, err := idx.Index(dir)
	require.NoError(t, err)
	require.NotEmpty(t, g.FindNodesByName("Original"))

	require.NoError(t, os.Remove(gone))
	res, err := idx.incrementalDiscoverPaths(dir, nil)
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.Zero(t, res.DeletedFileCount)
	assert.NotEmpty(t, g.FindNodesByName("Original"),
		"directory discovery must leave deletion ownership to the file event")
}

// TestIncrementalReindexPaths_RejectsPathOutsideRoot verifies that a
// scoped path escaping the repository root is rejected — scoping is a
// narrowing operation, never an escape hatch.
func TestIncrementalReindexPaths_RejectsPathOutsideRoot(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	writeFile(t, filepath.Join(root, "main.go"), "package main\n\nfunc Main() {}\n")

	g := graph.New()
	idx := newTestIndexer(g)
	_, err := idx.Index(root)
	require.NoError(t, err)

	_, err = idx.IncrementalReindexPaths(root, []string{outside})
	require.Error(t, err, "a path outside the repo root must be rejected")
	assert.Contains(t, err.Error(), "outside repository root")
}

// TestIncrementalReindexPaths_ConvergesForScopedFile is the consistency
// invariant for the scoped path: editing a file and reconciling it with
// a file-scoped IncrementalReindexPaths must leave that file's symbols
// identical to a full index of the same disk state.
func TestIncrementalReindexPaths_ConvergesForScopedFile(t *testing.T) {
	build := func(dir string) {
		writeFile(t, filepath.Join(dir, "main.go"),
			"package main\n\nfunc main() { helper() }\n\nfunc helper() {}\n")
		writeFile(t, filepath.Join(dir, "other.go"),
			"package main\n\nfunc Other() {}\n")
	}

	dirA := t.TempDir()
	build(dirA)
	gA := graph.New()
	idxA := newTestIndexer(gA)
	_, err := idxA.Index(dirA)
	require.NoError(t, err)

	bumpMtime(t, filepath.Join(dirA, "main.go"),
		"package main\n\nfunc main() { helper(); helper() }\n\nfunc helper() {}\n")
	_, err = idxA.IncrementalReindexPaths(dirA, []string{filepath.Join(dirA, "main.go")})
	require.NoError(t, err)

	dirB := t.TempDir()
	build(dirB)
	writeFile(t, filepath.Join(dirB, "main.go"),
		"package main\n\nfunc main() { helper(); helper() }\n\nfunc helper() {}\n")
	gB := graph.New()
	idxB := newTestIndexer(gB)
	_, err = idxB.Index(dirB)
	require.NoError(t, err)

	// main.go's symbols must match between the scoped-incremental and
	// the full-index graphs.
	assert.Len(t, gA.FindNodesByName("main"), len(gB.FindNodesByName("main")))
	assert.Len(t, gA.FindNodesByName("helper"), len(gB.FindNodesByName("helper")))
	assert.NotEmpty(t, gA.FindNodesByName("Other"),
		"a scoped reindex must leave the untouched file's nodes intact")
}

// TestIncrementalReindexPathsReportsRepoFileCountNotScopeSize pins the
// documented contract on IndexResult.FileCount: it is how big the repo
// is, not how much work the pass did. A scoped pass walks only the
// caller's paths, so reporting the size of that walk here used to make
// `daemon status` show an actively-edited repo's file count as the size
// of its last changed-file batch.
func TestIncrementalReindexPathsReportsRepoFileCountNotScopeSize(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "pkg"), 0o755))
	const total = 10
	for i := range total {
		name := filepath.Join(dir, fmt.Sprintf("f%d.go", i))
		if i >= total/2 {
			name = filepath.Join(dir, "pkg", fmt.Sprintf("f%d.go", i))
		}
		writeFile(t, name, fmt.Sprintf("package main\n\nfunc F%d() {}\n", i))
	}

	g := graph.New()
	idx := newTestIndexer(g)
	full, err := idx.Index(dir)
	require.NoError(t, err)
	require.Equal(t, total, full.FileCount, "baseline: a full pass counts the repo")

	// Scope the pass to exactly one changed file.
	one := filepath.Join(dir, "f0.go")
	bumpMtime(t, one, "package main\n\nfunc F0() {}\n\nfunc F0Edited() {}\n")

	res, err := idx.IncrementalReindexPaths(dir, []string{one})
	require.NoError(t, err)
	assert.Equal(t, 1, res.StaleFileCount,
		"the pass's work size belongs to StaleFileCount")
	assert.Equal(t, total, res.FileCount,
		"FileCount must stay the repo's size under a one-file scope")

	// Same contract when the scope is a directory rather than one file.
	sub := filepath.Join(dir, "pkg", "f5.go")
	bumpMtime(t, sub, "package main\n\nfunc F5() {}\n\nfunc F5Edited() {}\n")

	res, err = idx.IncrementalReindexPaths(dir, []string{filepath.Join(dir, "pkg")})
	require.NoError(t, err)
	assert.Equal(t, total, res.FileCount,
		"FileCount must stay the repo's size under a directory scope")
}

// TestIncrementalReindexPathsFileCountTracksAddsAndDeletes verifies the
// repo-wide count still moves when the repo itself changes size, so the
// fix reports a live total rather than a frozen one.
func TestIncrementalReindexPathsFileCountTracksAddsAndDeletes(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.go"), "package main\n\nfunc A() {}\n")
	writeFile(t, filepath.Join(dir, "b.go"), "package main\n\nfunc B() {}\n")

	g := graph.New()
	idx := newTestIndexer(g)
	_, err := idx.Index(dir)
	require.NoError(t, err)

	added := filepath.Join(dir, "c.go")
	writeFile(t, added, "package main\n\nfunc C() {}\n")
	res, err := idx.IncrementalReindexPaths(dir, []string{added})
	require.NoError(t, err)
	assert.Equal(t, 3, res.FileCount, "a new file grows the repo count")

	require.NoError(t, os.Remove(added))
	res, err = idx.IncrementalReindexPaths(dir, []string{added})
	require.NoError(t, err)
	assert.Equal(t, 2, res.FileCount, "a deleted file shrinks the repo count")
}
