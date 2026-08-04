package indexer

import "testing"

func TestMerkleBaselineCollector_CapturesRawSourceIdentity(t *testing.T) {
	collector := newMerkleBaselineCollector(true, 1)
	src := []byte("package before\n")
	collector.record("pkg/file.go", src, 123)
	src[0] = 'X'

	files := collector.take()
	got, ok := files["pkg/file.go"]
	if !ok {
		t.Fatal("captured file identity missing")
	}
	if got.Hash != contentHashForSource([]byte("package before\n")) {
		t.Fatalf("hash = %q, want raw source identity", got.Hash)
	}
	if got.Mtime != 123 {
		t.Fatalf("mtime = %d, want 123", got.Mtime)
	}
	if again := collector.take(); again != nil {
		t.Fatalf("collector retained transferred identities: %#v", again)
	}
}

func TestMerkleBaselineCollector_DisabledIsAllocationFreeSentinel(t *testing.T) {
	collector := newMerkleBaselineCollector(false, 100)
	if collector != nil {
		t.Fatal("disabled collector must be nil")
	}
	collector.record("pkg/file.go", []byte("package p"), 1)
	if got := collector.take(); got != nil {
		t.Fatalf("disabled collector returned identities: %#v", got)
	}
}
