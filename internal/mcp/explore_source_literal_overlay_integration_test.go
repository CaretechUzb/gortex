package mcp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/query"
)

func newExploreSourceLiteralOverlayTestServer(t *testing.T, diskFiles map[string]string) (*Server, string) {
	t.Helper()
	root := t.TempDir()
	mtimes := make(map[string]int64, len(diskFiles))
	for rel, content := range diskFiles {
		abs := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", rel, err)
		}
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
		mtimes[rel] = 1
	}
	store := graph.New()
	reg := parser.NewRegistry()
	languages.RegisterAll(reg)
	idx := indexer.New(store, reg, config.IndexConfig{}, zap.NewNop())
	idx.SetRootPath(root)
	idx.SetRepoPrefix("repo")
	idx.SetWorkspaceID("workspace")
	idx.SetProjectID("project")
	idx.SetFileMtimes(mtimes)
	engine := query.NewEngine(store)
	server := NewServer(engine, store, idx, nil, zap.NewNop(), nil)
	manager := daemon.NewOverlayManager(time.Hour)
	server.SetOverlayManager(manager)
	const sessionID = "literal-overlay-test"
	if err := manager.RegisterWithID(sessionID, "workspace"); err != nil {
		t.Fatalf("register overlay: %v", err)
	}
	return server, sessionID
}

func pushExploreSourceLiteralOverlay(t *testing.T, server *Server, sessionID, path, content string, deleted bool) {
	t.Helper()
	if err := server.overlays.Push(sessionID, daemon.OverlayFile{
		Path: path, Content: content, Deleted: deleted,
	}, nil); err != nil {
		t.Fatalf("push %s: %v", path, err)
	}
}

func prepareExploreSourceLiteralOverlayContext(t *testing.T, server *Server, sessionID string) context.Context {
	t.Helper()
	ctx, _, err := server.prepareOverlayRequest(WithSessionID(context.Background(), sessionID))
	if err != nil {
		t.Fatalf("prepare overlay request: %v", err)
	}
	return ctx
}

func TestSearchExploreSourceLiteralFindsOverlayOnlyAndMasksDurable(t *testing.T) {
	server, sessionID := newExploreSourceLiteralOverlayTestServer(t, map[string]string{
		"replace.go": "package repo\nvar _ = \"needle\"\n",
		"delete.go":  "package repo\nvar _ = \"needle\"\n",
		"keep.go":    "package repo\nvar _ = \"needle\"\n",
	})
	pushExploreSourceLiteralOverlay(t, server, sessionID, "replace.go", "package repo\nvar _ = \"other\"\n", false)
	pushExploreSourceLiteralOverlay(t, server, sessionID, "delete.go", "", true)
	pushExploreSourceLiteralOverlay(t, server, sessionID, "new.go", "package repo\nvar _ = \"needle\"\n", false)

	result := server.searchExploreSourceLiteral(
		prepareExploreSourceLiteralOverlayContext(t, server, sessionID), "needle", "repo",
		query.QueryOptions{WorkspaceID: "workspace", ProjectID: "project", RepoAllow: map[string]bool{"repo": true}},
		24,
	)
	if result.err != nil {
		t.Fatalf("search: %v", result.err)
	}
	paths := make(map[string]bool, len(result.matches))
	for _, match := range result.matches {
		paths[match.Path] = true
	}
	if !paths["new.go"] || !paths["keep.go"] {
		t.Fatalf("overlay-only/durable match missing: %#v", result.matches)
	}
	if paths["replace.go"] || paths["delete.go"] {
		t.Fatalf("covered durable match leaked: %#v", result.matches)
	}
}

func TestSearchExploreSourceLiteralUsesPinnedCohortAfterPush(t *testing.T) {
	server, sessionID := newExploreSourceLiteralOverlayTestServer(t, nil)
	pushExploreSourceLiteralOverlay(t, server, sessionID, "new.go",
		"package repo\nvar _ = \"needle\"\n", false)
	ctx := prepareExploreSourceLiteralOverlayContext(t, server, sessionID)
	pushExploreSourceLiteralOverlay(t, server, sessionID, "new.go",
		"package repo\nvar _ = \"other\"\n", false)

	result := server.searchExploreSourceLiteral(ctx, "needle", "repo",
		query.QueryOptions{RepoAllow: map[string]bool{"repo": true}}, 24)
	if result.err != nil {
		t.Fatalf("search: %v", result.err)
	}
	if len(result.matches) != 1 || result.matches[0].Path != "new.go" {
		t.Fatalf("pinned cohort changed after push: %#v", result.matches)
	}
}

