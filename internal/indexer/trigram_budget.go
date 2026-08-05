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
	Live      int           `json:"live"`
	MaxLive   int           `json:"max_live"`
	Bytes     int64         `json:"bytes"`
	MaxBytes  int64         `json:"max_bytes"`
	IdleTTL   time.Duration `json:"idle_ttl"`
	BuildsOff bool          `json:"builds_off"`
	// Evictions counts searchers dropped since start, by any rule. A
	// number that climbs with every search says the budget is too small
	// for the workload and every query is paying a rebuild.
	Evictions int64 `json:"evictions"`
}

// TrigramCacheSnapshot reports the process-wide trigram cache.
func TrigramCacheSnapshot() TrigramCacheStats { return processTrigramBudget.stats() }

// trigramBudget tracks which repos currently hold a built trigram searcher
// and evicts by idle time and by count. It stores release callbacks rather
// than the searchers themselves so the owning Indexer keeps sole ownership
// of its field and its own mutex discipline.
type trigramBudget struct {
	mu        sync.Mutex
	ttl       time.Duration
	maxLive   int
	maxBytes  int64
	entries   map[*Indexer]*trigramEntry
	bytes     int64
	evictions int64
	now       func() time.Time // swappable in tests
	afterFunc func(time.Duration, func()) *time.Timer
	timer     *time.Timer
}

type trigramEntry struct {
	lastUsed time.Time
	release  func()
	// bytes is the owner's last reported estimated index size. Held per
	// entry so an eviction can subtract exactly what it removes rather
	// than re-polling an Indexer that is mid-release.
	bytes int64
}

var processTrigramBudget = newTrigramBudget(trigramIdleTTLFromEnv(), trigramMaxLiveFromEnv(), trigramMaxBytesFromEnv())

// newTrigramBudget builds a budget. A negative ttl / maxLive / maxBytes
// means "use the built-in default"; maxLive of exactly 0 is meaningful and
// disables building altogether.
func newTrigramBudget(ttl time.Duration, maxLive int, maxBytes int64) *trigramBudget {
	if ttl <= 0 {
		ttl = defaultTrigramIdleTTL
	}
	if maxLive < 0 {
		maxLive = defaultTrigramMaxLive
	}
	if maxBytes < 0 {
		maxBytes = defaultTrigramMaxBytes
	}
	return &trigramBudget{
		ttl:       ttl,
		maxLive:   maxLive,
		maxBytes:  maxBytes,
		entries:   make(map[*Indexer]*trigramEntry),
		now:       time.Now,
		afterFunc: time.AfterFunc,
	}
}

// allowsBuild reports whether a repo may build a searcher at all. False
// under GORTEX_TRIGRAM_MAX_LIVE=0, which turns every text search into a
// streaming scan over the known file list and holds no index state.
func (b *trigramBudget) allowsBuild() bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.maxLive > 0
}

// stats snapshots the budget for status output.
func (b *trigramBudget) stats() TrigramCacheStats {
	if b == nil {
		return TrigramCacheStats{}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return TrigramCacheStats{
		Live:      len(b.entries),
		MaxLive:   b.maxLive,
		Bytes:     b.bytes,
		MaxBytes:  b.maxBytes,
		IdleTTL:   b.ttl,
		BuildsOff: b.maxLive == 0,
		Evictions: b.evictions,
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
// bytes is owner's current estimated index size, used to hold the summed
// footprint under the byte budget. A count cap alone does not bound memory:
// three indexes of an arbitrarily large repo is still arbitrarily large.
func (b *trigramBudget) touch(owner *Indexer, release func(), bytes int64) {
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
		b.bytes += bytes - entry.bytes
		entry.bytes = bytes
	} else if release != nil {
		b.entries[owner] = &trigramEntry{lastUsed: now, release: release, bytes: bytes}
		b.bytes += bytes
	}

	for other, entry := range b.entries {
		if other == owner {
			continue
		}
		if !now.Before(entry.lastUsed.Add(b.ttl)) {
			evictions = append(evictions, b.dropLocked(other))
		}
	}

	for len(b.entries) > b.maxLive {
		release := b.evictLRULocked(owner)
		if release == nil {
			break
		}
		evictions = append(evictions, release)
	}

	// Byte eviction runs last so it sees the count rule's effect first.
	// owner is never evicted: it is the build being paid for right now, so
	// a single repo larger than the whole budget parks over it rather than
	// thrashing. The idle TTL still reclaims it once the repo goes quiet.
	for b.maxBytes > 0 && b.bytes > b.maxBytes {
		release := b.evictLRULocked(owner)
		if release == nil {
			break
		}
		evictions = append(evictions, release)
	}
	b.scheduleExpiryLocked(now)
	b.mu.Unlock()

	runTrigramReleases(evictions)
}

// dropLocked removes owner's entry, keeping the byte total in step, and
// returns its release callback for the caller to run outside the lock.
func (b *trigramBudget) dropLocked(owner *Indexer) func() {
	entry, ok := b.entries[owner]
	if !ok {
		return nil
	}
	b.bytes -= entry.bytes
	if b.bytes < 0 {
		b.bytes = 0
	}
	b.evictions++
	delete(b.entries, owner)
	return entry.release
}

// evictLRULocked drops the least recently used entry other than keep.
// Returns nil when nothing is eligible.
func (b *trigramBudget) evictLRULocked(keep *Indexer) func() {
	var oldest *Indexer
	var oldestAt time.Time
	for other, entry := range b.entries {
		if other == keep {
			continue
		}
		if oldest == nil || entry.lastUsed.Before(oldestAt) {
			oldest, oldestAt = other, entry.lastUsed
		}
	}
	if oldest == nil {
		return nil
	}
	return b.dropLocked(oldest)
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
			evictions = append(evictions, b.dropLocked(owner))
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
	if entry, ok := b.entries[owner]; ok {
		b.bytes -= entry.bytes
		if b.bytes < 0 {
			b.bytes = 0
		}
		delete(b.entries, owner)
	}
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

// trigramMaxLiveFromEnv reads GORTEX_TRIGRAM_MAX_LIVE. Zero is honoured
// and means "never build an index": every text search then streams over
// the known file list, trading latency for holding no index at all. A
// negative or unparseable value falls back to the default.
func trigramMaxLiveFromEnv() int {
	raw := os.Getenv("GORTEX_TRIGRAM_MAX_LIVE")
	if raw == "" {
		return defaultTrigramMaxLive
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return defaultTrigramMaxLive
	}
	return n
}

// trigramMaxBytesFromEnv reads GORTEX_TRIGRAM_MAX_MB, the ceiling on the
// summed estimated heap of every live searcher. Zero disables the byte
// ceiling and leaves only the count and TTL rules.
func trigramMaxBytesFromEnv() int64 {
	raw := os.Getenv("GORTEX_TRIGRAM_MAX_MB")
	if raw == "" {
		return defaultTrigramMaxBytes
	}
	mb, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || mb < 0 {
		return defaultTrigramMaxBytes
	}
	return mb << 20
}
