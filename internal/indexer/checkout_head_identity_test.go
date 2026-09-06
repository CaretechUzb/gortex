package indexer

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// foreignHeadFamily is the exact on-disk shape the 2026-09-04 incident had:
// a superproject whose working tree contains a nested repository, a linked
// worktree of that nested repository living beside it, and a commit that both
// repositories can resolve because one was cloned from the other.
//
// The shared commit is what makes the failure silent rather than loud. Without
// it a cross-repository diff would fail on a bad revision; with it the diff
// succeeds and describes somebody else's history as if it were this
// checkout's.
type foreignHeadFamily struct {
	superDir  string // enclosing repository (the "superproject")
	nestedDir string // repository the worktree belongs to (the "submodule")
	worktree  string // linked worktree of nestedDir, nested inside superDir
	sharedSHA string // commit both repositories can resolve; worktree HEAD
	superSHA  string // superproject HEAD — foreign to the worktree
}

func newForeignHeadFamily(t *testing.T) foreignHeadFamily {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available in PATH")
	}

	root := t.TempDir()
	superDir := filepath.Join(root, "super")
	require.NoError(t, os.MkdirAll(superDir, 0o755))
	runGit(t, superDir, "init", "-q", "-b", "main")
	runGit(t, superDir, "config", "user.email", "test@example.com")
	runGit(t, superDir, "config", "user.name", "Test")
	runGit(t, superDir, "config", "commit.gpgsign", "false")

	writeFile(t, filepath.Join(superDir, "shared.go"),
		"package main\n\nfunc SharedFunc() {}\n")
	runGit(t, superDir, "add", "shared.go")
	runGit(t, superDir, "commit", "-q", "-m", "shared base")
	sharedSHA := gitHead(t, superDir)

	// The nested repository is a clone, so it owns the shared commit object
	// too — the "submodule split out of its superproject" shape.
	nestedDir := filepath.Join(superDir, "src", "local")
	require.NoError(t, os.MkdirAll(filepath.Dir(nestedDir), 0o755))
	runGit(t, root, "clone", "-q", superDir, nestedDir)
	runGit(t, nestedDir, "config", "user.email", "test@example.com")
	runGit(t, nestedDir, "config", "user.name", "Test")
	runGit(t, nestedDir, "config", "commit.gpgsign", "false")

	worktree := filepath.Join(superDir, "src", "local.worktrees", "W1")
	runGit(t, nestedDir, "worktree", "add", "-q", "--detach", worktree, sharedSHA)
	require.Equal(t, sharedSHA, gitHead(t, worktree),
		"the linked worktree must start at the shared commit")

	// The superproject moves on. Its new commit touches the same path the
	// worktree has indexed, so a cross-repository diff produces a change set
	// that looks locally plausible.
	writeFile(t, filepath.Join(superDir, "shared.go"),
		"package main\n\nfunc SharedFunc() {}\n\nfunc SuperOnly() {}\n")
	runGit(t, superDir, "commit", "-q", "-am", "superproject advances")
	superSHA := gitHead(t, superDir)
	require.NotEqual(t, sharedSHA, superSHA)

	return foreignHeadFamily{
		superDir:  superDir,
		nestedDir: nestedDir,
		worktree:  worktree,
		sharedSHA: sharedSHA,
		superSHA:  superSHA,
	}
}