func TestSearchExploreSourceLiteralOutOfScopeReplacementStillMasksDurable(t *testing.T) {
	for _, test := range []struct {
		name  string
		path  string
		scope query.QueryOptions
	}{
		{name: "project", path: "replace.go", scope: query.QueryOptions{
			WorkspaceID: "workspace", ProjectID: "other-project", RepoAllow: map[string]bool{"repo": true},
		}},
		{name: "test", path: "replace_test.go", scope: query.QueryOptions{
			WorkspaceID: "workspace", RepoAllow: map[string]bool{"repo": true}, ExcludeTests: true,
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			server, sessionID := newExploreSourceLiteralOverlayTestServer(t, map[string]string{
				test.path: "package repo\nvar _ = \"needle\"\n",
			})
			pushExploreSourceLiteralOverlay(t, server, sessionID, test.path,
				"package repo\nvar _ = \"needle\"\n", false)
			ctx := prepareExploreSourceLiteralOverlayContext(t, server, sessionID)
			files, covered, incomplete, overflow, err := server.snapshotExploreSourceLiteralOverlays(ctx, test.scope, "repo")
			if err != nil || incomplete || overflow {
				t.Fatalf("snapshot err/incomplete/overflow = %v/%t/%t", err, incomplete, overflow)
			}
			if _, ok := covered[test.path]; !ok {
				t.Fatalf("covered paths = %#v, want %q", covered, test.path)
			}
			if len(files) != 1 || files[0].eligible {
				t.Fatalf("overlay files = %#v, want one ineligible replacement", files)
			}
			result := server.searchExploreSourceLiteral(ctx, "needle", "repo", test.scope, 24)
			if result.err != nil {
				t.Fatalf("search: %v", result.err)
			}
			if len(result.matches) != 0 {
				t.Fatalf("out-of-scope replacement leaked or durable was not masked: %#v", result.matches)
			}
		})
	}
}

func TestSearchExploreSourceLiteralCompensatesCoveredLeadingDurableRows(t *testing.T) {
	disk := make(map[string]string)
	for index := 0; index < 50; index++ {
		disk[fmt.Sprintf("a%02d.go", index)] = "package repo\nvar _ = \"needle\"\n"
	}
	disk["z-visible.go"] = "package repo\nvar _ = \"needle\"\n"
	server, sessionID := newExploreSourceLiteralOverlayTestServer(t, disk)
	for index := 0; index < 50; index++ {
		pushExploreSourceLiteralOverlay(t, server, sessionID, fmt.Sprintf("a%02d.go", index), "package repo\n", false)
	}

	result := server.searchExploreSourceLiteral(
		prepareExploreSourceLiteralOverlayContext(t, server, sessionID), "needle", "repo",
		query.QueryOptions{WorkspaceID: "workspace", RepoAllow: map[string]bool{"repo": true}},
		1,
	)
	if result.err != nil {
		t.Fatalf("search: %v", result.err)
	}
	if len(result.matches) != 1 || result.matches[0].Path != "z-visible.go" {
		t.Fatalf("compensated matches = %#v", result.matches)
	}
}

func newExploreSourceLiteralMultiRepoTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	store := graph.New()
	reg := parser.NewRegistry()
	languages.RegisterAll(reg)
	base := indexer.New(store, reg, config.IndexConfig{}, zap.NewNop())
	managerPath := filepath.Join(t.TempDir(), "missing-global.yaml")
	configManager, err := config.NewConfigManager(managerPath)
	if err != nil {
		t.Fatalf("config manager: %v", err)
	}
	multi := indexer.NewMultiIndexer(store, reg, base.Search(), configManager, zap.NewNop())
	for _, prefix := range []string{"target", "other"} {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "disk.go"), []byte("package repo\nvar _ = \"needle\"\n"), 0o644); err != nil {
			t.Fatalf("write %s: %v", prefix, err)
		}
		if _, err := multi.TrackRepo(config.RepoEntry{
			Name: prefix, Path: root, Workspace: "workspace", Project: "project",
		}); err != nil {
			t.Fatalf("track %s: %v", prefix, err)
		}
	}
	engine := query.NewEngine(store)
	server := NewServer(engine, store, nil, nil, zap.NewNop(), nil, MultiRepoOptions{
		MultiIndexer: multi, ConfigManager: configManager,
	})
	manager := daemon.NewOverlayManager(time.Hour)
	server.SetOverlayManager(manager)
	const sessionID = "literal-overlay-multi-test"
	if err := manager.RegisterWithID(sessionID, "workspace"); err != nil {
		t.Fatalf("register overlay: %v", err)
	}
	return server, sessionID
}

