package indexer

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// TestIncrementalReindex_SynthesizesCapabilityEdges guards the wiring that
// re-derives capability edges (reads_env / executes_process /
// accesses_field) on the daemon's incremental reindex path — not just at
// full index. A file added after the initial index produces fresh base
// edges (here a call to exec.Command); without the incremental synthesis
// pass its capability edge would never materialize until a full reindex,
// leaving a supply-chain / least-privilege audit looking at a stale
// capability surface in a long-lived daemon.
func TestIncrementalReindex_SynthesizesCapabilityEdges(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "base.go"), `package main

func main() {}
`)

	g := graph.New()
	idx := newTestIndexer(g)
	_, err := idx.Index(dir)
	require.NoError(t, err)

	countExec := func() int {
		n := 0
		for _, e := range g.AllEdges() {
			if e.Kind == graph.EdgeExecutesProcess {
				n++
			}
		}
		return n
	}
	require.Zero(t, countExec(), "no process execution exists before the new file is added")

	// A file added *after* the initial index — its capability edge can
	// only be synthesized by the incremental reindex path.
	writeFile(t, filepath.Join(dir, "shell.go"), `package main

import "os/exec"

func Run() error {
	return exec.Command("ls", "-la").Run()
}
`)

	_, err = idx.IncrementalReindexPaths(dir, nil)
	require.NoError(t, err)

	assert.Positive(t, countExec(),
		"incremental reindex must synthesize executes_process edges for newly indexed code")

	// The synthesized edge targets the canonical process node.
	var foundTarget bool
	for _, e := range g.AllEdges() {
		if e.Kind == graph.EdgeExecutesProcess && e.To == "string::process::exec.Command" {
			foundTarget = true
		}
	}
	assert.True(t, foundTarget, "executes_process edge should target string::process::exec.Command")
}
