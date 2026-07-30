package indexer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/sgtdi/fswatcher"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/excludes"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/pathkey"
	"github.com/zzet/gortex/internal/reach"
)

// ChangeKind describes the type of filesystem change.
type ChangeKind string

const (
	ChangeCreated  ChangeKind = "created"
	ChangeModified ChangeKind = "modified"
	ChangeDeleted  ChangeKind = "deleted"
	ChangeRenamed  ChangeKind = "renamed"
)

// GraphChangeEvent is emitted after a successful graph patch.
type GraphChangeEvent struct {
	FilePath       string     `json:"file_path"`
	Kind           ChangeKind `json:"kind"`
	Classification string     `json:"classification,omitempty"`
	NodesAdded     int        `json:"nodes_added"`
	NodesRemoved   int        `json:"nodes_removed"`
	EdgesAdded     int        `json:"edges_added"`
	EdgesRemoved   int        `json:"edges_removed"`
	Timestamp      time.Time  `json:"timestamp"`
	DurationMs     int64      `json:"duration_ms"`
}

// MutationTicket identifies one admitted file mutation and resolves when the
// graph has applied this generation or a newer coalesced generation.
type MutationTicket struct {
	Path       string
	Generation uint64
	Done       <-chan MutationResult
}

// MutationResult is the terminal graph-freshness result for a ticket.
type MutationResult struct {
	RequestedGeneration uint64
	AppliedGeneration   uint64
	Reindexed           bool
	Err                 error
}

// SymbolChangeCallback is called when symbols change during file re-indexing.
// It receives the file path, old symbols (before eviction), and new symbols (after re-index).
type SymbolChangeCallback func(filePath string, oldSymbols, newSymbols []*graph.Node)

// Watcher keeps the knowledge graph in live sync with the filesystem.
type Watcher struct {
	indexer  *Indexer
	fsw      fswatcher.Watcher
	fsCancel context.CancelFunc
	config   config.WatchConfig
	// degradedNoFsnotify is set when Start detected a slow mount (a WSL2
	// 9p/drvfs Windows drive, an SMB share) and skipped the native fsnotify
	// backend, relying on the adaptive poller + git hooks instead.
	degradedNoFsnotify bool
	excludes           *excludes.Matcher
	events             chan GraphChangeEvent
	history            []GraphChangeEvent
	historyMu          sync.Mutex
	pending            map[string]*time.Timer
	pendingGeneration  map[string]uint64
	mutationWaiters    map[string]map[uint64]chan MutationResult
	nextGeneration     uint64
	mu                 sync.Mutex
	stopping           bool
	asyncWork          sync.WaitGroup
	// patchMu serialises per-path patchGraph invocations so the
	// post-patch reach rebuild (which scans every Node.Meta) cannot
	// race with another debounced patch's IndexFile / EvictFile /
	// detectClonesAndEmitEdges, all of which mutate the same Meta
	// maps unprotected. Storm-mode uses patchGraphNoResolve (driven
	// from a single goroutine in drainStorm) and bypasses this lock.
	patchMu          sync.Mutex
	logger           *zap.Logger
	done             chan struct{}
	stopped          chan struct{}
	stoppedOnce      sync.Once
	symbolChangeCb   SymbolChangeCallback
	symbolChangeCbMu sync.RWMutex

	// Degraded-watch state: set when the OS can't fully cover the tree —
	// inotify watch exhaustion (ENOSPC) or FD exhaustion (EMFILE/ENFILE).
	// degradedReason is the operator-facing explanation (surfaced as a
	// whole-index "frozen" banner on read tools); degradedLogged makes the
	// operator log warning fire exactly once; degradedCb (optional) pushes the
	// notice onto the daemon's health channel. Guarded by degradedMu.
	degradedMu     sync.RWMutex
	degradedReason string
	degradedLogged bool
	degradedCb     func(reason string)

	// probeWaiters maps a probe-file path (created during Start to confirm
	// the inotify watch is active) to a chan that handleEvent closes when
	// the probe's event arrives. Empty after Start returns.
	probeWaiters sync.Map
	// initialReplayProbeWritten is a test seam fired after the Darwin startup
	// barrier creates a marker inside a watched root. nil in production.
	initialReplayProbeWritten func(string)
	// initialReplayDrainStarted and initialReplayMarkerCreating are test seams
	// for writes at the two Darwin startup ordering boundaries. nil in
	// production.
	initialReplayDrainStarted   func()
	initialReplayMarkerCreating func(string)
	// mutationBeforeAdmission and pointMutationClaimed expose the two point
	// mutation lifecycle boundaries to deterministic shutdown tests.
	mutationBeforeAdmission  func()
	pointMutationClaimed     func(string)
	stopAdmissionClosed      func()
	startFailureBeforeSignal func()

	// Storm-mode state. Guarded by stormMu so the hot per-file
	// debounce path (mu) doesn't contend with rate-tracking.
	stormMu          sync.Mutex
	eventTimes       []time.Time           // sliding window of recent event timestamps
	stormBatch       map[string]ChangeKind // dirty set during an event storm
	stormGenerations map[string]uint64     // newest debounced generation adopted per path
	stormTimer       *time.Timer           // fires after the quiet period
	stormActive      bool                  // true while waiting to drain
	stormStopped     bool                  // Stop has closed storm admission
	stormWork        sync.WaitGroup        // scheduled/running timer callbacks
	stormDrained     func(int)             // test hook: batch drained; batch size arg
	stormBeforeLock  func()                // test hook: immediately before patchMu acquisition
	batchReindex     watcherBatchReindex   // one bounded batch; MultiWatcher installs shared catch-up

	// poller is the adaptive-interval fallback that re-checks git
	// HEAD movement and tracked-file mtimes on a timer, catching the
	// changes the fsnotify backend misses (inotify watch exhaustion,
	// network filesystems, dropped events). Created in Start and torn
	// down in Stop alongside the fsnotify backend. nil when the
	// per-repo watcher is disabled via WatchConfig.Enabled.
	poller *Poller

	// reconcileMu guards the overflow-driven full-tree reconcile.
	// reconcilePending coalesces a burst of overflow / dropped-event
	// signals into at most one reconcile in flight: the kernel inotify
	// queue can overflow (EventOverflow) or the backend can drop events
	// under backpressure (the Dropped() channel), and either means we
	// may have lost a create/modify with no path to re-index. macOS
	// FSEvents self-heals (it re-scans on UserDropped/KernelDropped),
	// but Linux inotify does not — without this the lost event waits on
	// the up-to-1h janitor. reconcileFn is a test seam: nil in
	// production (the real IncrementalReindex runs).
	reconcileMu      sync.Mutex
	reconcilePending bool
	reconcileFn      func()

	// pendingScanDirs coalesces newly-created directories awaiting a
	// scoped subtree re-index — the new-subdir race (see enqueueDirScan).
	// dirScanActive guards a single in-flight drainer goroutine; scanFn
	// is a test seam, nil in production (the real IncrementalReindexPaths
	// runs). All three are guarded by reconcileMu.
	pendingScanDirs map[string]struct{}
	dirScanActive   bool
	scanFn          func(map[string]struct{})

	// pendingReresolve coalesces files the shape-degradation guard flagged
	// for a forced scoped re-resolve (see enqueueReresolve). reresolveActive
	// guards a single in-flight drainer; reresolveFn is a test seam, nil in
	// production (the real ReresolveFileScoped runs). All three are guarded
	// by reconcileMu.
	pendingReresolve map[string]struct{}
	reresolveActive  bool
	reresolveFn      func(map[string]struct{})
}

const maxHistory = 1000

var (
	errMutationSuperseded   = errors.New("mutation generation superseded")
	errMutationPatchAborted = errors.New("mutation patch aborted")
	errWatcherStopped       = errors.New("watcher stopped before mutation completed")
)

// probeMarker is the substring embedded in handshake-probe filenames
// (see confirmWatchActive) and used by handleEvent to absorb their
// create/remove events without touching the indexer.
const probeMarker = ".gortex-watcher-handshake-"

// NewWatcher creates a Watcher for the given indexer.
//
// cfg.Exclude is expected to carry the full effective pattern list (from
// ConfigManager.EffectiveExclude). If it is empty — e.g. a direct caller
// that bypasses ConfigManager — the watcher falls back to the builtin
// baseline so the obvious non-source dirs stay ignored.
func NewWatcher(idx *Indexer, cfg config.WatchConfig, logger *zap.Logger) (*Watcher, error) {
	debounce := cfg.DebounceMs
	if debounce <= 0 {
		debounce = 150
	}
	cfg.DebounceMs = debounce

	// Storm-mode defaults — kept conservative so a repo producing
	// normal save traffic stays on the per-file path. Threshold of
	// zero means the user explicitly disabled storm mode; negative is
	// coerced to zero for safety.
	if cfg.StormThreshold < 0 {
		cfg.StormThreshold = 0
	}
	if cfg.StormWindowMs <= 0 {
		cfg.StormWindowMs = 500
	}
	if cfg.StormQuietPeriodMs <= 0 {
		cfg.StormQuietPeriodMs = 500
	}

	patterns := cfg.Exclude
	if len(patterns) == 0 {
		patterns = excludes.Builtin
	}

	return &Watcher{
		indexer:           idx,
		config:            cfg,
		excludes:          excludes.New(patterns),
		events:            make(chan GraphChangeEvent, 64),
		pending:           make(map[string]*time.Timer),
		pendingGeneration: make(map[string]uint64),
		stormBatch:        make(map[string]ChangeKind),
		stormGenerations:  make(map[string]uint64),
		logger:            logger,
		done:              make(chan struct{}),
		stopped:           make(chan struct{}),
	}, nil
}

