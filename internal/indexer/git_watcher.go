package indexer

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/zzet/gortex/internal/gitcmd"
	"github.com/zzet/gortex/internal/graph"
	"go.uber.org/zap"
)

// GitWatcher observes `.git/HEAD` (and the target ref) for a single
// tracked repository. When the resolved commit moves, it computes the
// diff between the old and new commits and dispatches EvictFile /
// IndexFileNoResolve per changed path, running the resolver once at
// the end. This is the branch-switch / rebase / reset path: the
// regular fsnotify per-file watcher sees 500 Remove+Create events for
// a checkout and walks each through per-file resolve + search, which
// is measurably slow and incorrect for renames (git sees them as
// atomic; fsnotify sees unrelated remove+create pairs).
//
// Gitwatcher complements the file watcher — it does not replace it.
// File edits outside git operations (save buffer, `git stash`,
// `git reset --mixed` leaving the working tree dirty) still fire
// normal fsnotify events and flow through the per-file path.
type GitWatcher struct {
	repoPath string
	// indexer is the construction-time stable repository-lane carrier.
	// MultiWatcher installs currentIndexer so the freshness tail resolves the
	// currently registered Indexer only after that lane admits it.
	indexer        *Indexer
	currentIndexer func() *Indexer
	logger         *zap.Logger
	fsw            *fsnotify.Watcher
	debounce       time.Duration
	done           chan struct{}
	stopped        chan struct{}
	mu             sync.Mutex
	lastSHA        string
	fireTimer      *time.Timer
	loopStarted    bool
	stopCalled     bool
	// reconciling single-flights reconcile: a ref event landing while
	// one is in flight sets rerun instead of spawning a second
	// concurrent reconcile from the same stale base (each AfterFunc
	// fires on its own goroutine, and lastSHA only advances at the
	// END of a reconcile — overlapping runs would diff and apply the
	// same range twice and race applyChanges/ResolveAll).
	reconciling bool
	rerun       bool
	// drained is a test hook that fires after a reconcile completes
	// with the number of files patched. nil in production.
	drained      func(int)
	batchReindex watcherBatchReindex // MultiWatcher installs shared resolver + derived catch-up
}

// NewGitWatcher creates a watcher for repoPath/.git/HEAD. repoPath is
// the absolute path to the worktree root; the .git dir is discovered
// by looking at the HEAD file (handles worktrees, submodules via the
// `gitdir:` indirection).
func NewGitWatcher(repoPath string, idx *Indexer, logger *zap.Logger) (*GitWatcher, error) {
	absRepo, err := filepath.Abs(repoPath)
	if err != nil {
		return nil, err
	}
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	return &GitWatcher{
		repoPath: absRepo,
		indexer:  idx,
		logger:   logger,
		fsw:      fsw,
		debounce: 300 * time.Millisecond,
		done:     make(chan struct{}),
		stopped:  make(chan struct{}),
	}, nil
}

func (gw *GitWatcher) registeredIndexer() *Indexer {
	if gw.currentIndexer != nil {
		return gw.currentIndexer()
	}
	return gw.indexer
}

// finalizeReconcile publishes commit freshness only while holding the stable
// repository lane. IndexRepo replacement uses the same lane, so the registry
// lookup and state restamp cannot land on a retired Indexer. lastSHA advances
// after the restamp; failed lane admission therefore leaves the prior SHA for
// the next ref notification to retry.
func (gw *GitWatcher) finalizeReconcile(ctx context.Context, newSHA string) error {
	return gw.indexer.coordinateRepositoryMutation(ctx, func() error {
		idx := gw.registeredIndexer()
		if idx == nil {
			return fmt.Errorf("git-watcher: repository indexer is no longer registered")
		}
		idx.reconcileRepoIndexState(gw.repoPath)
		gw.mu.Lock()
		gw.lastSHA = newSHA
		gw.mu.Unlock()
		return nil
	})
}

