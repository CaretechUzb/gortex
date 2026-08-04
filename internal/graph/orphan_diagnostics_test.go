package graph

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestOrphanDiagnosticsCensus(t *testing.T) {
	var d OrphanDiagnostics
	d.Candidates = 4
	d.Record("a.go", "repo", FilePathLive, 2)
	d.Record("tmp/b.go", "repo", FilePathGone, 2)
	d.Record("tmp/c.go", "repo", FilePathGone, 2)
	d.Record("tmp/d.go", "repo", FilePathGone, 2)

	assert.Equal(t, 4, d.Checked)
	assert.Equal(t, 3, d.Orphans)
	assert.False(t, d.Clean())
	assert.InDelta(t, 0.75, d.Rate(), 1e-9)
	assert.Equal(t, 25.0, d.LiveScore())
	assert.Equal(t, 3, d.EstimatedOrphans(), "a census extrapolates to itself")
	assert.Equal(t, []string{"tmp/b.go", "tmp/c.go"}, d.OrphanSamples, "samples stop at the limit")
	assert.Equal(t, map[string]int{"repo": 3}, d.OrphansByRepo)
}

// Unknown is not absence: a path the audit could not decide must never be
// counted as an orphan, or an unreadable directory reads as a deleted repo.
func TestOrphanDiagnosticsUnresolvableIsNotAnOrphan(t *testing.T) {
	var d OrphanDiagnostics
	d.Record("a.go", "repo", FilePathUnknown, 3)
	d.Record("b.go", "repo", FilePathUnknown, 3)

	assert.Zero(t, d.Checked)
	assert.Zero(t, d.Orphans)
	assert.Equal(t, 2, d.Unresolvable)
	assert.True(t, d.Clean())
	assert.Zero(t, d.Rate())
	assert.Equal(t, 100.0, d.LiveScore(), "an audit that decided nothing must not lower health")
	assert.Empty(t, d.OrphansByRepo)
}

func TestOrphanDiagnosticsSampledScanExtrapolates(t *testing.T) {
	var d OrphanDiagnostics
	d.Candidates = 1000
	d.Truncated = true
	for i := 0; i < 75; i++ {
		d.Record("gone.go", "repo", FilePathGone, 1)
	}
	for i := 0; i < 25; i++ {
		d.Record("live.go", "repo", FilePathLive, 1)
	}

	assert.Equal(t, 100, d.Checked)
	assert.Equal(t, 75, d.Orphans)
	assert.Equal(t, 750, d.EstimatedOrphans())
	assert.Equal(t, 25.0, d.LiveScore())
	assert.Contains(t, d.Summary(), "sampled from 1000")
	assert.Contains(t, d.Summary(), "~750 total")
}

func TestOrphanDiagnosticsCleanGraph(t *testing.T) {
	var d OrphanDiagnostics
	d.Candidates = 2
	d.Record("a.go", "repo", FilePathLive, 3)
	d.Record("b.go", "repo", FilePathLive, 3)

	assert.True(t, d.Clean())
	assert.Equal(t, 100.0, d.LiveScore())
	assert.Equal(t, "0 of 2 indexed files no longer exist on disk", d.Summary())
}