// Start begins watching the given paths recursively. The backend is
// fswatcher, which uses FSEvents on macOS (one stream per root,
// constant FD cost) and inotify on Linux (one watch per directory in
// the tree). On the inotify path the per-user `max_user_watches` cap
// applies; bump that sysctl if a multi-repo install grows beyond it.
func (w *Watcher) Start(paths []string) (retErr error) {
	loopLaunched := false
	defer func() {
		if retErr == nil || loopLaunched {
			return
		}
		if w.startFailureBeforeSignal != nil {
			w.startFailureBeforeSignal()
		}
		w.signalStopped()
	}()
	if len(paths) == 0 {
		return errors.New("watcher: no paths to watch")
	}

	// WSL2 / slow-mount degradation: on a 9p/drvfs mount (a Windows drive
	// under WSL2, an SMB share) native fsnotify delivers events late or not
	// at all, and confirmWatchActive would hang ~5s per path before timing
	// out. Skip the fsnotify backend entirely and rely on the adaptive
	// poller + git hooks, which are mount-agnostic. The downstream code
	// already tolerates a nil fsw. GORTEX_FORCE_FSNOTIFY=1 overrides.
	if w.config.Enabled {
		probe := paths[0]
		if abs, err := filepath.Abs(probe); err == nil {
			probe = abs
		}
		if slowWatchMount(probe) {
			w.degradedNoFsnotify = true
			w.logger.Warn("watcher: slow mount detected — disabling native fsnotify, using adaptive poller fallback",
				zap.String("path", probe))
			w.poller = newPoller(w, w.indexer, w.logger)
			w.poller.Start()
			return nil
		}
	}
	ready := make(chan struct{})
	// Own the events/dropped channels so the library never closes them on
	// teardown. fswatcher's shutdown closes its events channel while its
	// EventAggregator goroutine may still be flushing a final event into
	// it — a "send on closed channel" panic under -race on the Linux
	// inotify path (the aggregator's close() does not join its run loop).
	// When we supply the channels, ownsEventsChannel is false and the
	// library skips the close; the aggregator's send is already
	// non-blocking, so a late flush lands harmlessly in our buffer (or
	// the dropped channel) and our loop still exits on its own stop
	// signal, not on the channel closing. Buffer sizes match the
	// library's defaults so coalescing behaviour is unchanged.
	droppedSize := max(fswatcher.DefaultBufferSize/fswatcher.MaxDroppedBufferRatio, fswatcher.MinDroppedBuffer)
	fswEvents := make(chan fswatcher.WatchEvent, fswatcher.DefaultBufferSize)
	fswDropped := make(chan fswatcher.WatchEvent, droppedSize)
	opts := []fswatcher.WatcherOpt{
		// Disable fswatcher's internal debouncer. Its mergeEvents path
		// mutates the Types backing array of an already-delivered event
		// when a follow-up event for the same path arrives, racing with
		// our consumer's read. Our per-file debounce + storm-mode logic
		// is the authoritative coalescer anyway.
		fswatcher.WithCooldown(0),
		// Drop the library's own logging chatter; we surface what we
		// care about through our own logger.
		fswatcher.WithSeverity(fswatcher.SeverityError),
		// Block Start until the OS-level streams are actually live.
		// Without this the first events after Start race against
		// stream registration and silently disappear.
		fswatcher.WithReadyChannel(ready),
		// We own the channels (see above) — eliminates the teardown
		// send-on-closed-channel race.
		fswatcher.WithCustomChannels(fswEvents, fswDropped),
	}
	absPaths := make([]string, 0, len(paths))
	for _, p := range paths {
		absPath, err := filepath.Abs(p)
		if err != nil {
			return err
		}
		absPaths = append(absPaths, absPath)
		opts = append(opts, fswatcher.WithPath(absPath))
	}
	fsw, err := fswatcher.New(opts...)
	if err != nil {
		return err
	}
	w.fsw = fsw

	ctx, cancel := context.WithCancel(context.Background())
	w.fsCancel = cancel
	watchErr := make(chan error, 1)
	go func() {
		err := fsw.Watch(ctx)
		watchErr <- err
		if err != nil && !errors.Is(err, context.Canceled) && w.logger != nil {
			w.logger.Warn("watcher: backend stopped", zap.Error(err))
		}
	}()
	// Wait for the backend to become ready or fail fast on early
	// initialisation errors (e.g. an inotify add returning ENOSPC).
	select {
	case <-ready:
	case err := <-watchErr:
		cancel()
		// Watch / FD exhaustion is not a hard failure: keep Start succeeding,
		// log a one-time operator warning, and fall back to the adaptive
		// poller so the graph still catches git-HEAD + mtime changes. Failing
		// here would leave a busy machine with no daemon at all.
		if isInotifyExhausted(err) || isFDExhausted(err) {
			w.noteWatchDegraded(err)
			w.degradedNoFsnotify = true
			if w.fsw != nil {
				w.fsw.Close()
				w.fsw = nil
			}
			if w.config.Enabled {
				w.poller = newPoller(w, w.indexer, w.logger)
				w.poller.Start()
			}
			return nil
		}
		return err
	case <-time.After(5 * time.Second):
		cancel()
		return errors.New("watcher: backend did not become ready within 5s")
	}
	// FSEvents reports its stream as "started" before its startup replay has
	// drained. Synthetic exists events and genuine writes are indistinguishable,
	// so collect (never discard) the quiet-window prefix, then merge it with the
	// ordered marker tail. Persisted file receipts remove unchanged replay paths
	// in one batch query; genuine writes are reconciled before Start returns.
	if runtime.GOOS == "darwin" {
		initialReplay, err := w.drainInitialReplay(150 * time.Millisecond)
		if err == nil {
			err = w.reconcileInitialReplayThroughMarkers(absPaths, 5*time.Second, initialReplay)
		}
		if err != nil {
			cancel()
			if w.fsw != nil {
				w.fsw.Close()
			}
			return err
		}
	}
	loopLaunched = true
	go w.loop()
	// On Linux, fswatcher closes its ready channel as soon as the
	// inotify FD is allocated, but it registers initial paths in
	// background goroutines that may not have called inotify_add_watch
	// yet. Events fired before those goroutines run are lost forever.
	// Probe each path with a sentinel file and wait for the resulting
	// event before declaring the watcher ready.
	if runtime.GOOS != "darwin" {
		for _, p := range absPaths {
			if err := w.confirmWatchActive(p, 5*time.Second); err != nil {
				cancel()
				if w.fsw != nil {
					w.fsw.Close()
				}
				close(w.done)
				<-w.stopped
				return err
			}
		}
	}

	// Launch the adaptive-interval poller alongside the fsnotify
	// backend. It is a fallback for the changes fsnotify misses, so
	// it shares the watcher's lifecycle. Gated on WatchConfig.Enabled
	// — a repo that opted out of watching gets no fallback either.
	if w.config.Enabled {
		w.poller = newPoller(w, w.indexer, w.logger)
		w.poller.Start()
	}
	return nil
}

// confirmWatchActive writes sentinel files under root in a polling loop
// until the corresponding fswatcher event arrives — proving the
// OS-level watch is registered — or the overall timeout fires. The
// retry loop is needed because the first probe may be written before
// fswatcher's async addWatch goroutine has called inotify_add_watch,
// in which case its create event is invisible to inotify entirely.
//
// The sentinel name avoids fswatcher's built-in isSystemFile filter
// (which drops *.tmp / *.bak / *.swp / etc. before they reach our
// handleEvent) and our own excludes matcher.
func (w *Watcher) confirmWatchActive(root string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	const probeStep = 100 * time.Millisecond
	for time.Now().Before(deadline) {
		probe := filepath.Join(root, fmt.Sprintf("%s%d-%d", probeMarker, os.Getpid(), time.Now().UnixNano()))
		ch := make(chan struct{})
		w.probeWaiters.Store(probe, ch)
		if err := os.WriteFile(probe, nil, 0o600); err != nil {
			w.probeWaiters.Delete(probe)
			if w.logger != nil {
				w.logger.Warn("watcher: could not write probe; continuing without confirmation",
					zap.String("root", root), zap.Error(err))
			}
			return nil
		}
		select {
		case <-ch:
			_ = os.Remove(probe)
			return nil
		case <-time.After(probeStep):
			w.probeWaiters.Delete(probe)
			_ = os.Remove(probe)
		}
	}
	return fmt.Errorf("watcher: inotify watch on %s did not activate within %s", root, timeout)
}

// drainInitialReplay reads from the backend's events channel until
// `window` of quiet has elapsed with no further events. macOS FSEvents
// streams emit a burst of synthetic "exists" events at startup; this
// burst is bounded by the per-stream latency (~50 ms). The first call
// blocks at least one window so early events have a chance to arrive.
func (w *Watcher) drainInitialReplay(window time.Duration) (map[string]fswatcher.WatchEvent, error) {
	if w.fsw == nil {
		return nil, nil
	}
	deferred := make(map[string]fswatcher.WatchEvent)
	eventsCh := w.fsw.Events()
	droppedCh := w.fsw.Dropped()
	if w.initialReplayDrainStarted != nil {
		w.initialReplayDrainStarted()
	}
	t := time.NewTimer(window)
	defer t.Stop()
	for {
		select {
		case event, ok := <-eventsCh:
			if !ok {
				return nil, errors.New("watcher: event stream closed during startup replay drain")
			}
			if err := w.mergeInitialReplayEvent(deferred, event); err != nil {
				return nil, err
			}
			if !t.Stop() {
				select {
				case <-t.C:
				default:
				}
			}
			t.Reset(window)
		case _, ok := <-droppedCh:
			if !ok {
				droppedCh = nil
				continue
			}
			return nil, errors.New("watcher: event dropped during startup replay drain")
		case <-t.C:
			return deferred, nil
		}
	}
}

func (w *Watcher) mergeInitialReplayEvent(events map[string]fswatcher.WatchEvent, event fswatcher.WatchEvent) error {
	for _, eventType := range event.Types {
		if eventType == fswatcher.EventOverflow {
			return errors.New("watcher: event stream overflowed during startup replay barrier")
		}
	}
	path := filepath.Clean(normalizeEventPath(event.Path, w.indexer.rootPath))
	if path == "." || strings.Contains(filepath.Base(path), probeMarker) {
		return nil
	}
	event.Path = path
	if prior, exists := events[path]; exists {
		seen := make(map[fswatcher.EventType]struct{}, len(prior.Types)+len(event.Types))
		merged := make([]fswatcher.EventType, 0, len(prior.Types)+len(event.Types))
		for _, eventType := range append(prior.Types, event.Types...) {
			if _, duplicate := seen[eventType]; duplicate {
				continue
			}
			seen[eventType] = struct{}{}
			merged = append(merged, eventType)
		}
		event.Types = merged
	}
	events[path] = event
	return nil
}