// Start sets up fsnotify watches on the repo's git control files and
// launches the event-processing goroutine. Safe to call once per
// GitWatcher instance. Returns an error (and does not launch the loop)
// when the repo has no .git directory — Stop remains safe to call.
func (gw *GitWatcher) Start() error {
	gitDir, err := resolveGitDir(gw.repoPath)
	if err != nil {
		return fmt.Errorf("resolve .git dir for %s: %w", gw.repoPath, err)
	}
	// HEAD + refs/heads/ cover branch switches and same-branch
	// commits; packed-refs covers the gc case where loose refs get
	// packed and moved out of refs/heads. Missing files are not fatal
	// — a fresh repo may not have packed-refs yet.
	for _, rel := range []string{"HEAD", "packed-refs", "refs/heads"} {
		path := filepath.Join(gitDir, rel)
		if _, err := os.Stat(path); err != nil {
			continue
		}
		if err := gw.fsw.Add(path); err != nil {
			gw.logger.Warn("git-watcher: failed to watch",
				zap.String("path", path), zap.Error(err))
		}
	}

	head, _ := gw.currentSHA(context.Background())
	seed := gw.seedSHA(head)
	gw.mu.Lock()
	gw.lastSHA = seed
	gw.loopStarted = true
	gw.mu.Unlock()
	go gw.loop()

	// A commit that landed while no watcher was running produces no event to
	// react to, so the catch-up has to be started here or it never happens.
	// It goes through the debounce like any other trigger: Start runs on the
	// warmup path, concurrently across every tracked repository, and must not
	// block on a diff.
	if seed != "" && head != "" && seed != head {
		gw.logger.Info("git-watcher: catching up on commits that landed while the watcher was down",
			zap.String("from", seed[:min(len(seed), 12)]),
			zap.String("to", head[:min(len(head), 12)]))
		gw.scheduleReconcile("startup-catchup")
	}
	return nil
}

