package hooks

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/profiles"
)

// TestMain ensures hook tests never write telemetry to the user's real
// ~/.cache/gortex/hook-decisions.jsonl. Tests that want to inspect the
// log redirect it again via t.Setenv to a per-test tmp file.
func TestMain(m *testing.M) {
	// Pin the instruction profile so a developer machine that ran
	// `gortex instructions switch` cannot change hook-tier behavior
	// in unrelated tests; tier tests stub activeHookTier directly.
	_ = os.Setenv(profiles.ActiveEnv, profiles.DefaultName)
	dir, err := os.MkdirTemp("", "gortex-hooks-test")
	if err == nil {
		_ = os.Setenv("GORTEX_HOOK_LOG", filepath.Join(dir, "hook-decisions.jsonl"))
		defer func() { _ = os.RemoveAll(dir) }()
	}
	// Default the file-indexed / file-summary probes to "not indexed" so no
	// test dials a real daemon. Tests needing an indexed verdict stub
	// fileIndexedFn / fileSummaryFn (fakeIndexedBridge / newIndexedBridge /
	// stubBridge) and restore these defaults on cleanup.
	fileIndexedFn = func(_, _ string) (bool, int) { return false, 0 }
	fileSummaryFn = func(_, _ string) (*hookFileSummary, bool) { return nil, false }
	os.Exit(m.Run())
}
