package store_sqlite

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search"
)

const ftsNormalizationTestModeEnv = "GORTEX_FTS_NORMALIZATION_TEST_MODE"

func TestSymbolFTSNormalizationStateLifecycle(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "fts-state.sqlite"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	if mode, ok, err := s.GetSymbolFTSNormalization("repo-a"); err != nil || ok || mode != "" {
		t.Fatalf("missing repo-a marker = (%q, %v, %v), want (\"\", false, nil)", mode, ok, err)
	}
	if err := s.SetSymbolFTSNormalization("repo-a", "porter-v1"); err != nil {
		t.Fatalf("SetSymbolFTSNormalization(repo-a): %v", err)
	}
	if err := s.SetSymbolFTSNormalization("repo-b", ""); err != nil {
		t.Fatalf("SetSymbolFTSNormalization(repo-b): %v", err)
	}
	const opaqueMode = "future/vendor-v7+opaque"
	if err := s.SetSymbolFTSNormalization("repo-opaque", opaqueMode); err != nil {
		t.Fatalf("SetSymbolFTSNormalization(repo-opaque): %v", err)
	}
	assertFTSNormalizationState(t, s, "repo-a", "porter-v1", true)
	assertFTSNormalizationState(t, s, "repo-b", "", true)
	assertFTSNormalizationState(t, s, "repo-opaque", opaqueMode, true)

	if err := s.SetSymbolFTSNormalization("repo-a", "porter-v2"); err != nil {
		t.Fatalf("overwrite repo-a marker: %v", err)
	}
	assertFTSNormalizationState(t, s, "repo-a", "porter-v2", true)
	assertFTSNormalizationState(t, s, "repo-b", "", true)

	if err := s.SetSymbolFTSNormalization("orphan", "porter-v1"); err != nil {
		t.Fatalf("set orphan marker: %v", err)
	}
	if err := s.SetSymbolFTSNormalization("", "porter-v1"); err != nil {
		t.Fatalf("set protected empty-prefix marker: %v", err)
	}
	orphans := s.OrphanRepoPrefixes([]string{"repo-a", "repo-b", "repo-opaque"})
	if !slices.Equal(orphans, []string{"orphan"}) {
		t.Fatalf("OrphanRepoPrefixes = %v, want [orphan] (empty prefix must stay protected)", orphans)
	}

	if err := s.PurgeRepo("repo-a"); err != nil {
		t.Fatalf("PurgeRepo(repo-a): %v", err)
	}
	assertFTSNormalizationState(t, s, "repo-a", "", false)
	assertFTSNormalizationState(t, s, "repo-b", "", true)

	if err := s.SetSymbolFTSNormalization("old", "porter-v1"); err != nil {
		t.Fatalf("set old marker: %v", err)
	}
	if err := s.SetSymbolFTSNormalization("new", "stale-mode"); err != nil {
		t.Fatalf("set destination marker: %v", err)
	}
	if err := s.RekeyRepoPrefix("old", "new"); err != nil {
		t.Fatalf("RekeyRepoPrefix: %v", err)
	}
	// Rekey drops the symbol FTS corpus because node IDs change. Neither the
	// source marker nor a stale destination marker may survive and falsely
	// certify that the newly empty corpus was built in a particular mode.
	assertFTSNormalizationState(t, s, "old", "", false)
	assertFTSNormalizationState(t, s, "new", "", false)
}

func assertFTSNormalizationState(t *testing.T, s *Store, repo, wantMode string, wantOK bool) {
	t.Helper()
	mode, ok, err := s.GetSymbolFTSNormalization(repo)
	if err != nil {
		t.Fatalf("GetSymbolFTSNormalization(%q): %v", repo, err)
	}
	if mode != wantMode || ok != wantOK {
		t.Fatalf("GetSymbolFTSNormalization(%q) = (%q, %v), want (%q, %v)", repo, mode, ok, wantMode, wantOK)
	}
}

func TestFTSNormalizationWriteQueryAndContentParity(t *testing.T) {
	if mode := os.Getenv(ftsNormalizationTestModeEnv); mode != "" {
		runFTSNormalizationModeAssertions(t, mode == "enabled")
		return
	}

	for _, tc := range []struct {
		name     string
		stemming string
	}{
		{name: "disabled", stemming: "0"},
		{name: "enabled", stemming: "1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestFTSNormalizationWriteQueryAndContentParity$")
			cmd.Env = ftsNormalizationTestEnv(tc.name, tc.stemming)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("subprocess failed: %v\n%s", err, out)
			}
		})
	}
}