// reconcileInitialReplayThroughMarkers closes the Darwin FSEvents startup
// ordering gap without scanning the repository. Each watched root gets one
// ignored marker after the quiet drain. FSEvents preserves order within a
// root stream, so observing that marker proves every earlier replay event for
// the root has reached this channel. Only paths actually observed in that tail
// are deduplicated and reconciled before Start returns.
func (w *Watcher) reconcileInitialReplayThroughMarkers(
	roots []string,
	timeout time.Duration,
	initialSets ...map[string]fswatcher.WatchEvent,
) error {
	if w.fsw == nil || len(roots) == 0 {
		return nil
	}
	markers := make(map[string]struct{}, len(roots))
	markerPaths := make([]string, 0, len(roots))
	initialSize := 0
	if len(initialSets) > 0 {
		initialSize = len(initialSets[0])
	}
	deferred := make(map[string]fswatcher.WatchEvent, initialSize)
	if len(initialSets) > 0 {
		for path, event := range initialSets[0] {
			deferred[path] = event
		}
	}
	defer func() {
		for _, marker := range markerPaths {
			_ = os.Remove(marker)
		}
	}()

	for i, root := range roots {
		markerDirs := make([]string, 0, 2)
		if info, err := os.Stat(filepath.Join(root, ".git")); err == nil && info.IsDir() {
			// Keep the transient barrier out of git status in normal worktrees.
			// A linked worktree has a .git file, so it falls back to the root
			// but is unlinked immediately after publication below.
			markerDirs = append(markerDirs, filepath.Join(root, ".git"))
		}
		markerDirs = append(markerDirs, root)
		markerBase := fmt.Sprintf("%s%d-%d-%d", probeMarker, os.Getpid(), time.Now().UnixNano(), i)
		var marker string
		var f *os.File
		var markerErr error
		for _, markerDir := range markerDirs {
			marker = filepath.Join(markerDir, markerBase)
			if w.initialReplayMarkerCreating != nil {
				w.initialReplayMarkerCreating(marker)
			}
			f, markerErr = os.OpenFile(marker, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			if markerErr == nil {
				break
			}
			if !errors.Is(markerErr, os.ErrPermission) && !errors.Is(markerErr, syscall.EROFS) {
				return fmt.Errorf("watcher: create startup stream marker in %s: %w", root, markerErr)
			}
		}
		if f == nil {
			if errors.Is(markerErr, syscall.EROFS) {
				readOnly, statErr := filesystemReadOnly(root)
				if statErr != nil {
					return fmt.Errorf("watcher: verify read-only startup root %s: %w", root, statErr)
				}
				if readOnly {
					if w.logger != nil {
						w.logger.Info("watcher: startup marker skipped on read-only filesystem",
							zap.String("root", root))
					}
					continue
				}
			}
			return fmt.Errorf("watcher: create startup stream marker in %s: %w", root, markerErr)
		}
		if err := f.Close(); err != nil {
			_ = os.Remove(marker)
			return fmt.Errorf("watcher: close startup stream marker in %s: %w", root, err)
		}
		marker = filepath.Clean(marker)
		markerPaths = append(markerPaths, marker)
		markers[marker] = struct{}{}
		if w.initialReplayProbeWritten != nil {
			w.initialReplayProbeWritten(marker)
		}
		// FSEvents retains the create/remove record after unlink. Remove now,
		// rather than after the barrier wait, so the marker cannot survive a
		// slow stream or appear in post-Start repository state. The defer is
		// still the panic/error safety net.
		if err := os.Remove(marker); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("watcher: remove startup stream marker in %s: %w", root, err)
		}
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	eventsCh := w.fsw.Events()
	droppedCh := w.fsw.Dropped()
	for len(markers) > 0 {
		select {
		case event, ok := <-eventsCh:
			if !ok {
				return errors.New("watcher: event stream closed during startup marker barrier")
			}
			path := filepath.Clean(normalizeEventPath(event.Path, w.indexer.rootPath))
			if _, marker := markers[path]; marker {
				delete(markers, path)
				continue
			}
			if strings.Contains(filepath.Base(path), probeMarker) {
				continue
			}
			if err := w.mergeInitialReplayEvent(deferred, event); err != nil {
				return err
			}
		case _, ok := <-droppedCh:
			if !ok {
				droppedCh = nil
				continue
			}
			return errors.New("watcher: event dropped during startup marker barrier")
		case <-timer.C:
			return fmt.Errorf("watcher: startup marker barrier did not complete within %s", timeout)
		}
	}

	if err := w.reconcileInitialReplayEvents(deferred); err != nil {
		return err
	}
	w.mu.Lock()
	idle := len(w.pending) == 0 && len(w.pendingGeneration) == 0
	w.mu.Unlock()
	if !idle {
		return errors.New("watcher: startup marker barrier left queued mutations")
	}
	return nil
}

func (w *Watcher) reconcileInitialReplayEvents(events map[string]fswatcher.WatchEvent) error {
	if len(events) == 0 {
		return nil
	}
	paths := make([]string, 0, len(events))
	for path := range events {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	unchanged, indexed, err := w.matchingInitialReplayReceipts(events, paths)
	if err != nil {
		return err
	}
	dirs := make(map[string]struct{})
	for _, path := range paths {
		event := events[path]
		if isGortexAtomicTemp(path) || w.isExcluded(path) {
			continue
		}
		kind := pickKind(event.Types)
		if kind == "" {
			continue
		}
		// FSEvents can label a write to a pre-existing file as Create+Mod. The
		// persisted receipt proves it was already indexed, so use the replace
		// path that evicts stale definitions instead of treating it as additive.
		if kind == ChangeCreated {
			if _, existed := indexed[path]; existed {
				kind = ChangeModified
			}
		}
		if kind == ChangeCreated || kind == ChangeModified {
			if info, err := os.Stat(path); err == nil && info.IsDir() {
				if hasEventType(event.Types, fswatcher.EventCreate) {
					dirs[path] = struct{}{}
				}
				continue
			}
		}
		if _, ok := w.indexer.effectiveLanguage(path, nil); !ok && kind != ChangeDeleted && kind != ChangeRenamed {
			continue
		}
		if _, replay := unchanged[path]; replay {
			continue
		}
		err := w.patchGraphAfterReceiptCheck(path, kind)
		if errors.Is(err, errFileVersionChanged) {
			// One bounded retry catches a second write that landed during the
			// first parse/commit. Continuous writers fail Start explicitly.
			err = w.patchGraphAfterReceiptCheck(path, kind)
		}
		if err != nil {
			return fmt.Errorf("watcher: reconcile startup event for %s: %w", path, err)
		}
	}
	if len(dirs) > 0 {
		w.runDirScan(dirs, nil)
	}
	return nil
}

type initialReplayReceiptCandidate struct {
	path              string
	relPath           string
	graphPath         string
	mtimeKey          string
	before            os.FileInfo
	receiptComparable bool
}

// matchingInitialReplayReceipts removes Darwin's synthetic startup replay
// without a per-file SQL loop. Existing source paths are fetched in one set
// query; equal-mtime paths then have their source read/transformed/hashed
// sequentially to keep memory bounded. Any missing receipt, changed mtime, or
// unstable read is a real reconciliation candidate.
func (w *Watcher) matchingInitialReplayReceipts(
	events map[string]fswatcher.WatchEvent,
	paths []string,
) (map[string]struct{}, map[string]struct{}, error) {
	reader, ok := w.indexer.graph.(graph.FileMetaPathReader)
	if !ok {
		return nil, nil, nil
	}
	candidates := make([]initialReplayReceiptCandidate, 0, len(paths))
	for _, path := range paths {
		if isGortexAtomicTemp(path) || w.isExcluded(path) {
			continue
		}
		kind := pickKind(events[path].Types)
		if kind != ChangeCreated && kind != ChangeModified {
			continue
		}
		before, err := os.Stat(path)
		if err != nil || before.IsDir() {
			continue
		}
		if _, supported := w.indexer.effectiveLanguage(path, nil); !supported {
			continue
		}
		relPath := w.indexer.graphRelKey(path)
		candidates = append(candidates, initialReplayReceiptCandidate{
			path:      path,
			relPath:   relPath,
			graphPath: w.indexer.prefixPath(relPath),
			mtimeKey:  w.indexer.relKey(path),
			before:    before,
		})
	}

	// The in-memory mtime map is only the fast hash gate. Every existing replay
	// path participates in the one set query so Create+Mod can still be routed
	// through replacement semantics when its mtime really changed.
	w.indexer.mtimeMu.RLock()
	for i := range candidates {
		candidate := &candidates[i]
		mtime, tracked := w.indexer.fileMtimes[candidate.mtimeKey]
		if tracked && mtime == candidate.before.ModTime().UnixNano() {
			candidate.receiptComparable = true
		}
	}
	w.indexer.mtimeMu.RUnlock()
	if len(candidates) == 0 {
		return nil, nil, nil
	}

	graphPaths := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		graphPaths = append(graphPaths, candidate.graphPath)
	}
	rows, err := reader.FileMetasByPaths(w.indexer.repoPrefix, graphPaths)
	if err != nil {
		return nil, nil, fmt.Errorf("watcher: read startup replay receipts: %w", err)
	}
	unchanged := make(map[string]struct{}, len(candidates))
	indexed := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		row, exists := rows[candidate.graphPath]
		if !exists {
			continue
		}
		indexed[candidate.path] = struct{}{}
		if !candidate.receiptComparable || row.ContentHash == "" {
			continue
		}
		src, err := os.ReadFile(candidate.path)
		if err != nil {
			continue
		}
		after, err := os.Stat(candidate.path)
		if err != nil || !sameFileVersion(candidate.before, after) {
			continue
		}
		src = w.indexer.transforms.run(candidate.relPath, src)
		if len(src) == row.Size && contentHashForSource(src) == row.ContentHash {
			unchanged[candidate.path] = struct{}{}
		}
	}
	return unchanged, indexed, nil
}

// Stop halts the watcher and cleans up resources.
func (w *Watcher) Stop() error {
	// Close global admission first. Every point timer and auxiliary reconcile
	// drainer takes an asyncWork token while holding this same gate, so no Add
	// can race the Wait below. A timer whose Stop succeeds hands its token back
	// here; a callback that is already runnable retains ownership until return.
	w.mu.Lock()
	w.stopping = true
	for _, timer := range w.pending {
		if timer.Stop() {
			w.asyncWork.Done()
		}
	}
	w.pending = make(map[string]*time.Timer)
	w.pendingGeneration = make(map[string]uint64)
	w.mu.Unlock()
	if w.stopAdmissionClosed != nil {
		w.stopAdmissionClosed()
	}

	// Close storm admission and detach queued work before teardown. Every
	// published quiet timer owns one stormWork token: a successful Stop hands
	// that token back here; otherwise the callback is already runnable and its
	// defer owns it. No Add can occur after stormStopped while this lock orders
	// recordInStorm against Wait below.
	w.stormMu.Lock()
	w.stormStopped = true
	w.stopStormTimerLocked()
	w.stormBatch = make(map[string]ChangeKind)
	w.stormGenerations = make(map[string]uint64)
	w.eventTimes = nil
	w.stormActive = false
	w.stormMu.Unlock()

	close(w.done)
	// Stop the adaptive poller so a poll cycle in flight can't dispatch a
	// point patch into a half-torn-down watcher.
	if w.poller != nil {
		w.poller.Stop()
	}
	// Queued directory and forced-resolve work has not entered the graph yet.
	// Drop it after admission closes; an already-running drainer is counted and
	// will observe the empty set before returning.
	w.reconcileMu.Lock()
	w.pendingScanDirs = nil
	w.pendingReresolve = nil
	w.reconcileMu.Unlock()
	w.failMutationWaiters(errWatcherStopped)

	// Never hold stormMu, reconcileMu, mu, patchMu, or the reach topology gate
	// while joining. Already-claimed point patches, directory scans, forced
	// resolves, overflow reconciles, and storm drains may finish, but all must
	// be completely out of graph/SQLite work before backend teardown.
	w.asyncWork.Wait()
	w.stormWork.Wait()
	if w.fsCancel != nil {
		w.fsCancel()
	}
	if w.fsw != nil {
		w.fsw.Close()
	}
	// In slow-mount degraded mode the fsnotify loop never ran, so its
	// stopped channel is never closed — don't block on it.
	if !w.degradedNoFsnotify {
		<-w.stopped
	}
	return nil
}

// Events returns a read-only channel of graph change events.
func (w *Watcher) Events() <-chan GraphChangeEvent {
	return w.events
}

// History returns recent change events (up to maxHistory).
func (w *Watcher) History() []GraphChangeEvent {
	w.historyMu.Lock()
	defer w.historyMu.Unlock()
	out := make([]GraphChangeEvent, len(w.history))
	copy(out, w.history)
	return out
}

// HistorySince returns change events after the given timestamp.
func (w *Watcher) HistorySince(since time.Time) []GraphChangeEvent {
	w.historyMu.Lock()
	defer w.historyMu.Unlock()
	var out []GraphChangeEvent
	for _, ev := range w.history {
		if ev.Timestamp.After(since) {
			out = append(out, ev)
		}
	}
	return out
}

// OnSymbolChange registers a callback that is invoked when symbols change
// during file re-indexing. The callback receives old symbols (before eviction)
// and new symbols (after re-index).
func (w *Watcher) OnSymbolChange(cb SymbolChangeCallback) {
	w.symbolChangeCbMu.Lock()
	defer w.symbolChangeCbMu.Unlock()
	w.symbolChangeCb = cb
}

// OnDegraded registers a callback invoked once when the file watcher first
// enters a degraded state (inotify / FD exhaustion). The daemon wires it to its
// health push-notification channel so a subscribed agent learns the index may
// be frozen without polling.
func (w *Watcher) OnDegraded(cb func(reason string)) {
	w.degradedMu.Lock()
	w.degradedCb = cb
	w.degradedMu.Unlock()
}

// DegradedReason returns a human-readable explanation when the native file
// watcher is running degraded — inotify watch exhaustion or FD exhaustion — so
// live edits may not reach the graph until a reindex. Empty when watching is
// healthy. Read tools surface this as a whole-index "frozen" banner, distinct
// from a per-file stale flag.
func (w *Watcher) DegradedReason() string {
	w.degradedMu.RLock()
	defer w.degradedMu.RUnlock()
	return w.degradedReason
}

