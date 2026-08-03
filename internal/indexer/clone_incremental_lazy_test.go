package indexer

import (
	"testing"

	"github.com/zzet/gortex/internal/clones"
	"github.com/zzet/gortex/internal/graph"
)

func TestNewIncrementalCloneIndexStartsLazy(t *testing.T) {
	ci := newIncrementalCloneIndex()
	if ci == nil {
		t.Fatal("newIncrementalCloneIndex returned nil")
	}
	if ci.cms != nil || ci.lsh != nil || ci.shingles != nil {
		t.Fatal("unbuilt clone index eagerly allocated corpus state")
	}
	if ci.Ready() {
		t.Fatal("new clone index reported ready before a baseline was built")
	}

	ci.MarkPending()
	if !ci.Pending() {
		t.Fatal("lazy clone index did not retain pending state")
	}
	if ci.cms != nil || ci.lsh != nil || ci.shingles != nil {
		t.Fatal("marking an unbuilt clone index pending allocated corpus state")
	}
}

func TestLazyIncrementalCloneIndexRebuildInitializesState(t *testing.T) {
	ci := newIncrementalCloneIndex()
	ci.MarkPending()

	ci.Rebuild(graph.New(), "repo")

	if !ci.Ready() {
		t.Fatal("rebuilt clone index did not report ready")
	}
	if ci.cms == nil || ci.lsh == nil || ci.shingles == nil {
		t.Fatal("rebuild did not initialize clone corpus state")
	}
	if ci.Pending() {
		t.Fatal("rebuild did not clear pending state")
	}
}

func TestLazyIncrementalCloneIndexAdoptsBaseline(t *testing.T) {
	cms := clones.NewCMS(32, 2)
	lsh := clones.NewStratifiedIndex()
	shingles := map[string][]uint64{"body": {7}}
	baseline := &cloneCorpusBaseline{
		repoPrefix: "repo",
		cms:        cms,
		lsh:        lsh,
		shingles:   shingles,
		corpus:     1,
		complete:   true,
	}
	ci := newIncrementalCloneIndex()
	ci.MarkPending()

	ci.AdoptBaselineOrRebuild(nil, "repo", baseline)

	if !ci.Ready() {
		t.Fatal("clone index did not report ready after baseline adoption")
	}
	if ci.cms != cms || ci.lsh != lsh || len(ci.shingles["body"]) != 1 {
		t.Fatal("clone index did not adopt the supplied corpus state")
	}
	if ci.Pending() {
		t.Fatal("baseline adoption did not clear pending state")
	}
	if baseline.cms != nil || baseline.lsh != nil || baseline.shingles != nil || baseline.complete {
		t.Fatal("baseline ownership was not transferred")
	}
}

var benchmarkIncrementalCloneIndex *incrementalCloneIndex

func BenchmarkNewIncrementalCloneIndexLazy(b *testing.B) {
	b.ReportAllocs()
	for range b.N {
		benchmarkIncrementalCloneIndex = newIncrementalCloneIndex()
	}
}
