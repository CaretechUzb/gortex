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
	// once, which is what actually bounds the worst case: without it, N repos
	// searched inside one TTL window still means N live indexes.
	defaultTrigramMaxLive = 3
)

// trigramBudget tracks which repos currently hold a built trigram searcher
// and evicts by idle time and by count. It stores release callbacks rather
// than the searchers themselves so the owning Indexer keeps sole ownership
// of its field and its own mutex discipline.
type trigramBudget struct {
	mu      sync.Mutex
	ttl     time.Duration
	maxLive int
	entries map[*Indexer]*trigramEntry
	now     func() time.Time // swappable in tests
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
		ttl:     ttl,
		maxLive: maxLive,
		entries: make(map[*Indexer]*trigramEntry),
		now:     time.Now,
	}
}

// touch records that owner's searcher is live and just used, then evicts
// everything that has aged out and, if still over the cap, the
// least-recently-used entries until it fits.
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
		if now.Sub(entry.lastUsed) >= b.ttl {
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
	b.mu.Unlock()

	for _, release := range evictions {
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