// isInotifyExhausted reports whether err is the inotify watch-limit error
// (ENOSPC) — the kernel ran out of `fs.inotify.max_user_watches`.
func isInotifyExhausted(err error) bool { return errors.Is(err, syscall.ENOSPC) }

// isFDExhausted reports whether err is a file-descriptor-exhaustion error
// (EMFILE per-process, ENFILE system-wide).
func isFDExhausted(err error) bool {
	return errors.Is(err, syscall.EMFILE) || errors.Is(err, syscall.ENFILE)
}

// noteWatchDegraded records a watcher-degradation cause and logs a one-time
// operator warning (subsequent calls are silent — "warns once"). ENOSPC names
// the inotify watch-limit sysctl; FD exhaustion advises raising the open-file
// limit. The first occurrence also fires the optional degraded callback so the
// daemon can push the notice. Returns true on the first (logged) call.
func (w *Watcher) noteWatchDegraded(err error) bool {
	var reason, logMsg string
	switch {
	case isInotifyExhausted(err):
		reason = "inotify watch limit reached — the graph may miss live edits until you raise fs.inotify.max_user_watches and reindex (the adaptive poller covers some changes)"
		logMsg = "watcher: inotify watch limit (ENOSPC) — watches partially installed; raise fs.inotify.max_user_watches. Falling back to the adaptive poller for missed changes"
	case isFDExhausted(err):
		reason = "open-file limit reached — the watcher is degraded until you raise the process file-descriptor limit (ulimit -n) and reindex (the adaptive poller covers some changes)"
		logMsg = "watcher: file-descriptor limit (EMFILE/ENFILE) — watcher degraded; raise ulimit -n. Falling back to the adaptive poller"
	default:
		return false
	}
	w.degradedMu.Lock()
	first := !w.degradedLogged
	w.degradedReason = reason
	w.degradedLogged = true
	cb := w.degradedCb
	w.degradedMu.Unlock()
	if first {
		if w.logger != nil {
			w.logger.Warn(logMsg)
		}
		if cb != nil {
			cb(reason)
		}
	}
	return first
}

func (w *Watcher) loop() {
	defer w.signalStopped()
	if w.fsw == nil {
		// Test path: handleEvent is being driven directly without
		// having called Start. Block until Stop closes w.done.
		<-w.done
		return
	}
	eventsCh := w.fsw.Events()
	droppedCh := w.fsw.Dropped()
	for {
		select {
		case <-w.done:
			return
		case event, ok := <-eventsCh:
			if !ok {
				return
			}
			w.handleEvent(event)
		case _, ok := <-droppedCh:
			if !ok {
				// Backend tore down its dropped channel; keep
				// draining Events only.
				droppedCh = nil
				continue
			}
			// The backend dropped an event under backpressure (the
			// main Events channel was full). We don't know which path
			// was lost, so reconcile the whole tree.
			w.triggerOverflowReconcile("dropped-event")
		}
	}
}

func (w *Watcher) signalStopped() {
	w.stoppedOnce.Do(func() { close(w.stopped) })
}

// guardWatcherPanic recovers a panic in a watcher background goroutine —
// a debounced patch, a storm drain, an overflow reconcile, or a
// new-directory scan. Those goroutines call into the graph store, and
// store_sqlite turns a fatal storage error (a closed DB during a daemon
// restart, a busy/locked DB, disk-full) into a panic via panicOnFatal.
// The MCP tool path has its own firewall (wrapToolHandler); these
// fsnotify-driven goroutines don't route through it, so without this a
// single transient store error during a restart or rebuild takes the
// whole daemon down. Recovering aborts just that unit of work — the file
// stays stale until the next event or the reconcile janitor — instead of
// crashing the process.
func (w *Watcher) guardWatcherPanic(op string) {
	if r := recover(); r != nil && w.logger != nil {
		w.logger.Error("watcher: recovered from panic in background re-index",
			zap.String("op", op),
			zap.Any("panic", r),
			zap.Stack("stack"))
	}
}

// beginReconcileWork atomically admits one auxiliary background unit and
// returns with reconcileMu held. Holding the admission gate until the queue or
// active bit can be published ensures Stop's later queue clear observes every
// pre-shutdown admission; once stopping is set no WaitGroup Add can race Wait.
// The caller must unlock reconcileMu and either call Done or transfer the token
// to the goroutine it publishes.
func (w *Watcher) beginReconcileWork() bool {
	w.mu.Lock()
	if w.stopping {
		w.mu.Unlock()
		return false
	}
	w.asyncWork.Add(1)
	w.reconcileMu.Lock()
	w.mu.Unlock()
	return true
}

// triggerOverflowReconcile schedules a single coalesced full-tree
// reconcile in response to a lost-event signal (a kernel inotify queue
// overflow or a backpressure-dropped event). A burst of signals
// collapses into at most one reconcile in flight: the first caller sets
// reconcilePending and runs the reconcile off the event loop; concurrent
// callers observe the flag and return immediately. Best-effort and
// logged — the event loop is never blocked.
func (w *Watcher) triggerOverflowReconcile(reason string) {
	if !w.beginReconcileWork() {
		return
	}
	if w.reconcilePending {
		w.reconcileMu.Unlock()
		w.asyncWork.Done()
		return
	}
	w.reconcilePending = true
	fn := w.reconcileFn
	w.reconcileMu.Unlock()

	if w.logger != nil {
		w.logger.Warn("watcher: event signal lost — scheduling full-tree reconcile",
			zap.String("reason", reason),
			zap.String("root", w.indexer.rootPath))
	}

	go func() {
		defer w.asyncWork.Done()
		defer func() {
			w.reconcileMu.Lock()
			w.reconcilePending = false
			w.reconcileMu.Unlock()
		}()
		defer w.guardWatcherPanic("overflow-reconcile")
		if fn != nil {
			fn()
			return
		}
		var err error
		if w.batchReindex != nil {
			_, err = w.batchReindex(nil)
		} else {
			_, err = w.indexer.IncrementalReindexPaths(w.indexer.rootPath, nil)
		}
		if err != nil && w.logger != nil {
			w.logger.Warn("watcher: overflow reconcile failed",
				zap.String("reason", reason),
				zap.Error(err))
		}
	}()
}

// dirScanEscalateCap bounds the scoped new-directory scan: a burst that
// creates more than this many directories (a large checkout or unpack)
// escalates to a single full-tree reconcile instead of fanning out into
// that many scoped subtree walks.
const dirScanEscalateCap = 64

// enqueueDirScan schedules a scoped re-index of a newly-created
// directory's subtree, closing the new-subdir race: on Linux inotify a
// file written into a directory before its watch attaches fires no
// event. A burst of directory creates coalesces into a single in-flight
// drainer (mirrors triggerOverflowReconcile) — the first caller starts
// the goroutine, concurrent callers add their directory to
// pendingScanDirs and return. The drainer loops until the set is empty,
// so a directory enqueued while a scan is in flight is still picked up;
// nothing is lost and there is no debounce-timing race.
func (w *Watcher) enqueueDirScan(dir string) {
	if !w.beginReconcileWork() {
		return
	}
	if w.pendingScanDirs == nil {
		w.pendingScanDirs = make(map[string]struct{})
	}
	w.pendingScanDirs[dir] = struct{}{}
	if w.dirScanActive {
		w.reconcileMu.Unlock()
		w.asyncWork.Done()
		return
	}
	w.dirScanActive = true
	w.reconcileMu.Unlock()

	go func() {
		defer w.asyncWork.Done()
		for {
			w.reconcileMu.Lock()
			dirs := w.pendingScanDirs
			w.pendingScanDirs = nil
			if len(dirs) == 0 {
				w.dirScanActive = false
				w.reconcileMu.Unlock()
				return
			}
			fn := w.scanFn
			w.reconcileMu.Unlock()
			func() {
				defer w.guardWatcherPanic("dir-scan")
				w.runDirScan(dirs, fn)
			}()
		}
	}()
}

// runDirScan discovers files in the accumulated new directories. A large burst
// escalates to one full-tree additive discovery (dirScanEscalateCap); otherwise
// the scoped subtrees are walked in a single IncrementalReindexPaths
// call, which IsStale-gates each file so already-current files cost only
// a stat. fn is the test seam.
func (w *Watcher) runDirScan(dirs map[string]struct{}, fn func(map[string]struct{})) {
	if fn != nil {
		fn(dirs)
		return
	}
	if len(dirs) > dirScanEscalateCap {
		if w.logger != nil {
			w.logger.Info("watcher: large new-directory burst — full-tree discovery",
				zap.Int("dirs", len(dirs)), zap.String("root", w.indexer.rootPath))
		}
		if _, err := w.indexer.incrementalDiscoverPaths(w.indexer.rootPath, nil); err != nil && w.logger != nil {
			w.logger.Warn("watcher: new-directory discovery failed", zap.Error(err))
		}
		return
	}
	paths := make([]string, 0, len(dirs))
	for d := range dirs {
		paths = append(paths, d)
	}
	if _, err := w.indexer.incrementalDiscoverPaths(w.indexer.rootPath, paths); err != nil && w.logger != nil {
		w.logger.Warn("watcher: new-directory scan failed",
			zap.Strings("dirs", paths), zap.Error(err))
	}
}

// hasEventType reports whether the aggregated event-type set contains want.
func hasEventType(types []fswatcher.EventType, want fswatcher.EventType) bool {
	for _, t := range types {
		if t == want {
			return true
		}
	}
	return false
}

func isGortexAtomicTemp(path string) bool {
	// filepath.Base only recognizes the host separator. Watcher tests and
	// forwarded events can contain either slash, so split both explicitly.
	if idx := strings.LastIndexAny(path, `/\\`); idx >= 0 {
		path = path[idx+1:]
	}
	const marker = ".gortex.tmp-"
	idx := strings.LastIndex(path, marker)
	if idx < 0 {
		return false
	}
	suffix := path[idx+len(marker):]
	if suffix == "" {
		return false
	}
	for _, ch := range suffix {
		if ch < '0' || ch > '9' {
			return false
		}
	}
	return true
}

