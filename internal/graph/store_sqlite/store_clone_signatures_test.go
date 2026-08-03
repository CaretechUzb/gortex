package store_sqlite

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestBulkSetCloneSignaturesDoesNotRewriteShingles(t *testing.T) {
	store, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const (
		repo   = "repo"
		nodeID = "repo/file.go::work"
	)
	store.AddNode(&graph.Node{
		ID: nodeID, RepoPrefix: repo, Kind: graph.KindFunction,
		Name: "work", FilePath: "file.go",
	})
	if err := store.BulkSetCloneCorpus(repo, []graph.CloneCorpusRow{{
		NodeID: nodeID, Shingles: []uint64{11, 22, 33}, TokenCount: 17,
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`
CREATE TRIGGER reject_clone_shingle_rewrite
BEFORE UPDATE OF shingles ON clone_shingles
BEGIN
  SELECT RAISE(FAIL, 'shingles rewritten');
END`); err != nil {
		t.Fatal(err)
	}

	if err := store.BulkSetCloneSignatures(repo, []graph.CloneCorpusSignatureUpdate{{
		NodeID: nodeID, Signature: "encoded-signature",
	}}); err != nil {
		t.Fatalf("signature-only update touched shingles: %v", err)
	}
	page, err := store.CloneCorpusPage(repo, "", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 1 || !page[0].Finalized || page[0].Signature != "encoded-signature" {
		t.Fatalf("finalized page = %+v", page)
	}
	if got := page[0].Shingles; len(got) != 3 || got[0] != 11 || got[1] != 22 || got[2] != 33 {
		t.Fatalf("shingles changed: %v", got)
	}
	var promoted string
	if err := store.db.QueryRow(`SELECT clone_sig FROM nodes WHERE id = ?`, nodeID).Scan(&promoted); err != nil {
		t.Fatal(err)
	}
	if promoted != "encoded-signature" {
		t.Fatalf("promoted clone_sig = %q", promoted)
	}

	if err := store.BulkSetCloneSignatures(repo, []graph.CloneCorpusSignatureUpdate{{NodeID: nodeID}}); err != nil {
		t.Fatalf("filtered signature update touched shingles: %v", err)
	}
	var promotedCount int
	if err := store.db.QueryRow(`SELECT count(*) FROM nodes WHERE id = ? AND clone_sig IS NOT NULL`, nodeID).Scan(&promotedCount); err != nil {
		t.Fatal(err)
	}
	if promotedCount != 0 {
		t.Fatal("filtered signature did not clear promoted clone_sig")
	}
}
