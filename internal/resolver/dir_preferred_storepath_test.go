package resolver

import (
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// Fixtures build the repo-relative part with filepath.FromSlash, matching the
// store shape (`repo/` + OS-native rest): all-slash on POSIX, mixed
// `repo/dir\file` on Windows.

func TestResolveByConventionSameDirWithNativeSeparators(t *testing.T) {
	g := graph.New()
	sameDir := "r/" + filepath.FromSlash("a/work.cs")
	otherDir := "r/" + filepath.FromSlash("b/work.cs")
	g.AddBatch([]*graph.Node{
		{ID: sameDir + "::Work", Kind: graph.KindFunction, Name: "Work", FilePath: sameDir},
		{ID: otherDir + "::Work", Kind: graph.KindFunction, Name: "Work", FilePath: otherDir},
	}, nil)

	fromFile := "r/" + filepath.FromSlash("a/caller.cs")
	id, conf := ResolveByConvention(g, "Work", "", nil, fromFile)
	if id != sameDir+"::Work" || conf != 0.85 {
		t.Fatalf("same-dir tier = (%q, %v), want (%q, 0.85)", id, conf, sameDir+"::Work")
	}
}

func TestResolveByConventionPreferredDirWithNativeSeparators(t *testing.T) {
	g := graph.New()
	middleware := "r/" + filepath.FromSlash("middleware/auth.cs")
	service := "r/" + filepath.FromSlash("services/auth.cs")
	g.AddBatch([]*graph.Node{
		{ID: middleware + "::auth", Kind: graph.KindFunction, Name: "auth", FilePath: middleware},
		{ID: service + "::auth", Kind: graph.KindFunction, Name: "auth", FilePath: service},
	}, nil)

	fromFile := "r/" + filepath.FromSlash("routes/index.cs")
	id, conf := ResolveByConvention(g, "auth", "", []string{"/middleware/"}, fromFile)
	if id != middleware+"::auth" || conf != 0.9 {
		t.Fatalf("preferred-dir tier = (%q, %v), want (%q, 0.9)", id, conf, middleware+"::auth")
	}
}