func (w *Watcher) handleEvent(event fswatcher.WatchEvent) {
	// Kernel queue overflow arrives as a pathless EventOverflow on the
	// Events channel: the Linux inotify and Windows backends emit it when
	// the kernel drops events and cannot tell us which paths were lost.
	// macOS FSEvents never emits it — the darwin backend absorbs
	// UserDropped/KernelDropped by re-scanning the affected subtree
	// internally — so this branch is effectively Linux/Windows-only. With
	// no path to re-index, trigger a coalesced full-tree reconcile and
	// stop; every path-based step below would misfire on the empty path.
	for _, t := range event.Types {
		if t == fswatcher.EventOverflow {
			w.triggerOverflowReconcile("queue-overflow")
			return
		}
	}

	path := normalizeEventPath(event.Path, w.indexer.rootPath)
	// Guarded edits use atomic temp files in the watched directory. They are
	// implementation artifacts, not source changes; indexing them duplicates
	// the subsequent target-file patch and can monopolize the serialized queue.
	if isGortexAtomicTemp(path) {
		return
	}

	// Probe artifacts: sentinel files Start writes to confirm the
	// OS-level watch is actually active. Their create event signals
	// the registered waiter; their remove event (after Start removes
	// the file) is silently absorbed so it never reaches user-visible
	// event consumers.
	if strings.Contains(filepath.Base(path), probeMarker) {
		if v, loaded := w.probeWaiters.LoadAndDelete(path); loaded {
			if ch, ok := v.(chan struct{}); ok {
				close(ch)
			}
		}
		return
	}

	// Skip events from excluded paths. A single matcher call covers
	// what the old code split across inExcludedDir + isExcluded.
	if w.isExcluded(path) {
		return
	}

	kind := pickKind(event.Types)
	if kind == "" {
		return
	}

	// Directory events. fswatcher with WatchNested attaches the watch
	// for a new directory itself, so we never re-attach. But on Linux
	// inotify that watch lands only AFTER the directory's create event is
	// read, so a file written into the directory in that gap fires no
	// event and would stay invisible until the hourly janitor. When the
	// event carries a Create, scan the new directory's subtree on disk so
	// those pre-watch files are picked up regardless of whether an event
	// ever fired ("watch first, then scan": files created after the watch
	// fire normal events, files created before are caught by the scan,
	// and the overlap is at worst a redundant idempotent re-index). A dir
	// event without a Create — a bare mtime bump on an existing dir —
	// needs no scan: entry changes inside it fire their own file events.
	// Either way the directory event itself reaches no indexer logic.
	if kind == ChangeCreated || kind == ChangeModified {
		if info, err := os.Stat(path); err == nil && info.IsDir() {
			if hasEventType(event.Types, fswatcher.EventCreate) {
				w.enqueueDirScan(path)
			}
			return
		}
	}

	// Only process files with a detectable language — an extension
	// the registry knows, or an unknown-extension script the shebang
	// fallback can place.
	if _, ok := w.indexer.effectiveLanguage(path, nil); !ok {
		// Still handle remove for previously indexed files.
		if kind != ChangeDeleted && kind != ChangeRenamed {
			return
		}
	}

	// Storm mode — if more than StormThreshold events arrived within
	// StormWindowMs, skip the per-file debounced path and accumulate
	// into a batch. The batch drains once StormQuietPeriodMs has
	// passed with no further events.
	if w.shouldEnterStorm() {
		w.recordInStorm(path, kind)
		return
	}

	w.scheduleFileMutation(path, kind)
}

// EnqueueFileMutation hands a committed filesystem mutation directly to the
// watcher's debounced generation queue. It does not depend on fsnotify delivery,
// so daemon-backed MCP edits remain reliable even when native watch delivery is
// degraded. The request context governs admission only; the returned ticket
// resolves when this generation or a newer coalesced generation reaches a
// terminal graph state.
func (w *Watcher) EnqueueFileMutation(ctx context.Context, filePath string) (*MutationTicket, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	absPath, err := filepath.Abs(filePath)
	if err != nil {
		return nil, err
	}
	root := w.indexer.RootPath()
	if root == "" {
		return nil, nil
	}
	rel, err := filepath.Rel(root, absPath)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, nil
	}
	if w.isExcluded(absPath) {
		return nil, nil
	}
	if _, ok := w.indexer.effectiveLanguage(absPath, nil); !ok {
		return nil, nil
	}
	select {
	case <-w.done:
		return nil, errWatcherStopped
	default:
	}
	if w.mutationBeforeAdmission != nil {
		w.mutationBeforeAdmission()
	}
	ticket := w.scheduleFileMutation(absPath, ChangeModified)
	if ticket == nil {
		return nil, errWatcherStopped
	}
	return ticket, nil
}

// scheduleFileMutation is the single admission point for native events and
// direct daemon mutations. A later admission supersedes every queued callback
// for the same path; every earlier ticket stays attached until the newest patch
// produces a terminal graph event.
func (w *Watcher) scheduleFileMutation(path string, kind ChangeKind) *MutationTicket {
	w.mu.Lock()
	if w.stopping {
		w.mu.Unlock()
		return nil
	}
	if timer, exists := w.pending[path]; exists {
		if timer.Stop() {
			w.asyncWork.Done()
		}
	}
	w.nextGeneration++
	generation := w.nextGeneration
	if w.pendingGeneration == nil {
		w.pendingGeneration = make(map[string]uint64)
	}
	if w.mutationWaiters == nil {
		w.mutationWaiters = make(map[string]map[uint64]chan MutationResult)
	}
	if w.mutationWaiters[path] == nil {
		w.mutationWaiters[path] = make(map[uint64]chan MutationResult)
	}
	done := make(chan MutationResult, 1)
	w.mutationWaiters[path][generation] = done
	w.pendingGeneration[path] = generation
	debounce := time.Duration(w.config.DebounceMs) * time.Millisecond
	var timer *time.Timer
	w.asyncWork.Add(1)
	timer = time.AfterFunc(debounce, func() {
		defer w.asyncWork.Done()
		// Timer.Stop can lose a race with a callback that is already queued.
		// Only the newest timer may consume the pending entry.
		if !w.claimPendingTimer(path, &timer) {
			return
		}
		if w.pointMutationClaimed != nil {
			w.pointMutationClaimed(path)
		}
		patchErr := errMutationPatchAborted
		superseded := false
		defer w.guardWatcherPanic("patch " + path)
		defer func() {
			if !superseded {
				w.completeMutationWaiters(path, generation, patchErr)
			}
		}()
		patchErr = w.patchGraph(path, kind, generation)
		if w.mutationAdmissionStopped() {
			patchErr = errWatcherStopped
		} else if w.mutationGenerationSuperseded(path, generation) {
			patchErr = errMutationSuperseded
			superseded = true
			return
		}
	})
	w.pending[path] = timer
	w.mu.Unlock()
	return &MutationTicket{Path: path, Generation: generation, Done: done}
}

func (w *Watcher) mutationAdmissionStopped() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.stopping
}

func (w *Watcher) completeMutationWaiters(path string, appliedGeneration uint64, err error) {
	type completion struct {
		requested uint64
		done      chan MutationResult
	}
	w.mu.Lock()
	var completions []completion
	for generation, done := range w.mutationWaiters[path] {
		if generation <= appliedGeneration {
			completions = append(completions, completion{requested: generation, done: done})
			delete(w.mutationWaiters[path], generation)
		}
	}
	if len(w.mutationWaiters[path]) == 0 {
		delete(w.mutationWaiters, path)
	}
	w.mu.Unlock()

	for _, completion := range completions {
		completion.done <- MutationResult{
			RequestedGeneration: completion.requested,
			AppliedGeneration:   appliedGeneration,
			Reindexed:           err == nil,
			Err:                 err,
		}
		close(completion.done)
	}
}

func (w *Watcher) failMutationWaiters(err error) {
	w.mu.Lock()
	waiters := w.mutationWaiters
	w.mutationWaiters = nil
	w.mu.Unlock()
	for _, byGeneration := range waiters {
		for generation, done := range byGeneration {
			done <- MutationResult{RequestedGeneration: generation, Err: err}
			close(done)
		}
	}
}

func (w *Watcher) mutationGenerationSuperseded(path string, generation uint64) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	current, pending := w.pendingGeneration[path]
	return pending && current != generation
}

// claimPendingTimer atomically consumes timer only when it is still the
// newest debounce timer for path. time.Timer.Stop may return false after a
// callback has been queued; without this identity check that stale callback
// can delete a newer timer's pending entry and let later saves fan out into
// redundant concurrent callbacks.
func (w *Watcher) claimPendingTimer(path string, timerRef **time.Timer) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.pending[path] != *timerRef {
		return false
	}
	delete(w.pending, path)
	return true
}

// shouldEnterStorm records the current event in the rate window and
// reports whether the watcher is over threshold. Returns false when
// storm mode is disabled (threshold <= 0). The returned-true path
// guarantees the caller will enqueue to the batch, so any single
// event that crosses the threshold is captured correctly.
func (w *Watcher) shouldEnterStorm() bool {
	if w.config.StormThreshold <= 0 {
		return false
	}
	now := time.Now()
	window := time.Duration(w.config.StormWindowMs) * time.Millisecond
	cutoff := now.Add(-window)

	w.stormMu.Lock()
	defer w.stormMu.Unlock()
	if w.stormStopped {
		return false
	}
	// Already batching — stay in storm until the drain completes.
	if w.stormActive {
		return true
	}
	// Drop timestamps older than the window. The slice is append-only
	// so a linear scan from the front is the minimal thing that
	// works; the window is O(threshold) bounded in steady state.
	trimFrom := 0
	for i, t := range w.eventTimes {
		if t.After(cutoff) {
			trimFrom = i
			break
		}
		trimFrom = i + 1
	}
	if trimFrom > 0 {
		w.eventTimes = w.eventTimes[trimFrom:]
	}
	w.eventTimes = append(w.eventTimes, now)
	return len(w.eventTimes) > w.config.StormThreshold
}

// recordInStorm adds the event to the pending batch and resets the
// drain timer. Repeated create/modify collapse to a single patch; a
// later delete of the same path overwrites an earlier create so the
// drain does the right final thing (treats the path as deleted).
func (w *Watcher) recordInStorm(path string, kind ChangeKind) {
	w.stormMu.Lock()
	defer w.stormMu.Unlock()
	if w.stormStopped {
		return
	}
	w.stormActive = true
	// Cancel any pending per-file timers for this path — storm mode
	// takes over.
	w.mu.Lock()
	if timer, exists := w.pending[path]; exists {
		if timer.Stop() {
			w.asyncWork.Done()
		}
		delete(w.pending, path)
		// The stopped callback can no longer complete its tickets: its
		// claimPendingTimer identity check will fail after the delete above.
		// Transfer the newest generation to the batch; completing through it
		// also completes every earlier coalesced waiter for this path.
		if generation := w.pendingGeneration[path]; generation != 0 {
			if w.stormGenerations == nil {
				w.stormGenerations = make(map[string]uint64)
			}
			w.stormGenerations[path] = generation
		}
	}
	w.mu.Unlock()
	w.stormBatch[path] = kind

	quiet := time.Duration(w.config.StormQuietPeriodMs) * time.Millisecond
	w.stopStormTimerLocked()
	w.stormWork.Add(1)
	w.stormTimer = time.AfterFunc(quiet, func() {
		defer w.stormWork.Done()
		w.drainStorm()
	})
}

// stopStormTimerLocked cancels the currently published quiet timer. A timer
// owns one stormWork token from publication until either its callback returns
// or Stop succeeds here; Timer.Stop false means the callback alone must Done.
// stormMu must be held, which also prevents Add racing a shutdown Wait.
func (w *Watcher) stopStormTimerLocked() {
	if w.stormTimer == nil {
		return
	}
	if w.stormTimer.Stop() {
		w.stormWork.Done()
	}
	w.stormTimer = nil
}

