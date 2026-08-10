package mcp

import (
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// Fixture paths use filepath.FromSlash for the repo-relative part, matching
// the store shape: all-slash on POSIX, mixed `repo/dir\file` on Windows.

func TestBridgeAdjacencyScoreNativeSeparatorPaths(t *testing.T) {
	symNode := &graph.Node{
		FilePath:   "r/" + filepath.FromSlash("billing/api.cs"),
		RepoPrefix: "r",
	}
	sameDir := bridgeSideEntry{FilePath: "r/" + filepath.FromSlash("billing/impl.cs"), Repo: "r"}
	otherDir := bridgeSideEntry{FilePath: "r/" + filepath.FromSlash("shipping/impl.cs"), Repo: "r"}

	if got := bridgeAdjacencyScore(&bridgeGroupResult{Providers: []bridgeSideEntry{sameDir}}, symNode, nil); got != 2 {
		t.Fatalf("same-directory entry score = %v, want 2", got)
	}
	if got := bridgeAdjacencyScore(&bridgeGroupResult{Providers: []bridgeSideEntry{otherDir}}, symNode, nil); got != 1 {
		t.Fatalf("same-repo entry score = %v, want 1", got)
	}
}