// TestRepoHeadAndDirty_RefusesForeignRepositoryHead covers the freshness and
// copy-source half of the same bug the git watcher hit: four separate call
// sites shell out for a checkout's HEAD, and a bare `git -C <root> rev-parse
// HEAD` answers with the ENCLOSING repository's commit once the checkout's
// `.git` link is unlinked — which every non-atomic worktree removal passes
// through.
//
// Nothing downstream can tell that commit apart from a real one. The freshness
// row would be stamped at another repository's HEAD, the poller would diff
// this checkout against a commit it never had, and the worktree-copy gate
// would key a subgraph duplication on a tree that does not exist here.
//
// All four probes are asserted together because they share one guard; without
// it every one of them returns the superproject's commit.
func TestRepoHeadAndDirty_RefusesForeignRepositoryHead(t *testing.T) {
	fam := newForeignHeadFamily(t)

	// The guard must not get in the way of the ordinary answer.
	head, _ := repoHeadAndDirty(fam.worktree)
	require.Equal(t, fam.sharedSHA, head, "an intact checkout must still report its own HEAD")
	require.Equal(t, fam.sharedSHA, repoHead(fam.worktree))
	require.Equal(t, fam.sharedSHA, gitHeadSHA(fam.worktree))

	// The removal window: the `.git` link is unlinked while the root itself is
	// still on disk.
	require.NoError(t, os.Remove(filepath.Join(fam.worktree, ".git")))
	require.Equal(t, fam.superSHA, gitHead(t, fam.worktree),
		"premise: a de-linked checkout root resolves upward to the enclosing repository")

	head, dirty := repoHeadAndDirty(fam.worktree)
	assert.NotEqual(t, fam.superSHA, head,
		"the freshness probe must never report the enclosing repository's HEAD")
	assert.Empty(t, head,
		"a checkout that cannot prove its own identity has no commit to stamp")
	assert.False(t, dirty, "a refused probe reports no dirty bit either")

	assert.Empty(t, repoHead(fam.worktree),
		"the cheap freshness probe must refuse identically")
	assert.Empty(t, gitHeadSHA(fam.worktree),
		"the copy-source probe must refuse identically")

	pollerSHA, err := pollerHeadSHA(fam.worktree)
	assert.Error(t, err, "the poller must see a refusal, not a plausible commit")
	assert.Empty(t, pollerSHA,
		"the poller must never adopt the enclosing repository's HEAD as its baseline")
}

// TestCheckoutHeadIdentity_RefusesRelinkedGitDir covers the branch the two
// removal tests cannot reach. Both of those delete `.git`, so they stop at
// errCheckoutGitLinkGone and the identity comparison itself never runs.
//
// This is the case where the checkout is intact and still a git repository —
// it simply is no longer the SAME one. A `.git` re-pointed after admission is
// not exotic: `git worktree move`, a re-created worktree at a path a previous
// one used, or a plain `git init` in a directory the daemon already watches
// all produce it, and none of them changes anything the cheaper checks look
// at.
func TestCheckoutHeadIdentity_RefusesRelinkedGitDir(t *testing.T) {
	fam := newForeignHeadFamily(t)

	// The family this checkout was admitted under, captured while it was still
	// true — exactly what GitWatcher.Start records in commonDir.
	admitted := filepath.Join(fam.nestedDir, ".git")
	sha, err := checkoutHeadSHA(testCtx(), fam.worktree, admitted)
	require.NoError(t, err, "the intact checkout must pass its own baseline")
	require.Equal(t, fam.sharedSHA, sha)

	// Re-point the checkout at a different repository, leaving a perfectly
	// valid `.git` behind: the root is present, the link resolves, git answers
	// without error. Only the identity moved.
	require.NoError(t, os.Remove(filepath.Join(fam.worktree, ".git")))
	runGit(t, fam.worktree, "init", "-q", "-b", "main")
	runGit(t, fam.worktree, "config", "user.email", "test@example.com")
	runGit(t, fam.worktree, "config", "user.name", "Test")
	runGit(t, fam.worktree, "config", "commit.gpgsign", "false")
	runGit(t, fam.worktree, "add", "-A")
	runGit(t, fam.worktree, "commit", "-q", "-m", "a different repository entirely")
	stranger := gitHead(t, fam.worktree)
	require.NotEqual(t, fam.sharedSHA, stranger)

	sha, err = checkoutHeadSHA(testCtx(), fam.worktree, admitted)
	require.Error(t, err)
	assert.ErrorIs(t, err, errForeignGitIdentity,
		"a checkout re-pointed at another repository must refuse as a foreign identity")
	assert.Equal(t, "foreign_repository", gitIdentityRefusal(err),
		"the refusal label is what the log line carries; it must name this case")
	assert.Empty(t, sha, "a refused identity yields no commit")

	// Without a baseline the same call resolves the checkout's own (new) link
	// and allows it — the derived baseline can only speak for what is on disk
	// now, which is why an admitted baseline is the stronger one.
	derived, err := checkoutHeadSHA(testCtx(), fam.worktree, "")
	require.NoError(t, err)
	assert.Equal(t, stranger, derived,
		"a baseline-less caller verifies against the link the checkout actually has")
}