func TestSearchExploreSourceLiteralIsolatesTargetRepoOverlays(t *testing.T) {
	server, sessionID := newExploreSourceLiteralMultiRepoTestServer(t)
	for index := 0; index < 30; index++ {
		pushExploreSourceLiteralOverlay(t, server, sessionID,
			fmt.Sprintf("other/overlay-%02d.go", index), "package other\nvar _ = \"needle\"\n", false)
	}

	result := server.searchExploreSourceLiteral(
		prepareExploreSourceLiteralOverlayContext(t, server, sessionID), "needle", "target", query.QueryOptions{}, 1,
	)
	if result.err != nil {
		t.Fatalf("search: %v", result.err)
	}
	if result.incomplete {
		t.Fatal("unrelated overlay volume marked target search incomplete")
	}
	if len(result.matches) != 1 || result.matches[0].Path != "target/disk.go" {
		t.Fatalf("cross-repo overlay leaked or target starved: %#v", result.matches)
	}
}

func TestOverlayRequestCanonicalizesMultiRepoAliasesAcrossConsumers(t *testing.T) {
	server, sessionID := newExploreSourceLiteralMultiRepoTestServer(t)
	target := server.multiIndexer.GetIndexer("target")
	if target == nil {
		t.Fatal("target indexer missing")
	}
	absPath := filepath.Join(target.RootPath(), "disk.go")
	const content = "package repo\nvar _ = \"overlay_alias\"\n"
	for _, alias := range []string{absPath, "target/disk.go"} {
		pushExploreSourceLiteralOverlay(t, server, sessionID, alias, content, false)
	}

	ctx := prepareExploreSourceLiteralOverlayContext(t, server, sessionID)
	snapshot, ok := overlayRequestSnapshotFromContext(ctx)
	if !ok || !snapshot.canonical || len(snapshot.files) != 1 {
		t.Fatalf("canonical snapshot = %#v", snapshot)
	}
	if snapshot.files[0].Path != "target/disk.go" {
		t.Fatalf("canonical path = %q", snapshot.files[0].Path)
	}
	if got, found := server.overlayContentFor(ctx, absPath); !found || got != content {
		t.Fatalf("overlay content = %q, %v", got, found)
	}
	result := server.searchExploreSourceLiteral(
		ctx, "overlay_alias", "target", query.QueryOptions{RepoAllow: map[string]bool{"target": true}}, 1,
	)
	if result.err != nil || len(result.matches) != 1 || result.matches[0].Path != "target/disk.go" {
		t.Fatalf("literal result = %#v, err=%v", result.matches, result.err)
	}
}

func TestSearchExploreSourceLiteralSessionProjectDoesNotHideSiblingProject(t *testing.T) {
	server, sessionID := newExploreSourceLiteralMultiRepoTestServer(t)
	home := server.multiIndexer.GetIndexer("target")
	sibling := server.multiIndexer.GetIndexer("other")
	if home == nil || sibling == nil {
		t.Fatal("multi-repo test indexers missing")
	}
	home.SetProjectID("project-a")
	sibling.SetProjectID("project-b")
	pushExploreSourceLiteralOverlay(t, server, sessionID, "other/new.go",
		"package other\nvar _ = \"needle\"\n", false)
	baseCtx := WithSessionCWD(WithSessionID(context.Background(), sessionID), home.RootPath())
	ctx, _, err := server.prepareOverlayRequest(baseCtx)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}

	effective, ok := server.effectiveExploreSourceLiteralScope(ctx, query.QueryOptions{})
	if !ok || effective.ProjectID != "" || effective.WorkspaceID != "workspace" {
		t.Fatalf("effective sibling-project scope = %#v, %v", effective, ok)
	}
	result := server.searchExploreSourceLiteral(ctx, "needle", "other", query.QueryOptions{}, 1)
	if result.err != nil {
		t.Fatalf("search: %v", result.err)
	}
	if len(result.matches) != 1 || !strings.HasPrefix(result.matches[0].Path, "other/") {
		t.Fatalf("session home project hid sibling-project result: %#v", result.matches)
	}
	mismatch := server.searchExploreSourceLiteral(ctx, "needle", "other", query.QueryOptions{
		RepoAllow: map[string]bool{"target": true},
	}, 1)
	if mismatch.owned || len(mismatch.matches) != 0 || mismatch.backend != "multi-ambiguous-scope" {
		t.Fatalf("disjoint request narrow escaped scope: %#v", mismatch)
	}
}