// drainStorm processes every accumulated path through one bounded multi-file
// parse/evict pipeline. The batch runner performs one receipt-scoped resolver
// pass and one derived catch-up; no per-file transaction or graph-wide tail is
// paid for a checkout-sized event burst.
func (w *Watcher) drainStorm() {
	defer w.guardWatcherPanic("storm-drain")
	w.stormMu.Lock()
	stopped := w.stormStopped
	batch := w.stormBatch
	generations := w.stormGenerations
	w.stormBatch = make(map[string]ChangeKind)
	w.stormGenerations = make(map[string]uint64)
	w.eventTimes = nil
	w.stormActive = false
	drained := w.stormDrained
	w.stormMu.Unlock()

	if stopped {
		w.completeStormMutationWaiters(generations, nil, errWatcherStopped)
		return
	}
	if len(batch) == 0 {
		w.completeStormMutationWaiters(generations, nil, errMutationPatchAborted)
		return
	}
	// A pending point patch may already have left the debounce queue when storm
	// mode took over, and the adaptive poller enters patchGraph directly. Keep
	// the batch's snapshot/mutation boundary serialized with both producers.
	if w.stormBeforeLock != nil {
		w.stormBeforeLock()
	}
	w.patchMu.Lock()
	defer w.patchMu.Unlock()
	var result *IndexResult
	completionErr := errMutationPatchAborted
	defer func() {
		w.completeStormMutationWaiters(generations, result, completionErr)
	}()
	paths := make([]string, 0, len(batch))
	for path := range batch {
		paths = append(paths, path)
	}
	sort.Strings(paths)

	start := time.Now()
	w.logger.Info("watcher: storm drain starting", zap.Int("paths", len(paths)))
	finishTopologyMutation := reach.BeginTopologyMutation(w.indexer.graph)
	mutationFinished := false
	defer func() {
		if !mutationFinished {
			// The batch is non-empty and may have partially applied before a
			// panic. Invalidate conservatively before unblocking readers.
			finishTopologyMutation(true)
		}
	}()

	var err error
	result, err = w.reindexStormPaths(paths)
	completionErr = err
	// A batch-level error can arrive after sibling files committed. Invalidate
	// conservatively and report it without retrying every path one-by-one.
	finishTopologyMutation(true)
	mutationFinished = true

	reindexed, deleted, failed := 0, 0, 0
	if result != nil {
		reindexed = result.StaleFileCount
		deleted = result.DeletedFileCount
		failed = len(result.FailedFiles)
	}
	if err != nil {
		w.logger.Warn("watcher: storm drain failed",
			zap.Int("paths", len(paths)), zap.Error(err))
	}
	w.logger.Info("watcher: storm drain complete",
		zap.Int("paths", len(paths)),
		zap.Int("reindexed", reindexed),
		zap.Int("deleted", deleted),
		zap.Int("failed", failed),
		zap.Duration("elapsed", time.Since(start)))
	if drained != nil {
		drained(len(batch))
	}
}

// completeStormMutationWaiters publishes a storm batch's terminal outcome to
// every direct-mutation ticket whose debounce timer the batch adopted. Waiters
// are detached in one critical section and notified after the lock is released,
// so a thousand-path storm does not pay one lock handoff per ticket and Stop can
// never race this path into a double close.
func (w *Watcher) completeStormMutationWaiters(
	generations map[string]uint64,
	result *IndexResult,
	batchErr error,
) {
	if len(generations) == 0 {
		return
	}
	failed := make(map[string]struct{})
	if result != nil {
		failed = make(map[string]struct{}, len(result.FailedFiles))
		for _, path := range result.FailedFiles {
			failed[filepath.Clean(path)] = struct{}{}
		}
	}
	type completion struct {
		done   chan MutationResult
		result MutationResult
	}
	completions := make([]completion, 0, len(generations))

	w.mu.Lock()
	for path, appliedGeneration := range generations {
		pathErr := batchErr
		if pathErr == nil && result == nil {
			pathErr = errors.New("storm mutation batch completed without a result")
		}
		if pathErr == nil {
			if _, pathFailed := failed[filepath.Clean(path)]; pathFailed {
				pathErr = fmt.Errorf("storm mutation batch failed to index %s", path)
			}
		}
		if w.pendingGeneration[path] == appliedGeneration {
			delete(w.pendingGeneration, path)
		}
		for requestedGeneration, done := range w.mutationWaiters[path] {
			if requestedGeneration > appliedGeneration {
				continue
			}
			completions = append(completions, completion{
				done: done,
				result: MutationResult{
					RequestedGeneration: requestedGeneration,
					AppliedGeneration:   appliedGeneration,
					Reindexed:           pathErr == nil,
					Err:                 pathErr,
				},
			})
			delete(w.mutationWaiters[path], requestedGeneration)
		}
		if len(w.mutationWaiters[path]) == 0 {
			delete(w.mutationWaiters, path)
		}
	}
	w.mu.Unlock()

	for _, completion := range completions {
		completion.done <- completion.result
		close(completion.done)
	}
}

// patchGraphNoResolve is retained as a compatibility seam for focused watcher
// tests and older in-package callers. It no longer performs point mutations:
// even a single path goes through the same batch runner used by drainStorm.
func (w *Watcher) patchGraphNoResolve(path string, kind ChangeKind) {
	kind = w.reconcileKindWithDisk(path, kind)
	_ = kind // disk truth is consumed by IncrementalReindexPaths.
	if _, err := w.reindexStormPaths([]string{path}); err != nil {
		w.logger.Warn("storm: batched index failed",
			zap.String("path", path), zap.Error(err))
	}
}

// reconcileKindWithDisk corrects an event kind against the file's actual
// on-disk state at patch time. FSEvents accumulates flags per path, so an
// atomic replace — git checkout writing a temp file and renaming it over the
// target, or an unlink + recreate — surfaces with ItemRemoved / ItemRenamed
// set even though a file is right back at the same path with a new inode.
// pickKind ranks Remove and Rename above Modify, so it classifies that replace
// as a deletion; the delete branch then EvictFiles the definition and stubs
// its cross-file callers with nothing to rebind them, and find_usages goes
// silently to zero. By the time the debounced patch runs the filesystem has
// settled, so the path's existence is authoritative: a delete/rename whose
// path is still a regular file is really a modify (re-parse + rebind incoming
// refs), and a create/modify whose path has vanished is really a delete. An
// in-place write (same inode) is already a bare Modify, so it is unaffected.
func (w *Watcher) reconcileKindWithDisk(path string, kind ChangeKind) ChangeKind {
	info, err := os.Stat(path)
	exists := err == nil && info.Mode().IsRegular()
	switch kind {
	case ChangeDeleted, ChangeRenamed:
		if exists {
			return ChangeModified
		}
	case ChangeCreated, ChangeModified:
		if !exists {
			return ChangeDeleted
		}
	}
	return kind
}

// forgetDeletedFileMtime drops a just-evicted file's recorded mtime from
// both the in-memory map and the store's FileMtime sidecar. EvictFile
// removes the file's nodes but leaves its mtime behind, so without this the
// persisted mtime row outlives the file: the next warm restart reads it
// back, finds the path gone from disk, and treats it as a phantom deletion
// — re-running a scoped reconcile for a file that is already correct on
// every boot. Mirrors IncrementalReindex's deletion handling: prune the
// in-memory map first (pruneDeletedFileMtimes documents that its caller has
// already done so, and a later snapshot persist would otherwise resurrect
// the row from the stale in-memory entry), then the store, which self-skips
// on a backend without the FileMtimeDeleter capability. relPath must be the
// canonical relKey the mtime map and store are keyed on — the same key
// EvictFile evicted the file's nodes under.
func (w *Watcher) forgetDeletedFileMtime(relPath string) {
	w.indexer.mtimeMu.Lock()
	delete(w.indexer.fileMtimes, relPath)
	w.indexer.mtimeMu.Unlock()
	w.indexer.pruneDeletedFileMtimes([]string{relPath})
}

