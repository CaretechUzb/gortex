package indexer

import (
	"reflect"
	"testing"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
)

type countingRepoProjectionStore struct {
	graph.Store
	languageCalls    int
	languagePrefixes []string
	languageCounts   map[string]map[string]int
	kindIDCalls      int
	kindIDPrefixes   []string
	kindIDKinds      []graph.NodeKind
}

func (s *countingRepoProjectionStore) RepoLanguageCounts(prefixes []string) map[string]map[string]int {
	s.languageCalls++
	s.languagePrefixes = append([]string(nil), prefixes...)
	return s.languageCounts
}

func (s *countingRepoProjectionStore) RepoNodeIDsByKinds(prefixes []string, kinds []graph.NodeKind) []string {
	if reflect.DeepEqual(kinds, []graph.NodeKind{graph.KindType, graph.KindInterface}) {
		s.kindIDCalls++
		s.kindIDPrefixes = append([]string(nil), prefixes...)
		s.kindIDKinds = append([]graph.NodeKind(nil), kinds...)
	}
	return nil
}

func (s *countingRepoProjectionStore) RepoStats() map[string]graph.GraphStats {
	panic("batchLanguageSets must use node-only language counts")
}

func TestBatchLanguageSetsUsesOneNodeOnlyProjection(t *testing.T) {
	store := &countingRepoProjectionStore{
		Store: graph.New(),
		languageCounts: map[string]map[string]int{
			"a": {"typescript": 2, "go": 5},
			"b": {"python": 3},
		},
	}
	mi := &MultiIndexer{graph: store}
	a := &Indexer{repoPrefix: "a"}
	b := &Indexer{repoPrefix: "b"}

	got, _ := mi.batchLanguageSets([]*Indexer{a, b})
	if store.languageCalls != 1 {
		t.Fatalf("language projection calls = %d, want 1", store.languageCalls)
	}
	if want := []string{"a", "b"}; !reflect.DeepEqual(store.languagePrefixes, want) {
		t.Fatalf("projection prefixes = %#v, want %#v", store.languagePrefixes, want)
	}
	if want := []string{"go", "typescript"}; !reflect.DeepEqual(got[a], want) {
		t.Fatalf("languages[a] = %#v, want %#v", got[a], want)
	}
	if want := []string{"python"}; !reflect.DeepEqual(got[b], want) {
		t.Fatalf("languages[b] = %#v, want %#v", got[b], want)
	}
}

func TestRunGlobalGraphPassesProjectsScopedTypeIDsOnce(t *testing.T) {
	store := &countingRepoProjectionStore{Store: graph.New()}
	mi := &MultiIndexer{
		graph:  store,
		logger: zap.NewNop(),
	}
	mi.BeginBatch()
	mi.ArmBatchScope(map[string]struct{}{"b": {}, "a": {}})
	mi.EndBatch()

	if store.kindIDCalls != 1 {
		t.Fatalf("kind-ID projection calls = %d, want 1", store.kindIDCalls)
	}
	if want := []string{"a", "b"}; !reflect.DeepEqual(store.kindIDPrefixes, want) {
		t.Fatalf("kind-ID prefixes = %#v, want %#v", store.kindIDPrefixes, want)
	}
	if want := []graph.NodeKind{graph.KindType, graph.KindInterface}; !reflect.DeepEqual(store.kindIDKinds, want) {
		t.Fatalf("kind-ID kinds = %#v, want %#v", store.kindIDKinds, want)
	}
}