func TestResolveExploreSourceLiteralRepoPrefixRejectsScopeMismatch(t *testing.T) {
	server, _ := newExploreSourceLiteralOverlayTestServer(t, nil)
	if prefix, ok := server.resolveExploreSourceLiteralRepoPrefix("repo", query.QueryOptions{
		RepoAllow: map[string]bool{"other": true},
	}); ok || prefix != "" {
		t.Fatalf("mismatched explicit prefix = %q, %v", prefix, ok)
	}
}

func TestSearchExploreSourceLiteralRejectsUnresolvedOverlayOwnership(t *testing.T) {
	server, sessionID := newExploreSourceLiteralMultiRepoTestServer(t)
	pushExploreSourceLiteralOverlay(t, server, sessionID, "ambiguous.go", "needle\n", false)
	_, _, err := server.prepareOverlayRequest(WithSessionID(context.Background(), sessionID))
	if err == nil {
		t.Fatal("unresolved overlay ownership did not fail request preparation")
	}
}

func TestSearchExploreSourceLiteralFailsClosedAboveCoveredPathCap(t *testing.T) {
	server, sessionID := newExploreSourceLiteralOverlayTestServer(t, map[string]string{
		"visible.go": "package repo\nvar _ = \"needle\"\n",
	})
	for index := 0; index <= exploreSourceLiteralOverlayMaxCoveredFiles; index++ {
		pushExploreSourceLiteralOverlay(t, server, sessionID,
			fmt.Sprintf("covered-%03d.go", index), "package repo\n", false)
	}
	result := server.searchExploreSourceLiteral(
		prepareExploreSourceLiteralOverlayContext(t, server, sessionID), "needle", "repo",
		query.QueryOptions{RepoAllow: map[string]bool{"repo": true}}, 24,
	)
	if result.err != nil {
		t.Fatalf("search: %v", result.err)
	}
	if !result.incomplete || len(result.matches) != 0 {
		t.Fatalf("covered-cap result: matches=%#v incomplete=%v", result.matches, result.incomplete)
	}
	if result.backend != "overlay-covered-cap" {
		t.Fatalf("backend = %q", result.backend)
	}
}

func TestSearchExploreSourceLiteralAllowsExactlyCoveredPathCap(t *testing.T) {
	server, sessionID := newExploreSourceLiteralOverlayTestServer(t, map[string]string{
		"visible.go": "package repo\nvar _ = \"needle\"\n",
	})
	for index := 0; index < exploreSourceLiteralOverlayMaxCoveredFiles; index++ {
		pushExploreSourceLiteralOverlay(t, server, sessionID,
			fmt.Sprintf("covered-%03d.go", index), "package repo\n", false)
	}
	result := server.searchExploreSourceLiteral(
		prepareExploreSourceLiteralOverlayContext(t, server, sessionID), "needle", "repo",
		query.QueryOptions{RepoAllow: map[string]bool{"repo": true}}, 1,
	)
	if result.err != nil {
		t.Fatalf("search: %v", result.err)
	}
	if result.incomplete || len(result.matches) != 1 || result.matches[0].Path != "visible.go" {
		t.Fatalf("exact-cap result: matches=%#v incomplete=%v", result.matches, result.incomplete)
	}
}

func TestSearchExploreSourceLiteralRejectsViewWithoutPinnedSnapshot(t *testing.T) {
	server, _ := newExploreSourceLiteralOverlayTestServer(t, map[string]string{
		"visible.go": "package repo\nvar _ = \"needle\"\n",
	})
	ctx := WithOverlayView(context.Background(), graph.NewOverlaidView(server.graph, graph.NewOverlayLayer()))
	result := server.searchExploreSourceLiteral(ctx, "needle", "repo", query.QueryOptions{}, 1)
	if result.err == nil || len(result.matches) != 0 || !result.incomplete {
		t.Fatalf("missing snapshot result: matches=%#v incomplete=%v err=%v", result.matches, result.incomplete, result.err)
	}
}