func ftsNormalizationTestEnv(mode, stemming string) []string {
	env := make([]string, 0, len(os.Environ())+2)
	for _, entry := range os.Environ() {
		if strings.HasPrefix(entry, ftsNormalizationTestModeEnv+"=") || strings.HasPrefix(entry, "GORTEX_FTS_STEMMING=") {
			continue
		}
		env = append(env, entry)
	}
	return append(env, ftsNormalizationTestModeEnv+"="+mode, "GORTEX_FTS_STEMMING="+stemming)
}

func runFTSNormalizationModeAssertions(t *testing.T, enabled bool) {
	wantMode := ""
	wantSymbolMatch := `"the"* OR "manager"*`
	if enabled {
		wantMode = "porter-v1"
		wantSymbolMatch = `"manag"*`
	}
	if got := search.FTSNormalizationMode(); got != wantMode {
		t.Fatalf("FTSNormalizationMode = %q, want %q", got, wantMode)
	}

	s, err := Open(filepath.Join(t.TempDir(), "fts-mode.sqlite"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	if got := s.buildFTSMatch("the manager", true); got != wantSymbolMatch {
		t.Fatalf("normalized symbol MATCH = %q, want %q", got, wantSymbolMatch)
	}
	if got := s.buildFTSMatch("the manager", false); got != `"the"* OR "manager"*` {
		t.Fatalf("raw content MATCH = %q, want unnormalised terms", got)
	}

	const symbolID = "repo/worker.go::WorkerCatalog"
	s.AddNode(&graph.Node{
		ID: symbolID, Kind: graph.KindType, Name: "WorkerCatalog",
		FilePath: "repo/worker.go", Language: "go", RepoPrefix: "repo",
	})
	writeTokens := search.NormalizeFTSTokens(search.Tokenize("the manage"))
	if err := s.UpsertSymbolFTS(symbolID, strings.Join(writeTokens, " ")); err != nil {
		t.Fatalf("UpsertSymbolFTS: %v", err)
	}

	hits, err := s.SearchSymbols("manager", 10)
	if err != nil {
		t.Fatalf("SearchSymbols(manager): %v", err)
	}
	if enabled && (len(hits) != 1 || hits[0].NodeID != symbolID) {
		t.Fatalf("enabled stemming hits = %+v, want %q", hits, symbolID)
	}
	if !enabled && len(hits) != 0 {
		t.Fatalf("disabled stemming hits = %+v, want no morphological match", hits)
	}

	hits, err = s.SearchSymbols("the", 10)
	if err != nil {
		t.Fatalf("SearchSymbols(stopword): %v", err)
	}
	if enabled && len(hits) != 0 {
		t.Fatalf("enabled stopword query hits = %+v, want none", hits)
	}
	if !enabled && (len(hits) != 1 || hits[0].NodeID != symbolID) {
		t.Fatalf("disabled stopword query hits = %+v, want raw-token hit", hits)
	}

	const contentBody = "the managers are running"
	const contentID = "repo/readme.md::doc:section-0"
	if err := s.AppendContent("repo", []graph.ContentFTSItem{{
		NodeID: contentID, FilePath: "repo/readme.md", Body: contentBody,
	}}); err != nil {
		t.Fatalf("AppendContent: %v", err)
	}
	contentHits, err := s.SearchContent("the", "repo", 10)
	if err != nil {
		t.Fatalf("SearchContent(stopword): %v", err)
	}
	if len(contentHits) != 1 || contentHits[0].NodeID != contentID {
		t.Fatalf("content stopword hits = %+v, want raw content hit in both modes", contentHits)
	}
	var storedBody string
	if err := s.db.QueryRow(`SELECT body FROM content_fts WHERE node_id = ?`, contentID).Scan(&storedBody); err != nil {
		t.Fatalf("read raw content_fts body: %v", err)
	}
	if storedBody != contentBody {
		t.Fatalf("content_fts body = %q, want raw %q", storedBody, contentBody)
	}
}
