package daemon

import (
	"context"
	"testing"
)

// TestFederator_MergeReappliesUsageLimit pins the global row cap: each
// daemon applies limit:N independently, so the merged find_usages
// result must be re-capped once after the merge instead of growing to
// N times the daemon count. The discarded peer tails make the exact
// deduplicated total unknowable, so the merged totals are explicitly a
// floor (lower_bound) and the response is marked truncated.
func TestFederator_MergeReappliesUsageLimit(t *testing.T) {
	remote := fakeRemote(t, fakeRemoteOpts{indexed: true, toolJSON: `{
		"nodes":[{"id":"r/a.go::RUse1"},{"id":"r/b.go::RUse2"},{"id":"pkg/hot.go::Hot"}],
		"edges":[
			{"from":"r/a.go::RUse1","to":"pkg/hot.go::Hot","kind":"calls","file_path":"r/a.go","line":3},
			{"from":"r/b.go::RUse2","to":"pkg/hot.go::Hot","kind":"calls","file_path":"r/b.go","line":4}
		],
		"total_nodes":3,"total_edges":3,"truncated":true}`})
	local := envelope(`{
		"nodes":[{"id":"l/a.go::LUse1"},{"id":"l/b.go::LUse2"},{"id":"pkg/hot.go::Hot"}],
		"edges":[
			{"from":"l/a.go::LUse1","to":"pkg/hot.go::Hot","kind":"calls","file_path":"l/a.go","line":5},
			{"from":"l/b.go::LUse2","to":"pkg/hot.go::Hot","kind":"calls","file_path":"l/b.go","line":6}
		],
		"total_nodes":3,"total_edges":4,"truncated":true}`)

	out := testFederator().Augment(context.Background(), "find_usages",
		[]byte(`{"id":"pkg/hot.go::Hot","limit":2}`),
		local, []ServerEntry{{Slug: "r2", URL: remote.URL}})

	m := decodeFederated(t, out)
	edges, _ := m["edges"].([]any)
	if len(edges) != 2 {
		t.Fatalf("limit:2 must hold globally after the merge, got %d edges", len(edges))
	}
	if m["truncated"] != true {
		t.Errorf("a re-capped merged result must be truncated")
	}
	if m["lower_bound"] != true {
		t.Errorf("with peer tails discarded the merged totals are a floor; lower_bound must be set")
	}
}

// TestFederator_RemoteOnlyTruncationPropagates pins that a complete
// local result merged with a truncated remote page cannot claim the
// merged result is complete: the remote's discarded tail must surface
// as truncated + lower_bound on the merged response.
func TestFederator_RemoteOnlyTruncationPropagates(t *testing.T) {
	remote := fakeRemote(t, fakeRemoteOpts{indexed: true, toolJSON: `{
		"nodes":[{"id":"r/a.go::RUse1"}],
		"edges":[{"from":"r/a.go::RUse1","to":"pkg/hot.go::Hot","kind":"calls","file_path":"r/a.go","line":3}],
		"total_nodes":1,"total_edges":10,"truncated":true}`})
	local := envelope(`{
		"nodes":[{"id":"l/a.go::LUse1"}],
		"edges":[{"from":"l/a.go::LUse1","to":"pkg/hot.go::Hot","kind":"calls","file_path":"l/a.go","line":5}],
		"total_nodes":1,"total_edges":1,"truncated":false}`)

	out := testFederator().Augment(context.Background(), "find_usages",
		[]byte(`{"id":"pkg/hot.go::Hot","limit":50}`),
		local, []ServerEntry{{Slug: "r2", URL: remote.URL}})

	m := decodeFederated(t, out)
	if m["truncated"] != true {
		t.Errorf("remote-only truncation must propagate to the merged result")
	}
	if m["lower_bound"] != true {
		t.Errorf("the remote's discarded tail makes the merged totals a floor")
	}
}

// TestFederator_DistinctCallSitesSurviveMerge pins the merge dedup key:
// two usages of the same (from, to, kind) at different file/line call
// sites are distinct rows and must both survive the merge.
func TestFederator_DistinctCallSitesSurviveMerge(t *testing.T) {
	remote := fakeRemote(t, fakeRemoteOpts{indexed: true, toolJSON: `{
		"nodes":[{"id":"l/a.go::LUse1"}],
		"edges":[{"from":"l/a.go::LUse1","to":"pkg/hot.go::Hot","kind":"calls","file_path":"l/a.go","line":9}],
		"total_nodes":1,"total_edges":1,"truncated":false}`})
	local := envelope(`{
		"nodes":[{"id":"l/a.go::LUse1"}],
		"edges":[{"from":"l/a.go::LUse1","to":"pkg/hot.go::Hot","kind":"calls","file_path":"l/a.go","line":5}],
		"total_nodes":1,"total_edges":1,"truncated":false}`)

	out := testFederator().Augment(context.Background(), "find_usages",
		[]byte(`{"id":"pkg/hot.go::Hot","limit":50}`),
		local, []ServerEntry{{Slug: "r2", URL: remote.URL}})

	m := decodeFederated(t, out)
	edges, _ := m["edges"].([]any)
	if len(edges) != 2 {
		t.Fatalf("distinct call sites (same from/to/kind, different line) must both survive, got %d", len(edges))
	}
}
