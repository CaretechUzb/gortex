package mcp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/agents"
)

func TestAtomicBatchLifecycleRecoveryRollsBackPartialMove(t *testing.T) {
	s := newAtomicBatchTestServer(t, mutationTestWatcher{})
	dir := t.TempDir()
	source := writeAtomicBatchFixture(t, dir, "source.txt", "source\n")
	destination := filepath.Join(dir, "destination.txt")
	buffers := map[string]*batchFileBuffer{
		source: {
			absPath: source, relPath: "source.txt", mode: 0o644,
			original: []byte("source\n"), content: []byte("source\n"),
			existsBefore: true, existsAfter: false, existenceSet: true,
		},
		destination: {
			absPath: destination, relPath: "destination.txt", mode: 0o644,
			content: []byte("source\n"), existsAfter: true, existenceSet: true,
		},
	}
	results := []batchEditResult{{
		Op: "move_file", FilePath: "source.txt", DestinationPath: "destination.txt", Status: "validated",
	}}
	receipt := batchTransactionReceipt{
		Version: batchTransactionVersion, TransactionID: "recover-partial-move", Fingerprint: "recovery-fixture",
		Status: "preparing", DiskStatus: "unchanged", GraphStatus: "not_started",
		Results: results, Summary: batchSummary(results),
	}
	if err := s.prepareBatchJournal(&receipt, buffers, []string{destination, source}); err != nil {
		t.Fatal(err)
	}
	if err := agents.AtomicWriteFile(destination, []byte("source\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	restarted := &Server{watcher: mutationTestWatcher{}, session: newSessionState()}
	recovered, err := restarted.batchTransactionStatus(context.Background(), "recover-partial-move")
	if err != nil {
		t.Fatal(err)
	}
	if !recovered.Recovered || recovered.Status != "aborted" || recovered.DiskStatus != "rolled_back" {
		t.Fatalf("recovered receipt = %+v", recovered)
	}
	if got := readAtomicBatchFixture(t, source); got != "source\n" {
		t.Fatalf("source = %q", got)
	}
	if _, err := os.Stat(destination); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("destination survived rollback: %v", err)
	}
}
