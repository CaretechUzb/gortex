package store_sqlite

import (
	"path/filepath"
	"testing"
)

func TestCloneCorpusInitializationPersistsEmptyReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "graph.sqlite")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}

	initialized, err := store.CloneCorpusInitialized("repo")
	if err != nil {
		t.Fatal(err)
	}
	if initialized {
		t.Fatal("fresh repository unexpectedly initialized")
	}
	if err := store.ReplaceCloneCorpus("repo", nil); err != nil {
		t.Fatal(err)
	}
	initialized, err = store.CloneCorpusInitialized("repo")
	if err != nil {
		t.Fatal(err)
	}
	if !initialized {
		t.Fatal("empty replacement did not mark clone corpus initialized")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	initialized, err = store.CloneCorpusInitialized("repo")
	if err != nil {
		t.Fatal(err)
	}
	if !initialized {
		t.Fatal("clone corpus initialization marker did not survive reopen")
	}
	page, err := store.CloneCorpusPage("repo", "", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 0 {
		t.Fatalf("empty replacement page has %d rows, want 0", len(page))
	}
}
