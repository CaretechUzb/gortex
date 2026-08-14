package mcp

import (
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph"
)

func TestRecordLastSearchPageBoundsPositionsAndCanonicalFiles(t *testing.T) {
	nodes := make([]*graph.Node, lastSearchPageCap+1)
	for index := range nodes {
		path := fmt.Sprintf("repo/file-%03d.go", index)
		nodes[index] = &graph.Node{ID: path + "::Symbol", FilePath: path}
	}

	var session sessionState
	session.recordLastSearchPage("needle", nodes)

	session.mu.Lock()
	state := session.lastSearch
	gotReturned := append([]string(nil), state.returned...)
	gotLastRank, gotLast := state.returnedIDs[nodes[lastSearchPageCap-1].ID]
	_, gotOverflow := state.returnedIDs[nodes[lastSearchPageCap].ID]
	_, gotOverflowFile := state.fileIDs[nodes[lastSearchPageCap].FilePath]
	gotFileBuckets := len(state.fileIDs)
	gotMemberships := 0
	for _, bucket := range state.fileIDs {
		gotMemberships += len(bucket)
	}
	session.mu.Unlock()

	if len(gotReturned) != lastSearchPageCap {
		t.Fatalf("returned len = %d, want %d", len(gotReturned), lastSearchPageCap)
	}
	if !gotLast || gotLastRank != lastSearchPageCap-1 {
		t.Fatalf("last admitted rank = (%d, %v), want (%d, true)", gotLastRank, gotLast, lastSearchPageCap-1)
	}
	if gotOverflow || gotOverflowFile {
		t.Fatalf("position %d escaped cap: id=%v file=%v", lastSearchPageCap, gotOverflow, gotOverflowFile)
	}
	if gotFileBuckets > lastSearchPageCap || gotMemberships > lastSearchPageCap {
		t.Fatalf("file attribution escaped cap: buckets=%d memberships=%d cap=%d", gotFileBuckets, gotMemberships, lastSearchPageCap)
	}
}

func TestRecordLastSearchPageUsesRawPositionsAndCanonicalFallback(t *testing.T) {
	nodes := make([]*graph.Node, lastSearchPageCap+1)
	nodes[0] = nil
	nodes[1] = &graph.Node{}
	nodes[2] = &graph.Node{ID: "repo/a.go::A", FilePath: " ./repo/../repo/a.go "}
	nodes[3] = &graph.Node{ID: "repo/a.go::A", FilePath: "wrong/duplicate.go"}
	nodes[4] = &graph.Node{ID: "repo/fallback.go::B", FilePath: " . "}
	for index := 5; index < lastSearchPageCap; index++ {
		nodes[index] = nil
	}
	nodes[lastSearchPageCap] = &graph.Node{ID: "repo/overflow.go::C", FilePath: "repo/overflow.go"}

	var session sessionState
	session.recordLastSearchPage("needle", nodes)

	session.mu.Lock()
	gotReturned := append([]string(nil), session.lastSearch.returned...)
	gotA := append([]string(nil), session.lastSearch.fileIDs["repo/a.go"]...)
	gotFallback := append([]string(nil), session.lastSearch.fileIDs["repo/fallback.go"]...)
	_, gotDuplicatePath := session.lastSearch.fileIDs["wrong/duplicate.go"]
	_, gotOverflowPath := session.lastSearch.fileIDs["repo/overflow.go"]
	session.mu.Unlock()

	if want := []string{"repo/a.go::A", "repo/fallback.go::B"}; !reflect.DeepEqual(gotReturned, want) {
		t.Fatalf("returned = %#v, want %#v", gotReturned, want)
	}
	if want := []string{"repo/a.go::A"}; !reflect.DeepEqual(gotA, want) {
		t.Fatalf("canonical file IDs = %#v, want %#v", gotA, want)
	}
	if want := []string{"repo/fallback.go::B"}; !reflect.DeepEqual(gotFallback, want) {
		t.Fatalf("fallback file IDs = %#v, want %#v", gotFallback, want)
	}
	if gotDuplicatePath || gotOverflowPath {
		t.Fatalf("inadmissible file membership recorded: duplicate=%v overflow=%v", gotDuplicatePath, gotOverflowPath)
	}
}

