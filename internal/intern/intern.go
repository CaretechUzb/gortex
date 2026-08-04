// Package intern provides a process-wide string interning table.
//
// The knowledge graph stores a node's ID once on the node, but also
// once on every edge endpoint that references it, and a file path once
// per node defined in that file. Under a multi-repo warmup those
// references are minted as fresh `repoPrefix + "/" + id` concatenations
// — a heap profile attributed ~425 MB of resident memory to that
// concatenation alone. Interning collapses every duplicate of a string
// to a single shared backing array.
//
// The table is process-global within one generation. Long-lived callers may
// explicitly rotate generations with Reset after every producer using the old
// canonical set has joined. Strings already returned remain valid; the reset
// only drops the table's references, allowing otherwise-unreferenced IDs and
// paths to be collected while preserving deduplication inside each generation.
package intern

import "sync"

// shardCount fans interning lookups across independent locks so the
// parallel warmup workers (one goroutine per repo) do not serialise on
// a single mutex. 64 keeps per-shard maps small without waste.
const shardCount = 64

type shard struct {
	mu sync.RWMutex
	m  map[string]string
}

var shards [shardCount]*shard

func init() {
	for i := range shards {
		shards[i] = &shard{m: make(map[string]string)}
	}
}

// shardFor picks a shard by FNV-1a hash of s — inlined rather than
// using hash/fnv to avoid a hash-object allocation on every call.
func shardFor(s string) *shard {
	var h uint32 = 2166136261
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= 16777619
	}
	return shards[h%shardCount]
}

// String returns the canonical instance of s. Every call with an equal
// string returns the same backing array, so the caller may drop its
// own copy. The empty string is returned as-is without touching the
// table. Safe for concurrent use.
func String(s string) string {
	if s == "" {
		return ""
	}
	sh := shardFor(s)
	sh.mu.RLock()
	c, ok := sh.m[s]
	sh.mu.RUnlock()
	if ok {
		return c
	}
	sh.mu.Lock()
	if c, ok = sh.m[s]; !ok {
		sh.m[s] = s
		c = s
	}
	sh.mu.Unlock()
	return c
}

// Reset starts a fresh process-wide intern generation and returns the number
// of canonical strings released from the table. Strings returned before the
// reset remain valid and keep their backing arrays for as long as callers hold
// them; equal strings interned after the reset may use a different backing
// array. Callers must therefore rely on string value equality, never backing
// identity, across a reset boundary.
//
// Lock every shard before swapping any map so concurrent String calls observe
// either side of one generation boundary, not a partially-rotated table. A
// String call holds at most one shard lock, so the fixed acquisition order
// cannot introduce a lock cycle.
func Reset() int {
	for _, sh := range shards {
		sh.mu.Lock()
	}
	removed := 0
	for _, sh := range shards {
		removed += len(sh.m)
		sh.m = make(map[string]string)
	}
	for i := len(shards) - 1; i >= 0; i-- {
		shards[i].mu.Unlock()
	}
	return removed
}

// Len reports the number of distinct strings currently interned.
// Intended for diagnostics and tests.
func Len() int {
	n := 0
	for _, sh := range shards {
		sh.mu.RLock()
		n += len(sh.m)
		sh.mu.RUnlock()
	}
	return n
}
