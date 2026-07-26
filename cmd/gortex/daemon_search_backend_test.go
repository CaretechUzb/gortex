package main

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search"
)

// fakeSymbolSearcher is a minimal graph.SymbolSearcher stand-in so
// resolveSearchBackend's SymbolSearcherBackend branch can be exercised
// without a real sqlite store.
type fakeSymbolSearcher struct{}

func (fakeSymbolSearcher) UpsertSymbolFTS(string, string) error                    { return nil }
func (fakeSymbolSearcher) BulkUpsertSymbolFTS(string, []graph.SymbolFTSItem) error { return nil }
func (fakeSymbolSearcher) BuildSymbolIndex() error                                 { return nil }
func (fakeSymbolSearcher) SearchSymbols(string, int) ([]graph.SymbolHit, error)    { return nil, nil }

// countingSymbolSearcher is a store that can answer the authoritative
// document count, as the real SQLite store does.
type countingSymbolSearcher struct {
	fakeSymbolSearcher
	count int
}

func (c countingSymbolSearcher) SymbolFTSCount() (int, error) { return c.count, nil }

func TestResolveSearchBackend_SymbolSearcherBackend(t *testing.T) {
	b := search.NewSymbolSearcherBackend(fakeSymbolSearcher{})
	b.Add("node-1")
	b.Add("node-2")

	info := resolveSearchBackend(b)

	assert.Equal(t, "sqlite-fts5", info.Name)
	assert.True(t, info.DiskResident, "the FTS5 index lives inside the graph store, not in-process heap")
	assert.Zero(t, info.Bytes, "no fabricated byte count for a disk-resident backend")
	// Count() is a since-construction Add/Remove delta, not a corpus size —
	// it goes negative as soon as an eviction path drops more than the admit
	// predicate ever added. A store that cannot answer the real count must
	// leave the figure unreported rather than have the delta stand in for it.
	assert.False(t, info.DocCountKnown, "the delta must never be reported as a document count")
	assert.Zero(t, info.DocCount)
}

func TestResolveSearchBackend_SymbolSearcherBackend_CountFromIndex(t *testing.T) {
	// Adds and removes move the adapter's delta; the reported figure must
	// still be the index's own count, not that delta.
	b := search.NewSymbolSearcherBackend(countingSymbolSearcher{count: 48572})
	b.Add("node-1")
	b.Remove("node-2")
	b.Remove("node-3")
	assert.Negative(t, b.Count(), "precondition: the delta is negative here")

	info := resolveSearchBackend(b)

	assert.True(t, info.DocCountKnown)
	assert.Equal(t, 48572, info.DocCount, "DocCount must come from the index, not the adapter delta")
}

func TestResolveSearchBackend_SymbolSearcherBackend_ThroughSwappable(t *testing.T) {
	b := search.NewSymbolSearcherBackend(fakeSymbolSearcher{})
	sw := search.NewSwappable(b)

	info := resolveSearchBackend(sw)

	assert.Equal(t, "sqlite-fts5", info.Name)
	assert.True(t, info.DiskResident)
}

func TestRenderDaemonHeader_SearchBackendRow_SymbolSearcher(t *testing.T) {
	st := sampleStatus()
	st.SearchBackend = daemon.SearchBackendStats{
		Name:          "sqlite-fts5",
		DocCount:      48572,
		DocCountKnown: true,
		DiskResident:  true,
	}
	var buf bytes.Buffer
	renderDaemonHeader(&buf, st)
	out := buf.String()
	assert.Contains(t, out, "sqlite-fts5")
	assert.Contains(t, out, "48572")
	assert.Contains(t, out, "disk-resident")
	assert.NotContains(t, out, "heap=0 B", "must not print a fabricated zero heap size")
}

func TestRenderDaemonHeader_OmitsUnknownDocCount(t *testing.T) {
	st := sampleStatus()
	st.SearchBackend = daemon.SearchBackendStats{
		Name:         "sqlite-fts5",
		DiskResident: true,
	}
	var buf bytes.Buffer
	renderDaemonHeader(&buf, st)
	out := buf.String()
	assert.Contains(t, out, "sqlite-fts5")
	assert.Contains(t, out, "disk-resident")
	assert.NotContains(t, out, "docs=",
		"a backend with no real count must omit the figure, not print docs=0 or a delta")
}
