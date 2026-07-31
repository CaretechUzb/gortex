package store_sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"log"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// captureLog redirects the standard logger for the duration of fn and returns
// everything written to it. The periodic checkpoint reports through log.Printf,
// so its message is the contract under test.
func captureLog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	prevWriter := log.Writer()
	prevFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(prevWriter)
		log.SetFlags(prevFlags)
	})
	fn()
	log.SetOutput(prevWriter)
	log.SetFlags(prevFlags)
	return buf.String()
}

// A periodic PASSIVE checkpoint whose context expires before the PRAGMA runs
// has measured nothing: walCheckpointResult is still the zero value. Printing
// its fields states busy=0 wal_frames=0 checkpointed_frames=0 as if SQLite had
// answered, and calling that "incomplete" describes a checkpoint that ran and
// failed. Both are wrong — during a bulk index the checkpoint never started.
// See https://github.com/zzet/gortex/issues/325.
func TestPassiveCheckpointWriterDeadlineIsNotReportedAsIncomplete(t *testing.T) {
	store, err := Open(t.TempDir() + "/graph.sqlite")
	require.NoError(t, err)
	defer func() { require.NoError(t, store.Close()) }()

	store.AddNode(&graph.Node{ID: "repo/a.go::A", Kind: graph.KindFunction, Name: "A"})

	// Expire the window before checkpointWALOnce can reach SQLite, which is
	// what a writer pool pinned by the bulk indexer does in production.
	store.passiveCheckpointTimeout = time.Nanosecond

	out := captureLog(t, store.checkpointWALPassive)

	assert.NotContains(t, out, "incomplete",
		"a checkpoint that never executed must not be reported as incomplete")
	assert.NotContains(t, out, "wal_frames=0",
		"zero-value result fields must not be printed as measurements")
	assert.Contains(t, out, "deferred mode=PASSIVE reason=writer_busy",
		"an expired window is a deferral, reported like its writer_gate and bulk_writer siblings")
}

// The opposite case must keep its warning. Here the PRAGMA does run, returns
// real counters, and leaves frames behind because a reader still holds the
// WAL — that is a measurement an operator should see. This pins the fix to
// classifying the failure rather than silencing the branch.
func TestPassiveCheckpointReaderLimitedStillWarns(t *testing.T) {
	store, err := Open(t.TempDir() + "/graph.sqlite")
	require.NoError(t, err)
	defer func() { require.NoError(t, store.Close()) }()

	store.AddNode(&graph.Node{ID: "repo/a.go::A", Kind: graph.KindFunction, Name: "A"})

	readConn, err := store.db.Conn(context.Background())
	require.NoError(t, err)
	readTx, err := readConn.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	require.NoError(t, err)
	defer func() {
		_ = readTx.Rollback()
		_ = readConn.Close()
	}()
	var count int
	require.NoError(t, readTx.QueryRow(`SELECT COUNT(*) FROM nodes`).Scan(&count))

	// Written after the reader's snapshot, so these frames cannot be drained
	// while the read transaction is open.
	store.AddNode(&graph.Node{ID: "repo/b.go::B", Kind: graph.KindFunction, Name: "B"})

	out := captureLog(t, store.checkpointWALPassive)

	assert.Contains(t, out, "incomplete",
		"a checkpoint that ran and left frames behind must still warn")
}
