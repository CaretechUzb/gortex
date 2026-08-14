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

func TestPrepareBatchJournalRequiresExplicitExistenceState(t *testing.T) {
	t.Setenv(batchTransactionDirEnv, filepath.Join(t.TempDir(), "transactions"))
	s := newAtomicBatchTestServer(t, mutationTestWatcher{})
	path := filepath.Join(t.TempDir(), "empty.txt")
	receipt := batchTransactionReceipt{TransactionID: "unset-existence-state"}
	err := s.prepareBatchJournal(&receipt, map[string]*batchFileBuffer{
		path: {absPath: path, relPath: "empty.txt"},
	}, []string{path})
	if err == nil || !strings.Contains(err.Error(), "existence state is unset") {
		t.Fatalf("prepareBatchJournal error = %v", err)
	}
}

func TestAtomicBatchLifecycleDestinationGuards(t *testing.T) {
	t.Run("destination symlink", func(t *testing.T) {
		if os.PathSeparator == '\\' {
			t.Skip("symlink creation is not reliably available on Windows CI")
		}
		repoRoot := t.TempDir()
		t.Setenv(batchTransactionDirEnv, filepath.Join(t.TempDir(), "transactions"))
		s := newReadGuardServer(t, repoRoot)
		source := writeAtomicBatchFixture(t, repoRoot, "source.txt", "source\n")
		target := writeAtomicBatchFixture(t, repoRoot, "target.txt", "target\n")
		destination := filepath.Join(repoRoot, "destination.txt")
		if err := os.Symlink(target, destination); err != nil {
			t.Fatal(err)
		}

		receipt, err := s.runBatchTransaction(context.Background(), []batchEditItem{
			atomicFileMove(source, destination, ""),
		}, "file-lifecycle-destination-symlink")
		if err != nil {
			t.Fatal(err)
		}
		if receipt.Status != "aborted" || receipt.DiskStatus != "unchanged" || !strings.Contains(receipt.Error, "symlink") {
			t.Fatalf("receipt = %+v", receipt)
		}
		if got := readAtomicBatchFixture(t, source); got != "source\n" {
			t.Fatalf("source = %q", got)
		}
		if got := readAtomicBatchFixture(t, target); got != "target\n" {
			t.Fatalf("target = %q", got)
		}
	})

	t.Run("symlinked destination parent", func(t *testing.T) {
		if os.PathSeparator == '\\' {
			t.Skip("symlink creation is not reliably available on Windows CI")
		}
		repoRoot := t.TempDir()
		t.Setenv(batchTransactionDirEnv, filepath.Join(t.TempDir(), "transactions"))
		s := newReadGuardServer(t, repoRoot)
		source := writeAtomicBatchFixture(t, repoRoot, "source.txt", "source\n")
		realParent := filepath.Join(repoRoot, "real-parent")
		if err := os.Mkdir(realParent, 0o755); err != nil {
			t.Fatal(err)
		}
		linkedParent := filepath.Join(repoRoot, "linked-parent")
		if err := os.Symlink(realParent, linkedParent); err != nil {
			t.Fatal(err)
		}
		destination := filepath.Join(linkedParent, "destination.txt")

		receipt, err := s.runBatchTransaction(context.Background(), []batchEditItem{
			atomicFileMove(source, destination, ""),
		}, "file-lifecycle-symlinked-parent")
		if err != nil {
			t.Fatal(err)
		}
		if receipt.Status != "aborted" || receipt.DiskStatus != "unchanged" || !strings.Contains(receipt.Error, "symlink") {
			t.Fatalf("receipt = %+v", receipt)
		}
		if got := readAtomicBatchFixture(t, source); got != "source\n" {
			t.Fatalf("source = %q", got)
		}
		if _, err := os.Stat(filepath.Join(realParent, "destination.txt")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("destination was created: %v", err)
		}
	})

	t.Run("dot-dot traversal destination", func(t *testing.T) {
		repoRoot := t.TempDir()
		t.Setenv(batchTransactionDirEnv, filepath.Join(t.TempDir(), "transactions"))
		s := newReadGuardServer(t, repoRoot)
		source := writeAtomicBatchFixture(t, repoRoot, "source.txt", "source\n")
		outsideName := "traversal-destination-" + filepath.Base(repoRoot) + ".txt"
		destination := repoRoot + string(os.PathSeparator) + ".." + string(os.PathSeparator) + outsideName

		receipt, err := s.runBatchTransaction(context.Background(), []batchEditItem{
			atomicFileMove(source, destination, ""),
		}, "file-lifecycle-dot-dot-destination")
		if err != nil {
			t.Fatal(err)
		}
		if receipt.Status != "aborted" || receipt.DiskStatus != "unchanged" || !strings.Contains(receipt.Error, "outside") {
			t.Fatalf("receipt = %+v", receipt)
		}
		if got := readAtomicBatchFixture(t, source); got != "source\n" {
			t.Fatalf("source = %q", got)
		}
		if _, err := os.Stat(filepath.Clean(destination)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("destination was created: %v", err)
		}
	})

	t.Run("delete directory", func(t *testing.T) {
		repoRoot := t.TempDir()
		t.Setenv(batchTransactionDirEnv, filepath.Join(t.TempDir(), "transactions"))
		s := newReadGuardServer(t, repoRoot)
		directory := filepath.Join(repoRoot, "directory")
		if err := os.Mkdir(directory, 0o755); err != nil {
			t.Fatal(err)
		}

		receipt, err := s.runBatchTransaction(context.Background(), []batchEditItem{
			atomicFileDelete(directory, ""),
		}, "file-lifecycle-delete-directory")
		if err != nil {
			t.Fatal(err)
		}
		if receipt.Status != "aborted" || receipt.DiskStatus != "unchanged" {
			t.Fatalf("receipt = %+v", receipt)
		}
		info, err := os.Stat(directory)
		if err != nil || !info.IsDir() {
			t.Fatalf("directory was changed: info=%v err=%v", info, err)
		}
	})
}