func TestSearchExploreSourceLiteralUsesCanonicalAliasCohort(t *testing.T) {
	server, sessionID := newExploreSourceLiteralOverlayTestServer(t, map[string]string{
		"alias.go": "package repo\nvar _ = \"needle\"\n",
	})
	const overlayContent = "package repo\nvar _ = \"other\"\n"
	pushExploreSourceLiteralOverlay(t, server, sessionID, "./alias.go", overlayContent, false)
	pushExploreSourceLiteralOverlay(t, server, sessionID, "alias.go", overlayContent, false)
	ctx := prepareExploreSourceLiteralOverlayContext(t, server, sessionID)
	snapshot, ok := overlayRequestSnapshotFromContext(ctx)
	if !ok || !snapshot.canonical || len(snapshot.files) != 1 {
		t.Fatalf("canonical snapshot = %#v", snapshot)
	}
	result := server.searchExploreSourceLiteral(
		ctx, "needle", "repo", query.QueryOptions{RepoAllow: map[string]bool{"repo": true}}, 1,
	)
	if result.err != nil {
		t.Fatalf("search: %v", result.err)
	}
	if len(result.matches) != 0 {
		t.Fatalf("canonical alias did not mask durable content: %#v", result.matches)
	}
}

func TestSnapshotExploreSourceLiteralOverlaysCollapsesCanonicalAliases(t *testing.T) {
	server, sessionID := newExploreSourceLiteralOverlayTestServer(t, nil)
	for index := 0; index <= exploreSourceLiteralOverlayMaxCoveredFiles; index++ {
		pushExploreSourceLiteralOverlay(t, server, sessionID,
			strings.Repeat("./", index+1)+"alias.go", "package repo\n", false)
	}
	ctx := prepareExploreSourceLiteralOverlayContext(t, server, sessionID)
	files, covered, incomplete, overflow, err := server.snapshotExploreSourceLiteralOverlays(
		ctx, query.QueryOptions{RepoAllow: map[string]bool{"repo": true}}, "repo",
	)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if incomplete || overflow || len(files) != 1 || len(covered) != 1 {
		t.Fatalf("alias cohort: files=%d covered=%d incomplete=%v overflow=%v", len(files), len(covered), incomplete, overflow)
	}
}

func TestResolveExploreSourceLiteralRepoPrefixRejectsDirectMismatch(t *testing.T) {
	server, _ := newExploreSourceLiteralOverlayTestServer(t, nil)
	if prefix, ok := server.resolveExploreSourceLiteralRepoPrefix("other", query.QueryOptions{}); ok || prefix != "" {
		t.Fatalf("mismatched direct prefix = %q, %v", prefix, ok)
	}
}

func TestSearchExploreSourceLiteralNormalizesHitLimits(t *testing.T) {
	diskFiles := make(map[string]string, 30)
	for i := 0; i < 30; i++ {
		diskFiles[fmt.Sprintf("file-%02d.go", i)] = `const value = "needle"`
	}
	server, sessionID := newExploreSourceLiteralOverlayTestServer(t, diskFiles)
	ctx := prepareExploreSourceLiteralOverlayContext(t, server, sessionID)
	scope := query.QueryOptions{RepoAllow: map[string]bool{"repo": true}}

	for _, maxHits := range []int{0, -1, 10_000} {
		t.Run(fmt.Sprintf("max_hits_%d", maxHits), func(t *testing.T) {
			got := server.searchExploreSourceLiteral(ctx, "needle", "repo", scope, maxHits)
			if got.err != nil {
				t.Fatalf("search: %v", got.err)
			}
			if len(got.matches) != exploreSourceLiteralOverlayMaxHits || !got.incomplete {
				t.Fatalf("matches/incomplete = %d/%t, want %d/true", len(got.matches), got.incomplete, exploreSourceLiteralOverlayMaxHits)
			}
		})
	}
}

func TestSearchExploreSourceLiteralAppliesScopeAndCancellation(t *testing.T) {
	server, sessionID := newExploreSourceLiteralOverlayTestServer(t, nil)
	pushExploreSourceLiteralOverlay(t, server, sessionID, "new.go", "package repo\nvar _ = \"needle\"\n", false)
	ctx := prepareExploreSourceLiteralOverlayContext(t, server, sessionID)

	outOfScope := server.searchExploreSourceLiteral(ctx, "needle", "repo", query.QueryOptions{
		WorkspaceID: "other", RepoAllow: map[string]bool{"repo": true},
	}, 24)
	if outOfScope.err != nil {
		t.Fatalf("scoped search: %v", outOfScope.err)
	}
	if len(outOfScope.matches) != 0 {
		t.Fatalf("out-of-scope overlay leaked: %#v", outOfScope.matches)
	}

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	result := server.searchExploreSourceLiteral(cancelled, "needle", "repo", query.QueryOptions{}, 24)
	if result.err == nil {
		t.Fatal("cancelled search did not return an error")
	}
}
