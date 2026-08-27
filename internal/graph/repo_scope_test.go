package graph

import "testing"

// TestSharedAcrossRepos pins which node kinds a repo-narrow predicate
// must admit regardless of the prefix stamped on them. Only contract
// nodes qualify: their IDs are global, so the prefix records mint order.
// Bridge nodes carry their scope in the ID and must stay narrowable.
func TestSharedAcrossRepos(t *testing.T) {
	tests := []struct {
		name string
		node *Node
		want bool
	}{
		{"nil node is not shared", nil, false},
		{"contract node is shared", &Node{Kind: KindContract, ID: "ws::pointerup", RepoPrefix: "local@aurora"}, true},
		{"contract node is shared even with no prefix", &Node{Kind: KindContract, ID: "http::GET::/x"}, true},
		{"contract bridge carries its own scope", &Node{Kind: KindContractBridge, ID: "bridge::ws::proj::http::GET::/x", RepoPrefix: "local"}, false},
		{"ordinary function is owned", &Node{Kind: KindFunction, ID: "local/a.go::F", RepoPrefix: "local"}, false},
		{"file is owned", &Node{Kind: KindFile, ID: "local/a.go", RepoPrefix: "local"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := SharedAcrossRepos(tc.node); got != tc.want {
				t.Fatalf("SharedAcrossRepos(%v) = %v, want %v", tc.node, got, tc.want)
			}
		})
	}
}
