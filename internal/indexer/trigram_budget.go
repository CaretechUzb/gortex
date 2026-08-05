package indexer

import (
	"os"
	"strconv"
	"sync"
	"time"
)

// The trigram searcher is a lazily-built, in-memory inverted index over the
// FULL TEXT of every indexed file in one repo. It is built on the first
// text/regex search in that repo and, before this budget existed, was then
// held for the daemon's lifetime with no ceiling — and one exists per repo,
// so a daemon tracking twenty repos could accumulate twenty of them by doing
// nothing more than one text search in each. Measured on a live daemon a
// single repo's index retained ~190 MB and its build spiked the process
// footprint past 2.6 GB.
//
// It is a cache: every entry is reconstructible from files on disk, and the
// searcher is only a candidate filter — Grep re-reads and scans the files it
// selects, so a cold rebuild costs latency, never correctness. Caches in a
// long-lived process need a ceiling, so this bounds them two ways: drop what
// has gone unused for a while, and never hold more than a few at once.
const (
	// defaultTrigramIdleTTL is how long an unused searcher is kept. A repo
	// being actively grepped re-touches its entry on every query, so the TTL
	// only reclaims repos that have gone quiet.
	defaultTrigramIdleTTL = 10 * time.Minute
	// defaultTrigramMaxLive caps how many repos may hold a built searcher at
	// once. Set it to 0 (GORTEX_TRIGRAM_MAX_LIVE=0) to never build one: every
	// search then streams over the known file list instead.
	defaultTrigramMaxLive = 3
	// defaultTrigramMaxBytes caps the summed estimated heap of every live
	// searcher. A count cap alone does not bound memory — three indexes of an
	// arbitrarily large repo is still arbitrarily large — so the byte budget
	// is what makes the worst case a number instead of a hope. Override with
	// GORTEX_TRIGRAM_MAX_MB.
	defaultTrigramMaxBytes int64 = 256 << 20
)

// TrigramCacheStats is a point-in-time summary of the process-wide trigram
// searcher cache, for `gortex daemon status`. Without it the largest single
// in-memory search structure has no line in any status output, while the
// status line simultaneously advertises search as disk-resident.
type TrigramCacheStats struct {
	Live       int           `json:"live"`
	MaxLive    int           `json:"max_live"`
	Bytes      int64         `json:"bytes"`
	MaxBytes   int64         `json:"max_bytes"`
	IdleTTL    time.Duration `json:"idle_ttl"`
	BuildsOff  bool          `json:"builds_off"`
	Evictions  int64         `json:"evictions"`
	RepoCounts map[string]int64
}

// trigramBudget tracks which repos currently hold a built trigram searcher
// and evicts by idle time and by count. It stores release callbacks rather
// than the searchers themselves so the owning Indexer keeps sole ownership
// of its field and its own mutex discipline.
type trigramBudget struct {
	mu        sync.Mutex
	ttl       time.Duration
	maxLive   int
	entries   map[*Indexer]*trigramEntry
	now       func() time.Time // swappable in tests
	afterFunc func(time.Duration, func()) *time.Timer
	timer     *time.Timer
}

type trigramEntry struct {
	lastUsed time.Time
	release  func()
}

var processTrigramBudget = newTrigramBudget(trigramIdleTTLFromEnv(), trigramMaxLiveFromEnv())

func newTrigramBudget(ttl time.Duration, maxLive int) *trigramBudget {
	if ttl <= 0 {
		ttl = defaultTrigramIdleTTL
	}
	if maxLive <= 0 {
		maxLive = defaultTrigramMaxLive
	}
	return &trigramBudget{
		ttl:       ttl,
		maxLive:   maxLive,
		entries:   make(map[*Indexer]*trigramEntry),
		now:       time.Now,
		afterFunc: time.AfterFunc,
	}
}

