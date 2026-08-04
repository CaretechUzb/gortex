package store_sqlite

import (
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// TestContractStateRoundTrip: set/get, missing-row, upsert-in-place, and
// per-repo row isolation on the SQLite store.
func TestContractStateRoundTrip(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "store.sqlite"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	// Missing row → found=false, no error. That is the "contract tier never
	// built here" signal the query path reports on.
	if _, found, err := s.GetContractState("repo"); err != nil || found {
		t.Fatalf("missing row: found=%v err=%v, want found=false, err=nil", found, err)
	}

	st := graph.ContractState{
		RepoPrefix:    "repo",
		IndexedSHA:    "abc123",
		CompletedAt:   1_700_000_000,
		ContractCount: 149,
	}
	if err := s.SetContractState(st); err != nil {
		t.Fatalf("SetContractState: %v", err)
	}

	got, found, err := s.GetContractState("repo")
	if err != nil || !found {
		t.Fatalf("get after set: found=%v err=%v, want found=true", found, err)
	}
	if got.RepoPrefix != "repo" || got.IndexedSHA != "abc123" ||
		got.CompletedAt != 1_700_000_000 || got.ContractCount != 149 {
		t.Fatalf("round-trip mismatch: %+v", got)
	}

	// Upsert on repo_prefix replaces in place.
	st.IndexedSHA = "def456"
	st.ContractCount = 151
	if err := s.SetContractState(st); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, _, _ = s.GetContractState("repo")
	if got.IndexedSHA != "def456" || got.ContractCount != 151 {
		t.Fatalf("upsert did not replace in place: %+v", got)
	}

	// A repo that committed a tier holding zero contracts is still MARKED —
	// "the pass ran" and "the pass found something" are different facts.
	if err := s.SetContractState(graph.ContractState{RepoPrefix: "empty", CompletedAt: 7}); err != nil {
		t.Fatalf("seed empty-tier marker: %v", err)
	}
	got, found, err = s.GetContractState("empty")
	if err != nil || !found || got.ContractCount != 0 {
		t.Fatalf("empty-tier marker: got %+v found=%v err=%v", got, found, err)
	}

	// A different repo is a distinct row.
	if _, found, _ := s.GetContractState("other"); found {
		t.Fatalf("a different repo must be its own row, got found=true")
	}
}

// TestOpenPreservesContractStateOnReopen proves the new table is picked up by
// an existing store with no wipe: a marker written on the first open survives
// the second open (which re-runs schemaSQL unconditionally) and the schema
// version does not drift.
func TestOpenPreservesContractStateOnReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.sqlite")

	s, err := Open(path)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	if err := s.SetContractState(graph.ContractState{
		RepoPrefix: "r", IndexedSHA: "sha1", CompletedAt: 42, ContractCount: 8,
	}); err != nil {
		t.Fatalf("seed marker: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()

	got, found, err := s2.GetContractState("r")
	if err != nil || !found {
		t.Fatalf("marker lost across reopen: found=%v err=%v (the table must survive a no-op reopen)", found, err)
	}
	if got.IndexedSHA != "sha1" || got.ContractCount != 8 {
		t.Fatalf("marker corrupted across reopen: %+v", got)
	}
	if v, _ := readUserVersion(s2.db); v != currentSchemaVersion {
		t.Fatalf("schema version drifted to %d after reopen, want %d", v, currentSchemaVersion)
	}
}

// PurgeRepo must clear the marker with the rest of the repo's residue —
// otherwise an untracked-then-retracked repo would look "already built" while
// its contract nodes were deleted.
func TestPurgeRepoClearsContractState(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "store.sqlite"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	s.AddBatch([]*graph.Node{{
		ID: "gone/a.go::A", Kind: graph.KindFunction, Name: "A", FilePath: "gone/a.go", RepoPrefix: "gone",
	}}, nil)
	if err := s.SetContractState(graph.ContractState{RepoPrefix: "gone", ContractCount: 3}); err != nil {
		t.Fatalf("seed marker: %v", err)
	}
	if err := s.SetContractState(graph.ContractState{RepoPrefix: "kept", ContractCount: 5}); err != nil {
		t.Fatalf("seed sibling marker: %v", err)
	}

	if err := s.PurgeRepo("gone"); err != nil {
		t.Fatalf("PurgeRepo: %v", err)
	}

	if _, found, _ := s.GetContractState("gone"); found {
		t.Fatalf("PurgeRepo left the purged repo's contract marker behind")
	}
	if _, found, _ := s.GetContractState("kept"); !found {
		t.Fatalf("PurgeRepo removed a sibling repo's contract marker")
	}
}