// TestCheckoutHeadIdentity_SameGitPathThroughSymlink pins the comparison's
// symlink half. It is not decoration: git answers with the physical path
// getcwd() hands it, while a baseline taken from a caller-supplied root keeps
// whatever symlinks that root was written with — /tmp -> /private/tmp on
// Darwin, and every t.TempDir() beneath it. A plain string compare would
// refuse every macOS temp-dir checkout, which is most of this package's tests.
func TestCheckoutHeadIdentity_SameGitPathThroughSymlink(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real")
	require.NoError(t, os.MkdirAll(filepath.Join(real, ".git"), 0o755))
	link := filepath.Join(root, "link")
	require.NoError(t, os.Symlink(real, link))

	assert.True(t, sameGitPath(filepath.Join(link, ".git"), filepath.Join(real, ".git")),
		"the same directory reached through a symlink is the same directory")
	assert.True(t, sameGitPath(filepath.Join(real, ".git"), filepath.Join(real, ".git")),
		"identical paths need no resolution at all")

	other := filepath.Join(root, "other")
	require.NoError(t, os.MkdirAll(filepath.Join(other, ".git"), 0o755))
	assert.False(t, sameGitPath(filepath.Join(link, ".git"), filepath.Join(other, ".git")),
		"resolving symlinks must not make two different directories equal")
	assert.False(t, sameGitPath(filepath.Join(root, "gone", ".git"), filepath.Join(real, ".git")),
		"an unresolvable path falls back to its lexical form, which still names a different directory")
	assert.False(t, sameGitPath("", filepath.Join(real, ".git")),
		"an empty baseline can never match")

	// The comparison is pathkey.EqualPaths, not string equality, so the two
	// spellings a Unicode-normalising filesystem hands back for ONE directory
	// compare equal. macOS returns decomposed (NFD) names for paths created
	// with composed (NFC) ones; a string compare read those as two different
	// repositories and refused a checkout that was its own. Asserted with a
	// literal NFD/NFC pair rather than by creating the directory, so it pins
	// the fold on every platform instead of only on the ones that decompose.
	assert.True(t, sameGitPath(
		filepath.Join(root, "\u00e9cole", ".git"),
		filepath.Join(root, "e\u0301cole", ".git")),
		"one directory spelled NFC and NFD is one directory")

	// End to end: a real checkout reached through a symlinked root. git reports
	// the physical common dir, the caller's derived baseline carries the
	// symlinked one, and the guard must still admit it.
	repo := filepath.Join(root, "repo")
	require.NoError(t, os.MkdirAll(repo, 0o755))
	runGit(t, repo, "init", "-q", "-b", "main")
	runGit(t, repo, "config", "user.email", "test@example.com")
	runGit(t, repo, "config", "user.name", "Test")
	runGit(t, repo, "config", "commit.gpgsign", "false")
	writeFile(t, filepath.Join(repo, "a.go"), "package main\n\nfunc Alpha() {}\n")
	runGit(t, repo, "add", ".")
	runGit(t, repo, "commit", "-q", "-m", "first")

	repoLink := filepath.Join(root, "repo-link")
	require.NoError(t, os.Symlink(repo, repoLink))
	sha, err := checkoutHeadSHA(testCtx(), repoLink, "")
	require.NoError(t, err, "a checkout reached through a symlink must not read as foreign")
	assert.Equal(t, gitHead(t, repo), sha)
}
