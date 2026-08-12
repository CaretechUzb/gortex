package mcp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAtomicBatchLifecycleOutsideRootRefused(t *testing.T) {
	repoRoot := t.TempDir()
	outsideRoot := t.TempDir()
	t.Setenv(batchTransactionDirEnv, filepath.Join(t.TempDir(), "transactions"))
	s := newReadGuardServer(t, repoRoot)
	source := writeAtomicBatchFixture(t, repoRoot, "source.txt", "source\n")
	destination := filepath.Join(outsideRoot, "destination.txt")

	receipt, err := s.runBatchTransaction(context.Background(), []batchEditItem{
		atomicFileMove(source, destination, ""),
	}, "file-lifecycle-outside-root")
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Status != "aborted" || receipt.DiskStatus != "unchanged" || !strings.Contains(receipt.Error, "outside") {
		t.Fatalf("receipt = %+v", receipt)
	}
	if got := readAtomicBatchFixture(t, source); got != "source\n" {
		t.Fatalf("source = %q", got)
	}
	if _, err := os.Stat(destination); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("destination was created: %v", err)
	}
}

func TestAtomicBatchLifecycleInvalidDigestAndOverlapRefused(t *testing.T) {
	t.Run("invalid digest", func(t *testing.T) {
		s := newAtomicBatchTestServer(t, mutationTestWatcher{})
		path := writeAtomicBatchFixture(t, t.TempDir(), "source.txt", "source\n")
		receipt, err := s.runBatchTransaction(context.Background(), []batchEditItem{
			atomicFileDelete(path, "not-a-sha256"),
		}, "file-lifecycle-invalid-digest")
		if err != nil {
			t.Fatal(err)
		}
		if receipt.Status != "aborted" || receipt.DiskStatus != "unchanged" {
			t.Fatalf("receipt = %+v", receipt)
		}
		if got := readAtomicBatchFixture(t, path); got != "source\n" {
			t.Fatalf("source = %q", got)
		}
	})

	t.Run("overlapping path ownership", func(t *testing.T) {
		s := newAtomicBatchTestServer(t, mutationTestWatcher{})
		dir := t.TempDir()
		path := writeAtomicBatchFixture(t, dir, "source.txt", "source\n")
		receipt, err := s.runBatchTransaction(context.Background(), []batchEditItem{
			atomicFileEdit(path, "source", "edited"),
			atomicFileDelete(path, ""),
		}, "file-lifecycle-overlap")
		if err != nil {
			t.Fatal(err)
		}
		if receipt.Status != "aborted" || receipt.DiskStatus != "unchanged" || !strings.Contains(receipt.Error, "overlaps") {
			t.Fatalf("receipt = %+v", receipt)
		}
		if got := readAtomicBatchFixture(t, path); got != "source\n" {
			t.Fatalf("source = %q", got)
		}
	})
}