func TestAttributedFileConsumptionIsFreshExactAndOnce(t *testing.T) {
	var session sessionState
	session.recordLastSearchPage("needle", []*graph.Node{
		{ID: "repo/a.go::A", FilePath: "repo/a.go"},
		{ID: "repo/a.go::B", FilePath: "repo/a.go"},
		{ID: "repo/b.go::C", FilePath: "repo/b.go"},
	})

	query, ids := session.attributedFileConsumption(" ./repo/a.go ")
	if query != "needle" || !reflect.DeepEqual(ids, []string{"repo/a.go::A", "repo/a.go::B"}) {
		t.Fatalf("first attribution = (%q, %#v)", query, ids)
	}
	if query, ids := session.attributedFileConsumption("repo/a.go"); query != "" || len(ids) != 0 {
		t.Fatalf("repeat attribution = (%q, %#v), want empty", query, ids)
	}
	if query, ids := session.attributedFileConsumption("repo/missing.go"); query != "" || len(ids) != 0 {
		t.Fatalf("other-file attribution = (%q, %#v), want empty", query, ids)
	}
	if query, ids := session.attributedFileConsumption("repo/b.go"); query != "needle" || !reflect.DeepEqual(ids, []string{"repo/b.go::C"}) {
		t.Fatalf("second file attribution = (%q, %#v)", query, ids)
	}

	session.recordLastSearchPage("stale", []*graph.Node{{ID: "repo/stale.go::S", FilePath: "repo/stale.go"}})
	session.mu.Lock()
	session.lastSearch.at = time.Now().Add(-comboWindow - time.Second)
	session.mu.Unlock()
	if query, ids := session.attributedFileConsumption("repo/stale.go"); query != "" || len(ids) != 0 {
		t.Fatalf("stale attribution = (%q, %#v), want empty", query, ids)
	}
}

func TestRecordLastSearchPageEmptyClearsPriorState(t *testing.T) {
	var session sessionState
	session.recordLastSearchPage("first", []*graph.Node{{ID: "repo/a.go::A", FilePath: "repo/a.go"}})
	session.recordLastSearchPage("second", nil)

	session.mu.Lock()
	state := session.lastSearch
	session.mu.Unlock()
	if state.query != "" || len(state.returned) != 0 || len(state.returnedIDs) != 0 || len(state.fileIDs) != 0 || len(state.consumed) != 0 || !state.at.IsZero() {
		t.Fatalf("empty page retained state: %#v", state)
	}
}

func TestRecordLastSearchPageOwnsInputsAndAttributionResults(t *testing.T) {
	node := &graph.Node{ID: "repo/a.go::A", FilePath: "repo/a.go"}
	page := []*graph.Node{node}
	var session sessionState
	session.recordLastSearchPage("needle", page)

	page[0] = nil
	node.ID = "mutated"
	node.FilePath = "mutated.go"
	query, ids := session.attributedFileConsumption("repo/a.go")
	if query != "needle" || !reflect.DeepEqual(ids, []string{"repo/a.go::A"}) {
		t.Fatalf("owned attribution = (%q, %#v)", query, ids)
	}
	ids[0] = "mutated-result"

	session.mu.Lock()
	gotReturned := append([]string(nil), session.lastSearch.returned...)
	session.mu.Unlock()
	if !reflect.DeepEqual(gotReturned, []string{"repo/a.go::A"}) {
		t.Fatalf("stored IDs aliased caller/result: %#v", gotReturned)
	}
}

func TestRecordLastSearchPageConcurrentAccessIsBounded(t *testing.T) {
	var session sessionState
	var workers sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		worker := worker
		workers.Add(1)
		go func() {
			defer workers.Done()
			for iteration := 0; iteration < 32; iteration++ {
				path := fmt.Sprintf("repo/%d.go", worker)
				session.recordLastSearchPage("needle", []*graph.Node{{ID: fmt.Sprintf("%s::S%d", path, iteration), FilePath: path}})
				session.attributedFileConsumption(path)
			}
		}()
	}
	workers.Wait()

	session.mu.Lock()
	defer session.mu.Unlock()
	if len(session.lastSearch.returned) > lastSearchPageCap || len(session.lastSearch.returnedIDs) > lastSearchPageCap || len(session.lastSearch.fileIDs) > lastSearchPageCap {
		t.Fatalf("concurrent state escaped cap: %#v", session.lastSearch)
	}
}