// seedSHA picks the commit the next ref change should be diffed against.
//
// The working tree's HEAD is the wrong answer whenever HEAD moved while no
// watcher was running — across a daemon restart, or between a repository
// being tracked and its watcher being started. Seeding from it makes that
// move permanently invisible: the next event reads the current SHA, finds
// lastSHA already equal to it, and returns without ever taking a diff. The
// graph keeps the previous commit's content, nothing restamps the freshness
// row, and `gortex repos` reports the repository stale for the whole life of
// the daemon with no path back.
//
// The persisted index state records the commit the graph was actually built
// at, which is the honest base. It is used only when git can still resolve
// it: a stamp left dangling by a force-push or a gc would otherwise make
// every diff fail and every reconcile retry forever against a commit that no
// longer exists.
func (gw *GitWatcher) seedSHA(head string) string {
	idx := gw.registeredIndexer()
	if idx == nil {
		return head
	}
	r, ok := graph.Store(idx.graph).(graph.RepoIndexStateReader)
	if !ok {
		return head
	}
	st, found, err := r.GetRepoIndexState(idx.repoPrefix)
	if err != nil || !found || st.IndexedSHA == "" || st.IndexedSHA == head {
		return head
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := gitcmd.Output(ctx, gw.repoPath, "cat-file", "-e", st.IndexedSHA+"^{commit}"); err != nil {
		gw.logger.Warn("git-watcher: indexed commit is unreachable, starting from HEAD",
			zap.String("indexed", st.IndexedSHA), zap.Error(err))
		return head
	}
	return st.IndexedSHA
}

// Stop halts the watcher. Idempotent — safe whether Start succeeded,
// failed, or was never called. We only block on `stopped` when the
// loop goroutine is actually running; otherwise Stop would deadlock
// on a channel nobody's going to close.
func (gw *GitWatcher) Stop() error {
	gw.mu.Lock()
	started := gw.loopStarted
	already := gw.stopCalled
	gw.stopCalled = true
	gw.mu.Unlock()
	if already {
		return nil
	}
	close(gw.done)
	_ = gw.fsw.Close()
	if started {
		<-gw.stopped
	}
	return nil
}

func (gw *GitWatcher) loop() {
	defer close(gw.stopped)
	for {
		select {
		case <-gw.done:
			return
		case event, ok := <-gw.fsw.Events:
			if !ok {
				return
			}
			gw.scheduleReconcile(event.Name)
		case err, ok := <-gw.fsw.Errors:
			if !ok {
				return
			}
			gw.logger.Warn("git-watcher: fsnotify error", zap.Error(err))
		}
	}
}

// scheduleReconcile coalesces bursts of ref-file events (a branch
// switch touches HEAD, refs/heads/<new>, and often packed-refs in
// rapid succession) into a single reconcile. Resets the debounce
// timer on every event and lets the last one win.
func (gw *GitWatcher) scheduleReconcile(trigger string) {
	gw.mu.Lock()
	defer gw.mu.Unlock()
	if gw.fireTimer != nil {
		gw.fireTimer.Stop()
	}
	gw.fireTimer = time.AfterFunc(gw.debounce, func() {
		gw.reconcile(trigger)
	})
}

// gitWatcherScopedResolveMaxFiles caps how many changed files a ref
// reconcile resolves through the scoped incremental path before it
// falls back to a single whole-graph ResolveAll. Below the cap a
// commit-sized change resolves only its own files (and their incoming
// refs) -- the same path edit_file uses; above it (branch switches,
// large merges) one amortised whole-graph pass beats many per-file
// resolves. A package var so tests can lower it to exercise the
// fallback.
var gitWatcherScopedResolveMaxFiles = 100

// changedAbsPaths collects the absolute on-disk paths a diff touched --
// the new path for every status plus the old path of a rename --
// deduplicated. Disk existence is resolved later by
// IncrementalReindexPaths: a path present on disk is reindexed, one
// absent is evicted, so a delete needs no special-casing here.
func (gw *GitWatcher) changedAbsPaths(changes []gitChange) []string {
	seen := make(map[string]struct{}, len(changes)+4)
	out := make([]string, 0, len(changes)+4)
	add := func(rel string) {
		if rel == "" {
			return
		}
		abs := filepath.Join(gw.repoPath, rel)
		if _, ok := seen[abs]; ok {
			return
		}
		seen[abs] = struct{}{}
		out = append(out, abs)
	}
	for _, c := range changes {
		add(c.Path)
		add(c.OldPath)
	}
	return out
}

// reconcile reads the current HEAD SHA, diffs against the previously
// seen SHA via `git diff --name-status`, and dispatches
// the changed paths to a scoped reindex + resolve (whole-graph only
// above gitWatcherScopedResolveMaxFiles). Silently no-ops when HEAD hasn't moved — branches can
// touch packed-refs without the resolved commit actually changing.
func (gw *GitWatcher) reconcile(trigger string) {
	// Single-flight: one reconcile at a time. A ref change observed
	// mid-flight is coalesced into exactly one follow-up run that
	// diffs the (now advanced) lastSHA against wherever HEAD ended
	// up — so we always converge on the latest state without ever
	// running two whole-graph passes concurrently.
	gw.mu.Lock()
	if gw.reconciling {
		gw.rerun = true
		gw.mu.Unlock()
		return
	}
	gw.reconciling = true
	gw.mu.Unlock()
	defer func() {
		gw.mu.Lock()
		gw.reconciling = false
		again := gw.rerun && !gw.stopCalled
		gw.rerun = false
		gw.mu.Unlock()
		if again {
			go gw.reconcile("coalesced")
		}
	}()

	if gw.rebaseInProgress() {
		// Defer until the rebase lands. The final rebase state
		// either updates HEAD (triggering another fsnotify fire) or
		// leaves the branch where it was — in both cases we'll pick
		// up the right end state on the next event.
		gw.logger.Debug("git-watcher: rebase in progress, deferring",
			zap.String("trigger", trigger))
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	newSHA, err := gw.currentSHA(ctx)
	if err != nil || newSHA == "" {
		return
	}

	gw.mu.Lock()
	oldSHA := gw.lastSHA
	gw.mu.Unlock()
	if oldSHA == newSHA {
		return
	}

	// First observation (no prior SHA) needs no diff because the caller
	// warmed up with a full index already, but its SHA/restamp publication
	// still uses the repository lane and current Indexer.
	if oldSHA == "" {
		if err := gw.finalizeReconcile(context.Background(), newSHA); err != nil {
			gw.logger.Warn("git-watcher: freshness finalization failed",
				zap.String("trigger", trigger), zap.Error(err))
		}
		return
	}

	start := time.Now()
	changes, err := gw.diffNameStatus(ctx, oldSHA, newSHA)
	if err != nil {
		gw.logger.Warn("git-watcher: diff failed",
			zap.String("from", oldSHA), zap.String("to", newSHA), zap.Error(err))
		return
	}

	// Every ref-sized change set now uses the same bounded multi-file
	// parse/evict pipeline. The historical threshold remains observable so
	// branch-switch tests and logs distinguish large reconciles, but it no
	// longer selects the N-per-file + whole-graph fallback that caused the
	// warm/storm regression.
	changedPaths := gw.changedAbsPaths(changes)
	largeBatch := len(changedPaths) > gitWatcherScopedResolveMaxFiles
	patched, failed := 0, 0
	if len(changedPaths) > 0 {
		res, rerr := gw.reindexChangedPaths(changedPaths)
		if rerr != nil {
			// The bounded pipeline isolates individual unreadable/parse-failed
			// files in FailedFiles. A returned error is batch-level: keep the old
			// SHA so the next ref notification retries instead of hiding drift.
			gw.logger.Warn("git-watcher: batched reindex failed",
				zap.String("trigger", trigger),
				zap.Int("paths", len(changedPaths)),
				zap.Bool("large_batch", largeBatch),
				zap.Error(rerr))
			return
		}
		if res != nil {
			patched = res.StaleFileCount + res.DeletedFileCount
			failed = len(res.FailedFiles)
			if failed > 0 {
				gw.logger.Warn("git-watcher: batched reindex left failed files",
					zap.String("trigger", trigger),
					zap.Int("paths", len(changedPaths)),
					zap.Int("failed", failed),
					zap.Bool("large_batch", largeBatch))
				return
			}
		}
	}

	// Publish the new SHA and on-disk freshness row together under the stable
	// repository lane. Empty commits reach this tail without calling the batch
	// reindexer; lane admission failure intentionally keeps oldSHA for retry.
	if err := gw.finalizeReconcile(context.Background(), newSHA); err != nil {
		gw.logger.Warn("git-watcher: freshness finalization failed",
			zap.String("trigger", trigger),
			zap.String("from", oldSHA),
			zap.String("to", newSHA),
			zap.Error(err))
		return
	}
	gw.mu.Lock()
	drained := gw.drained
	gw.mu.Unlock()

	gw.logger.Info("git-watcher: reconciled ref change",
		zap.String("trigger", trigger),
		zap.String("from", oldSHA[:min(len(oldSHA), 12)]),
		zap.String("to", newSHA[:min(len(newSHA), 12)]),
		zap.Int("paths", patched),
		zap.Int("failed", failed),
		zap.Bool("large_batch", largeBatch),
		zap.Duration("elapsed", time.Since(start)))

	if drained != nil {
		drained(patched)
	}
}

// gitChange describes one entry in `git diff --name-status` output.
// Status is a single char (A/M/D/T) or R/C with a similarity score.
type gitChange struct {
	Status  byte
	Path    string
	OldPath string // only populated for R/C
}

// applyChanges is retained for in-package compatibility. It delegates the
// entire diff to the bounded batch runner; it never loops through point
// IndexFileNoResolve/EvictFile mutations.
func (gw *GitWatcher) applyChanges(changes []gitChange) int {
	paths := gw.changedAbsPaths(changes)
	if len(paths) == 0 {
		return 0
	}
	result, err := gw.reindexChangedPaths(paths)
	if err != nil {
		gw.logger.Warn("git-watcher: batched apply failed",
			zap.Int("paths", len(paths)), zap.Error(err))
		return 0
	}
	if result == nil {
		return 0
	}
	return result.StaleFileCount + result.DeletedFileCount
}

// currentSHA returns the resolved commit SHA of HEAD. Shells out to
// git rather than parsing .git/HEAD directly so symbolic refs,
// packed-refs, and worktree indirection all work without us
// reimplementing git's ref resolution.
func (gw *GitWatcher) currentSHA(ctx context.Context) (string, error) {
	return gitcmd.Output(ctx, gw.repoPath, "rev-parse", "HEAD")
}

// diffNameStatus shells out to `git diff --name-status -M -C oldSHA..newSHA`
// and decodes the output into gitChange records. -M enables rename
// detection, -C enables copy detection.
func (gw *GitWatcher) diffNameStatus(ctx context.Context, oldSHA, newSHA string) ([]gitChange, error) {
	out, err := gitcmd.Run(ctx, gw.repoPath,
		"diff", "--name-status", "-M", "-C", "-z", oldSHA, newSHA)
	if err != nil {
		return nil, err
	}
	return parseDiffNameStatus(out), nil
}

// parseDiffNameStatus decodes the `-z` NUL-delimited output of
// `git diff --name-status`. Each entry is: STATUS\0path[\0newpath].
// Rename (R) and copy (C) statuses carry a similarity score appended
// to the letter (e.g., "R100") and come with two paths separated by
// a NUL. Everything else is a single path.
func parseDiffNameStatus(out []byte) []gitChange {
	var changes []gitChange
	// bufio.Scanner with a NUL split function gives us one token per
	// field; we consume them in pairs (or triples for R/C).
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	scanner.Split(scanNul)

	for scanner.Scan() {
		status := scanner.Text()
		if status == "" {
			continue
		}
		letter := status[0]
		if !scanner.Scan() {
			break
		}
		path := scanner.Text()
		c := gitChange{Status: letter, Path: path}
		if letter == 'R' || letter == 'C' {
			if !scanner.Scan() {
				break
			}
			c.OldPath = path
			c.Path = scanner.Text()
		}
		changes = append(changes, c)
	}
	return changes
}

// scanNul is a bufio.SplitFunc that tokenises on NUL bytes. Used for
// `git diff -z` output where paths may contain whitespace.
func scanNul(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	for i, b := range data {
		if b == 0 {
			return i + 1, data[:i], nil
		}
	}
	if atEOF {
		return len(data), data, nil
	}
	return 0, nil, nil
}

// rebaseInProgress reports whether a rebase, merge, or cherry-pick is
// currently in flight — any of which touch HEAD/refs rapidly and
// produce intermediate states we don't want to reconcile against.
// Detection is by sentinel file presence in .git (the canonical way
// other git tooling does it).
func (gw *GitWatcher) rebaseInProgress() bool {
	gitDir, err := resolveGitDir(gw.repoPath)
	if err != nil {
		return false
	}
	for _, sentinel := range []string{"rebase-merge", "rebase-apply",
		"MERGE_HEAD", "CHERRY_PICK_HEAD", "BISECT_LOG"} {
		if _, err := os.Stat(filepath.Join(gitDir, sentinel)); err == nil {
			return true
		}
	}
	return false
}

// resolveGitDir returns the absolute path to the .git directory for a
// worktree. Handles the worktree / submodule case where .git is a
// file containing `gitdir: <path>` instead of a directory.
func resolveGitDir(repoPath string) (string, error) {
	candidate := filepath.Join(repoPath, ".git")
	info, err := os.Stat(candidate)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return candidate, nil
	}
	// .git is a file pointing at the real gitdir — common for
	// worktrees (under modules/<name>) and submodules.
	content, err := os.ReadFile(candidate)
	if err != nil {
		return "", err
	}
	line := strings.TrimSpace(string(content))
	const prefix = "gitdir:"
	if !strings.HasPrefix(line, prefix) {
		return "", fmt.Errorf("malformed .git file: %s", candidate)
	}
	dir := strings.TrimSpace(strings.TrimPrefix(line, prefix))
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(repoPath, dir)
	}
	return dir, nil
}