// touch records that owner's searcher is live and just used, then evicts
// everything that has aged out and, if still over the cap, the
// least-recently-used entries until it fits. It also arms the budget's one
// reusable deadline timer, so a lone warm cache is reclaimed without waiting
// for another grep to enter this method.
//
// Release callbacks run after the budget's own lock is dropped: each one
// takes its Indexer's trigramMu, and holding both locks in one order here
// while a concurrent warm path takes them in the other order would be a
// deadlock waiting for load.
func (b *trigramBudget) touch(owner *Indexer, release func()) {
	if b == nil || owner == nil {
		return
	}
	var evictions []func()

	b.mu.Lock()
	now := b.now()
	if entry, ok := b.entries[owner]; ok {
		entry.lastUsed = now
		if release != nil {
			entry.release = release
		}
	} else if release != nil {
		b.entries[owner] = &trigramEntry{lastUsed: now, release: release}
	}

	for other, entry := range b.entries {
		if other == owner {
			continue
		}
		if !now.Before(entry.lastUsed.Add(b.ttl)) {
			evictions = append(evictions, entry.release)
			delete(b.entries, other)
		}
	}

	for len(b.entries) > b.maxLive {
		var oldest *Indexer
		var oldestAt time.Time
		for other, entry := range b.entries {
			if other == owner {
				// The caller is about to use this one; evicting it would
				// discard the build that is being paid for right now.
				continue
			}
			if oldest == nil || entry.lastUsed.Before(oldestAt) {
				oldest, oldestAt = other, entry.lastUsed
			}
		}
		if oldest == nil {
			break
		}
		evictions = append(evictions, b.entries[oldest].release)
		delete(b.entries, oldest)
	}
	b.scheduleExpiryLocked(now)
	b.mu.Unlock()

	runTrigramReleases(evictions)
}

// scheduleExpiryLocked maintains one timer for the earliest idle deadline.
// A callback that loses a Stop/Reset race always re-checks current deadlines,
// so it cannot evict an entry that was touched after the callback was queued.
func (b *trigramBudget) scheduleExpiryLocked(now time.Time) {
	if len(b.entries) == 0 || b.afterFunc == nil {
		if b.timer != nil {
			b.timer.Stop()
		}
		return
	}

	var next time.Time
	for _, entry := range b.entries {
		deadline := entry.lastUsed.Add(b.ttl)
		if next.IsZero() || deadline.Before(next) {
			next = deadline
		}
	}
	delay := next.Sub(now)
	if delay < 0 {
		delay = 0
	}
	if b.timer == nil {
		b.timer = b.afterFunc(delay, b.expireIdle)
		return
	}
	b.timer.Reset(delay)
}

func (b *trigramBudget) expireIdle() {
	var evictions []func()

	b.mu.Lock()
	now := b.now()
	for owner, entry := range b.entries {
		if !now.Before(entry.lastUsed.Add(b.ttl)) {
			evictions = append(evictions, entry.release)
			delete(b.entries, owner)
		}
	}
	b.scheduleExpiryLocked(now)
	b.mu.Unlock()

	runTrigramReleases(evictions)
}

func runTrigramReleases(releases []func()) {
	for _, release := range releases {
		if release != nil {
			release()
		}
	}
}

// forget drops an owner without running its release callback. For an Indexer
// that is discarding its searcher itself (a repo untrack, say), so the budget
// does not later call back into a dead owner.
func (b *trigramBudget) forget(owner *Indexer) {
	if b == nil || owner == nil {
		return
	}
	b.mu.Lock()
	delete(b.entries, owner)
	b.scheduleExpiryLocked(b.now())
	b.mu.Unlock()
}

// live reports how many owners currently hold a built searcher.
func (b *trigramBudget) live() int {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.entries)
}

func trigramIdleTTLFromEnv() time.Duration {
	raw := os.Getenv("GORTEX_TRIGRAM_IDLE_TTL")
	if raw == "" {
		return defaultTrigramIdleTTL
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return defaultTrigramIdleTTL
	}
	return d
}

func trigramMaxLiveFromEnv() int {
	raw := os.Getenv("GORTEX_TRIGRAM_MAX_LIVE")
	if raw == "" {
		return defaultTrigramMaxLive
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return defaultTrigramMaxLive
	}
	return n
}