func (w *Watcher) generationCurrent(path string, generation uint64) bool {
	if generation == 0 {
		return true
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.pendingGeneration[path] == generation
}

func (w *Watcher) finishGeneration(path string, generation uint64) {
	if generation == 0 {
		return
	}
	w.mu.Lock()
	if w.pendingGeneration[path] == generation {
		delete(w.pendingGeneration, path)
	}
	w.mu.Unlock()
}

func (w *Watcher) patchGraph(path string, kind ChangeKind, generations ...uint64) error {
	var generation uint64
	if len(generations) > 0 {
		generation = generations[0]
	}
	return w.patchGraphWithReceiptState(path, kind, generation, false)
}

// patchGraphAfterReceiptCheck is used only by the Darwin startup barrier,
// whose set-oriented receipt pass has already decided that this path is not an
// unchanged replay. It prevents ChangeModified from repeating that SQL lookup
// one path at a time inside the ordinary point-mutation path.
func (w *Watcher) patchGraphAfterReceiptCheck(path string, kind ChangeKind) error {
	return w.patchGraphWithReceiptState(path, kind, 0, true)
}

func (w *Watcher) patchGraphWithReceiptState(path string, kind ChangeKind, generation uint64, receiptChecked bool) error {
	if !w.generationCurrent(path, generation) {
		return errMutationSuperseded
	}
	defer w.finishGeneration(path, generation)
	w.patchMu.Lock()
	defer w.patchMu.Unlock()
	if !w.generationCurrent(path, generation) {
		return errMutationSuperseded
	}
	// A replace/revert (rename-over or unlink+recreate) reaches us as a
	// delete/rename even though a file is right back at the same path;
	// reconcile against disk so it takes the parse-then-swap + incoming-
	// rebind modify path instead of a hard evict that would silently zero
	// the definition's callers. A vanished create/modify becomes a delete.
	kind = w.reconcileKindWithDisk(path, kind)
	start := time.Now()
	classification := "structural"
	var nodesAdded, nodesRemoved, edgesAdded, edgesRemoved int
	var finishTopologyMutation func(bool)
	topologyChanged := false
	// Keep a panic/error-safe release so no lookup can remain parked behind the
	// reach topology gate if an incremental path exits early.
	defer func() {
		if finishTopologyMutation != nil {
			finishTopologyMutation(topologyChanged)
		}
	}()
	beginTopologyMutation := func() {
		finishTopologyMutation = reach.BeginTopologyMutation(w.indexer.graph)
		// Conservatively invalidate after every structural reindex attempt. Most
		// parse failures leave the old graph untouched, but treating that as a
		// cache miss is safer than trusting count-based telemetry to prove no
		// resolver edge was retargeted.
		topologyChanged = true
	}
	endTopologyMutation := func() {
		if finishTopologyMutation == nil {
			return
		}
		finishTopologyMutation(topologyChanged)
		finishTopologyMutation = nil
	}

	// Two keys for this file. relPath (RelKey: slash form + NFC) is the
	// mtime-forget / change-callback key. graphKey (graphRelKey:
	// OS-native separators + NFC, repo-prefixed) is what the graph
	// stores the file's nodes under, so the GetFileNodes /
	// snapshotSymbols lookups below MUST use it — a slash-form key
	// misses the backslash-keyed nodes on Windows and would report every
	// symbol as added/removed. Both fold an NFD (macOS) event path to
	// NFC so a non-ASCII name still hits the indexed node. On POSIX the
	// two keys coincide.
	relPath := path
	graphKey := path
	if w.indexer.rootPath != "" {
		relPath = w.indexer.RelKey(path)
		graphKey = w.indexer.prefixPath(w.indexer.graphRelKey(path))
	}

	switch kind {
	case ChangeCreated:
		beginTopologyMutation()
		if err := w.indexer.IndexFile(path); err != nil {
			w.logger.Warn("index file failed", zap.String("path", path), zap.Error(err))
			return err
		}
		newSymbols := w.indexer.graph.GetFileNodes(graphKey)
		nodesAdded = len(newSymbols)
		edgesAdded = w.countFileEdges(newSymbols)
		endTopologyMutation()

		// Notify callback: no old symbols, only new symbols.
		w.symbolChangeCbMu.RLock()
		cb := w.symbolChangeCb
		w.symbolChangeCbMu.RUnlock()
		if cb != nil {
			cb(relPath, nil, newSymbols)
		}

	case ChangeModified:
		// Native backends and the adaptive poller deliberately overlap. A late
		// startup replay or duplicate report can therefore arrive after the file
		// is already committed. Mtime is only the fast receipt gate: equal values
		// are confirmed against the persisted content hash, so a timestamp-
		// preserving real edit still enters the normal mutation lifecycle.
		if !receiptChecked && w.indexer.fileMtimeMatches(path) {
			return nil
		}
		// Read the prior file state once. It supplies the callback snapshot,
		// gross change telemetry and the durable raw-extraction fingerprint;
		// repeating GetFileNodes here is particularly costly when SQLite is
		// under analysis load.
		snapshotStarted := time.Now()
		priorNodes := w.indexer.graph.GetFileNodes(graphKey)
		oldSymbols := make([]*graph.Node, 0, len(priorNodes))
		for _, n := range priorNodes {
			if n == nil || n.Kind == graph.KindFile || n.Kind == graph.KindImport {
				continue
			}
			cp := &graph.Node{ID: n.ID, Kind: n.Kind, Name: n.Name, QualName: n.QualName, FilePath: n.FilePath}
			if sig, ok := n.Meta["signature"]; ok {
				cp.Meta = map[string]any{"signature": sig}
			}
			oldSymbols = append(oldSymbols, cp)
		}
		snapshotDuration := time.Since(snapshotStarted)

		// Parse once and compare the complete post-coverage extraction, not
		// only declarations. A changed call target, doc, TODO, location,
		// metadata field or edge changes this fingerprint and must patch the
		// graph. Only a byte-for-byte-equivalent graph output is inert. The
		// prepared extraction is consumed by IndexFile below, avoiding the old
		// double parse on structural edits.
		probe, probeOK := w.indexer.prepareFileDelta(path)
		if !w.generationCurrent(path, generation) {
			w.indexer.discardPreparedExtraction(path)
			return errMutationSuperseded
		}
		stored := storedExtractionGraphFingerprints(priorNodes)
		classification := "structural"
		if probeOK && stored.semantic != "" {
			switch {
			case probe.fingerprints.semantic == stored.semantic && probe.fingerprints.metadata == stored.metadata:
				w.indexer.discardPreparedExtraction(path)
				w.logger.Info("watcher: inert delta phases",
					zap.String("file", path), zap.String("delta_class", "inert"),
					zap.Duration("snapshot", snapshotDuration), zap.Duration("read", probe.read),
					zap.Duration("extract", probe.extract), zap.Duration("coverage", probe.coverage),
					zap.Duration("fingerprint", probe.fingerprintTime))
				fresh := w.recordInertModify(path, relPath, oldSymbols, start, probe.readVersion)
				if !fresh {
					return errFileVersionChanged
				}
				return nil
			case probe.fingerprints.semantic == stored.semantic:
				var refreshed []*graph.Node
				var applied, fresh bool
				if generation == 0 {
					refreshed, applied, fresh = w.indexer.applyPreparedMetadataRefresh(path, priorNodes)
				} else {
					// Serialise generation validation with the bounded commit. A newer
					// event cannot register between the byte check and fingerprint/mtime write.
					w.mu.Lock()
					if w.pendingGeneration[path] == generation {
						refreshed, applied, fresh = w.indexer.applyPreparedMetadataRefresh(path, priorNodes)
					}
					w.mu.Unlock()
				}
				if applied {
					w.logger.Info("watcher: metadata-only delta phases",
						zap.String("file", path), zap.String("delta_class", "metadata_only"),
						zap.Duration("snapshot", snapshotDuration), zap.Duration("read", probe.read),
						zap.Duration("extract", probe.extract), zap.Duration("coverage", probe.coverage),
						zap.Duration("fingerprint", probe.fingerprintTime))
					w.recordMetadataModify(path, relPath, oldSymbols, refreshed, start)
					if !fresh {
						return errFileVersionChanged
					}
					return nil
				}
			case stored.core != "" && probe.fingerprints.core == stored.core:
				classification = "artifact_only"
			}
		}

		// Do NOT pre-evict. IndexFile parse-then-swaps internally and consumes
		// the prepared extraction when its transformed bytes still match.
		reindexStarted := time.Now()
		fileEdgesBefore := w.countFileEdges(priorNodes)
		resolvedBefore := w.countResolvedFileEdges(priorNodes)
		incomingBeforeByID := w.resolvedIncomingByNode(priorNodes)
		beginTopologyMutation()
		if err := w.indexer.IndexFile(path); err != nil {
			w.logger.Warn("reindex file failed", zap.String("path", path), zap.Error(err))
			return err
		}
		nodesRemoved = len(priorNodes)
		newSymbols := w.indexer.graph.GetFileNodes(graphKey)
		nodesAdded = len(newSymbols)
		// Edge churn scoped to this file's nodes. A graph-wide
		// EdgeCount delta would also pick up edges landed by whatever
		// else mutates the graph during this patch (concurrent
		// reconciles, deferred passes), which made the edges+ figure
		// meaningless noise on a busy daemon.
		if fileEdgesAfter := w.countFileEdges(newSymbols); fileEdgesAfter >= fileEdgesBefore {
			edgesAdded = fileEdgesAfter - fileEdgesBefore
		} else {
			edgesRemoved = fileEdgesBefore - fileEdgesAfter
		}
		// Shape-degradation guard: a modify that kept its symbols but lost
		// most of its resolved edges is a transient resolution failure, not a
		// real deletion — flag it and enqueue a forced scoped re-resolve so it
		// self-heals instead of persisting the degraded shape.
		incomingBefore, incomingAfter := w.incomingRegressionForSurvivors(incomingBeforeByID, newSymbols)
		w.guardResolvedEdgeRegression(path, len(priorNodes), len(newSymbols), resolvedBefore, w.countResolvedFileEdges(newSymbols), incomingBefore, incomingAfter)
		endTopologyMutation()
		w.logger.Info("watcher: structural delta phases",
			zap.String("file", path),
			zap.String("delta_class", classification),
			zap.Duration("snapshot", snapshotDuration),
			zap.Duration("read", probe.read),
			zap.Duration("extract", probe.extract),
			zap.Duration("coverage", probe.coverage),
			zap.Duration("fingerprint", probe.fingerprintTime),
			zap.Duration("reindex", time.Since(reindexStarted)),
			zap.Bool("probe_complete", probeOK),
			zap.Bool("clone_pending", w.indexer.CloneIndexPending()))

		// Notify callback with old and new symbols.
		w.symbolChangeCbMu.RLock()
		cb := w.symbolChangeCb
		w.symbolChangeCbMu.RUnlock()
		if cb != nil {
			cb(relPath, oldSymbols, newSymbols)
		}

	case ChangeDeleted, ChangeRenamed:
		// Snapshot old symbols before eviction.
		oldSymbols := w.snapshotSymbols(graphKey)

		beginTopologyMutation()
		nr, er := w.indexer.EvictFile(path)
		nodesRemoved = nr
		edgesRemoved = er
		topologyChanged = nr > 0 || er > 0
		endTopologyMutation()

		// The file is genuinely gone from disk here — reconcileKindWithDisk
		// already downgraded a replace/revert (path still present) to
		// ChangeModified. Drop its now-orphaned mtime so a warm restart does
		// not re-discover the vanished path as a phantom deletion. relPath is
		// the canonical relKey EvictFile evicted under.
		w.forgetDeletedFileMtime(relPath)
		// fsnotify and the adaptive poller are deliberately redundant. They
		// may both report the same deletion, but only the producer that
		// actually removed graph topology represents a semantic change. Do
		// not publish a second empty callback/history/event: consumers would
		// otherwise lose the pre-delete symbol snapshot or repeat downstream
		// invalidation work. EvictFile's zero delta is authoritative even for
		// source files that contain no symbols of their own.
		if nr == 0 && er == 0 {
			return nil
		}

		// Notify callback: old symbols removed, no new symbols.
		w.symbolChangeCbMu.RLock()
		cb := w.symbolChangeCb
		w.symbolChangeCbMu.RUnlock()
		if cb != nil {
			cb(relPath, oldSymbols, nil)
		}
	}

	ev := GraphChangeEvent{
		FilePath:       path,
		Kind:           kind,
		Classification: classification,
		NodesAdded:     nodesAdded,
		NodesRemoved:   nodesRemoved,
		EdgesAdded:     edgesAdded,
		EdgesRemoved:   edgesRemoved,
		Timestamp:      time.Now(),
		DurationMs:     time.Since(start).Milliseconds(),
	}

	// Reach invalidation is published by endTopologyMutation before any
	// callbacks or events can observe the new graph. The topology gate spans
	// IndexFile's parse-then-swap and nested resolver work without taking the
	// resolver's non-reentrant mutex twice; impact readers therefore see the
	// complete old graph or the complete new graph, never the eviction gap.

	w.historyMu.Lock()
	w.history = append(w.history, ev)
	if len(w.history) > maxHistory {
		w.history = w.history[len(w.history)-maxHistory:]
	}
	w.historyMu.Unlock()

	// Non-blocking send.
	select {
	case w.events <- ev:
	default:
	}

	w.logger.Info("graph patch",
		zap.String("kind", string(kind)),
		zap.String("file", path),
		zap.Int("nodes+", nodesAdded),
		zap.Int("nodes-", nodesRemoved),
		zap.Int("edges+", edgesAdded),
		zap.Int("edges-", edgesRemoved),
		zap.Int64("ms", ev.DurationMs),
	)
	return nil
}

// countFileEdges counts the edges incident to the given file nodes:
// every out-edge plus the in-edges that originate outside the file
// (an intra-file edge is already counted on its From side). Batched
// so a disk backend pays two bulk lookups instead of 2N point queries.
func (w *Watcher) countFileEdges(nodes []*graph.Node) int {
	if len(nodes) == 0 {
		return 0
	}
	ids := make([]string, 0, len(nodes))
	inFile := make(map[string]struct{}, len(nodes))
	for _, n := range nodes {
		ids = append(ids, n.ID)
		inFile[n.ID] = struct{}{}
	}
	total := 0
	for _, edges := range w.indexer.graph.GetOutEdgesByNodeIDs(ids) {
		total += len(edges)
	}
	for _, edges := range w.indexer.graph.GetInEdgesByNodeIDs(ids) {
		for _, e := range edges {
			if _, ok := inFile[e.From]; !ok {
				total++
			}
		}
	}
	return total
}

// resolvedEdgeRegressionFloor is the minimum pre-patch resolved-edge count
// below which the shape-degradation guard stays quiet — a 1→0 or 3→1 file is
// noise, not a resolution collapse worth self-healing.
const resolvedEdgeRegressionFloor = 4

// countResolvedFileEdges counts this file's OUTGOING edges whose target is a
// concrete (resolved) node — an edge pointing at an `unresolved::` stub does
// not count. Restricted to out-edges: an incoming edge's resolution state is
// owned by the OTHER file, not this one. This is the signal countFileEdges
// cannot give: an edge demoted from a resolved target to a stub keeps the total
// incident-edge count identical while losing a resolution.
func (w *Watcher) countResolvedFileEdges(nodes []*graph.Node) int {
	if len(nodes) == 0 {
		return 0
	}
	ids := make([]string, 0, len(nodes))
	for _, n := range nodes {
		ids = append(ids, n.ID)
	}
	total := 0
	for _, edges := range w.indexer.graph.GetOutEdgesByNodeIDs(ids) {
		for _, e := range edges {
			if e != nil && !graph.IsUnresolvedTarget(e.To) {
				total++
			}
		}
	}
	return total
}

// guardResolvedEdgeRegression fires when a modify patch lost most of its
// resolved edges on either side of the graph: the file's own out-edges dropped
// while it kept its symbols, OR a surviving definition lost most of the callers
// bound into it. Both are transient resolution failures (a dependency mid-write,
// an LSP provider not yet warm, an external revert re-parsing a definition
// file), not real deletions. It logs loudly, bumps the process-global
// regression counter, and enqueues the file for a forced scoped re-resolve so
// the graph self-heals instead of persisting the degraded shape.
func (w *Watcher) guardResolvedEdgeRegression(path string, nodesBefore, nodesAfter, resolvedBefore, resolvedAfter, incomingBefore, incomingAfter int) {
	// Out-edge regression: this file's own references lost their resolutions
	// while it kept (or grew) its symbols. Gated on nodesAfter >= nodesBefore —
	// a symbol removal legitimately drops out-edges.
	outRegressed := resolvedBefore >= resolvedEdgeRegressionFloor &&
		nodesAfter >= nodesBefore &&
		resolvedAfter*2 < resolvedBefore
	// Incoming-edge regression: a definition that SURVIVED the re-parse lost
	// most of the callers bound INTO it. The caller restricts the counts to
	// surviving definitions, so a genuinely deleted symbol's lost callers do
	// not read as a loss — which is why this arm is deliberately NOT gated on
	// nodesAfter >= nodesBefore. An external revert removes the appended probe
	// symbol (node count drops) yet the definition it leaves behind must keep
	// its incoming resolved edges; their loss is a resolution failure, not a
	// deletion, and without this arm the revert never self-heals the way the
	// symmetric add does.
	inRegressed := incomingBefore >= resolvedEdgeRegressionFloor &&
		incomingAfter*2 < incomingBefore
	if !outRegressed && !inRegressed {
		return
	}
	RecordResolutionRegression()
	if w.logger != nil {
		w.logger.Warn("watcher: resolved-edge regression — file kept its symbols but lost most resolved edges; enqueuing forced scoped re-resolve",
			zap.String("file", path),
			zap.Int("nodes_before", nodesBefore),
			zap.Int("nodes_after", nodesAfter),
			zap.Int("resolved_edges_before", resolvedBefore),
			zap.Int("resolved_edges_after", resolvedAfter),
			zap.Int("incoming_resolved_before", incomingBefore),
			zap.Int("incoming_resolved_after", incomingAfter))
	}
	w.enqueueReresolve(path)
}

// resolvedIncomingByNode returns, per referenceable definition node in `nodes`,
// the count of resolvable reference edges currently bound INTO it (callers,
// type/field references, …). countResolvedFileEdges is out-edge-only and so is
// structurally blind to a definition losing its incoming callers on a re-parse
// — this is the signal that catches an external revert zeroing a surviving
// symbol's usages.
func (w *Watcher) resolvedIncomingByNode(nodes []*graph.Node) map[string]int {
	if len(nodes) == 0 {
		return nil
	}
	ids := make([]string, 0, len(nodes))
	for _, n := range nodes {
		if n != nil && graph.IsReferenceableSymbol(n.Kind) {
			ids = append(ids, n.ID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	byNode := make(map[string]int, len(ids))
	for id, edges := range w.indexer.graph.GetInEdgesByNodeIDs(ids) {
		c := 0
		for _, e := range edges {
			if e != nil && graph.IsResolvableRefEdge(e.Kind) {
				c++
			}
		}
		if c > 0 {
			byNode[id] = c
		}
	}
	return byNode
}

// incomingRegressionForSurvivors sums the incoming resolved-edge counts, before
// and after the re-parse, over only the definitions that SURVIVED it (present
// in both snapshots by node ID). Restricting to survivors keeps a genuinely
// deleted symbol's lost callers from reading as a regression, so the guard's
// incoming arm fires only when a symbol that is still defined lost the callers
// bound into it. `before` is the resolvedIncomingByNode snapshot captured on
// the pre-re-parse nodes; `after` is the file's fresh node set.
func (w *Watcher) incomingRegressionForSurvivors(before map[string]int, after []*graph.Node) (sumBefore, sumAfter int) {
	if len(before) == 0 || len(after) == 0 {
		return 0, 0
	}
	afterByID := w.resolvedIncomingByNode(after)
	for _, n := range after {
		if n == nil {
			continue
		}
		b, ok := before[n.ID]
		if !ok {
			continue
		}
		sumBefore += b
		sumAfter += afterByID[n.ID]
	}
	return sumBefore, sumAfter
}

// enqueueReresolve batches shape-degraded files for a forced scoped re-resolve.
// Copies enqueueDirScan's coalescing drainer so a save-storm of degraded files
// runs at most one drainer goroutine and only ever O(file) scoped re-resolves
// (never a whole-graph pass). reresolveFn is a test seam.
func (w *Watcher) enqueueReresolve(path string) {
	if !w.beginReconcileWork() {
		return
	}
	if w.pendingReresolve == nil {
		w.pendingReresolve = make(map[string]struct{})
	}
	w.pendingReresolve[path] = struct{}{}
	if w.reresolveActive {
		w.reconcileMu.Unlock()
		w.asyncWork.Done()
		return
	}
	w.reresolveActive = true
	w.reconcileMu.Unlock()

	go func() {
		defer w.asyncWork.Done()
		for {
			w.reconcileMu.Lock()
			files := w.pendingReresolve
			w.pendingReresolve = nil
			if len(files) == 0 {
				w.reresolveActive = false
				w.reconcileMu.Unlock()
				return
			}
			fn := w.reresolveFn
			w.reconcileMu.Unlock()
			func() {
				defer w.guardWatcherPanic("reresolve")
				if fn != nil {
					fn(files)
					return
				}
				for p := range files {
					if err := w.indexer.ReresolveFileScoped(p); err != nil && w.logger != nil {
						w.logger.Warn("watcher: forced scoped re-resolve failed",
							zap.String("file", p), zap.Error(err))
					}
				}
			}()
		}
	}()
}

// recordInertModify finishes a ChangeModified patch that the
// content-aware skip proved structurally inert. The graph already
// holds the correct symbols, so the destructive evict + reindex is
// skipped; this records the bookkeeping the skipped path would
// otherwise have produced:
//
//   - the indexer's recorded mtime is restamped so the adaptive
//     poller's mtime sweep does not keep re-flagging the file;
//   - a zero-delta GraphChangeEvent is appended to history and
//     published, so get_recent_changes still shows the save (with
//     all node/edge counts zero — nothing structural moved);
//   - the symbol-change callback fires with the unchanged symbol set
//     on both sides, mirroring the no-op so consumers see a
//     consistent before == after.
//
// The reachability index is intentionally not rebuilt — the topology
// did not change, so the existing reach stamps stay valid.
func (w *Watcher) recordInertModify(path, relPath string, symbols []*graph.Node, start time.Time, version fileReadVersion) bool {
	// Claim only the exact byte version whose fingerprints were compared. A
	// later write must stay dirty for the next poll/native event even though the
	// earlier, real save still owns this inert event and callback.
	fresh := w.indexer.recordFileReadVersion(relPath, path, version)
	w.recordNonStructuralModify(path, relPath, symbols, symbols, "inert", start)
	return fresh
}

func (w *Watcher) recordMetadataModify(path, relPath string, oldSymbols, refreshed []*graph.Node, start time.Time) {
	newSymbols := make([]*graph.Node, 0, len(refreshed))
	for _, node := range refreshed {
		if node == nil || node.Kind == graph.KindFile || node.Kind == graph.KindImport {
			continue
		}
		newSymbols = append(newSymbols, node)
	}
	w.recordNonStructuralModify(path, relPath, oldSymbols, newSymbols, "metadata_only", start)
}

func (w *Watcher) recordNonStructuralModify(path, relPath string, oldSymbols, newSymbols []*graph.Node, classification string, start time.Time) {
	ev := GraphChangeEvent{
		FilePath: path, Kind: ChangeModified, Classification: classification,
		Timestamp: time.Now(), DurationMs: time.Since(start).Milliseconds(),
	}
	w.historyMu.Lock()
	w.history = append(w.history, ev)
	if len(w.history) > maxHistory {
		w.history = w.history[len(w.history)-maxHistory:]
	}
	w.historyMu.Unlock()
	select {
	case w.events <- ev:
	default:
	}
	w.symbolChangeCbMu.RLock()
	cb := w.symbolChangeCb
	w.symbolChangeCbMu.RUnlock()
	if cb != nil {
		cb(relPath, oldSymbols, newSymbols)
	}
	w.logger.Info("graph patch: non-structural change",
		zap.String("file", path), zap.String("delta_class", classification), zap.Int64("ms", ev.DurationMs))
}

// snapshotSymbols returns a deep copy of the symbols for a file, preserving
// their signatures in Meta so they can be compared after re-indexing.
func (w *Watcher) snapshotSymbols(graphKey string) []*graph.Node {
	nodes := w.indexer.graph.GetFileNodes(graphKey)
	snapshot := make([]*graph.Node, 0, len(nodes))
	for _, n := range nodes {
		// Skip file and import nodes — we only track code symbols.
		if n.Kind == graph.KindFile || n.Kind == graph.KindImport {
			continue
		}
		cp := &graph.Node{
			ID:       n.ID,
			Kind:     n.Kind,
			Name:     n.Name,
			QualName: n.QualName,
			FilePath: n.FilePath,
		}
		if sig, ok := n.Meta["signature"]; ok {
			cp.Meta = map[string]any{"signature": sig}
		}
		snapshot = append(snapshot, cp)
	}
	return snapshot
}

// normalizeEventPath aligns an event path emitted by the OS-level
// backend with the form the indexer stored when it walked the tree.
//
// Two macOS-specific corrections are applied:
//
//   - /private/ symlink resolution: FSEvents reports paths under
//     /private/var/... and /private/tmp/... even when the watcher was
//     registered with /var/... or /tmp/... — those are real
//     /private/-rooted symlinks. The indexer keyed its symbols by the
//     user-facing form, so without this we'd fail to find any symbols
//     to evict on modify or delete.
//
//   - Unicode NFC folding: APFS / HFS+ hand back filenames in
//     decomposed NFD form, so a watcher event for a non-ASCII-named
//     file carries different bytes than the same file does in `git
//     diff` output or on a Linux checkout. Folding the path to NFC
//     here means every consumer downstream — the exclude matcher, the
//     storm batch, the per-file debounce map — sees one stable form.
//     IndexFile / EvictFile fold again at their own boundary, so this
//     is belt-and-braces, but it also keeps the debounce/batch maps
//     (keyed on this path directly) free of accidental NFD/NFC
//     duplicates for the same file.
func normalizeEventPath(path, rootPath string) string {
	path = pathkey.Normalize(path)
	if runtime.GOOS != "darwin" {
		return path
	}
	if !strings.HasPrefix(path, "/private/") {
		return path
	}
	// Without a rootPath we have no way to know which form (the
	// /private/-prefixed canonical or the symlink form) the rest of
	// the daemon expects, so leave it alone.
	if rootPath == "" || strings.HasPrefix(rootPath, "/private/") {
		return path
	}
	stripped := path[len("/private"):]
	if !strings.HasPrefix(stripped, rootPath) {
		// Different prefix entirely — leave the canonical form alone.
		return path
	}
	return stripped
}

// pickKind reduces the aggregated event-type set from fswatcher to a
// single ChangeKind. Priority: Remove > Rename > Modify > Create.
// Modify outranks Create because FSEvents flags are cumulative — a
// write to an existing file fires with both Create and Modify set,
// and treating that as "created" loses the old-symbols snapshot the
// modify path produces. An event with only types we don't act on
// (e.g. chmod alone) returns "".
func pickKind(types []fswatcher.EventType) ChangeKind {
	var hasCreate, hasModify, hasRemove, hasRename bool
	for _, t := range types {
		switch t {
		case fswatcher.EventCreate:
			hasCreate = true
		case fswatcher.EventMod:
			hasModify = true
		case fswatcher.EventRemove:
			hasRemove = true
		case fswatcher.EventRename:
			hasRename = true
		}
	}
	switch {
	case hasRemove:
		return ChangeDeleted
	case hasRename:
		return ChangeRenamed
	case hasModify:
		return ChangeModified
	case hasCreate:
		return ChangeCreated
	}
	return ""
}

// isExcluded reports whether path is excluded by the effective pattern list.
func (w *Watcher) isExcluded(path string) bool {
	root := w.indexer.rootPath
	if root == "" {
		return w.excludes.MatchRel(filepath.Base(path))
	}
	return w.excludes.MatchAbs(path, root)
}
