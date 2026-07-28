package resolver

import "testing"

// A synthetic global external — dep::, external::, non-Go module:: — carries
// no repo prefix because it models a third-party symbol owned by no
// repository and visible from every one. Resolving to it is not a cross-repo
// hop.
//
// This mattered nowhere while a lone repo indexed unprefixed: caller and
// candidate were both "" so the same-repo tier claimed globals first. Once
// every repo carries a prefix they fall through to the cross-repo fallback
// tier, whose reachability gate admits any empty target unconditionally — so
// without this predicate every dep::/external:: bind would be stamped
// Edge.CrossRepo and counted under stats.ByRepo[""], a bucket naming no repo.
// `analyze kind=cross_repo` would then report boundary crossings that never
// happened, and nothing would error.
func TestIsCrossRepoHop(t *testing.T) {
	cases := []struct {
		name       string
		callerRepo string
		targetRepo string
		want       bool
	}{
		{"different repos is a crossing", "alpha", "beta", true},
		{"same repo is not", "alpha", "alpha", false},
		{"synthetic global target is not a crossing", "alpha", "", false},
		{"synthetic global from a prefix-less caller is not either", "", "", false},
		{"a prefix-less caller reaching a real repo is a crossing", "", "beta", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isCrossRepoHop(tc.callerRepo, tc.targetRepo); got != tc.want {
				t.Fatalf("isCrossRepoHop(%q, %q) = %v, want %v",
					tc.callerRepo, tc.targetRepo, got, tc.want)
			}
		})
	}
}
