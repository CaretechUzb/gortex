package languages

import (
	"fmt"
	"sync"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

func temporalOptionSource(helper, reassignment string) []byte {
	return []byte(fmt.Sprintf(`package sample
import "go.temporal.io/sdk/workflow"
func Run(ctx workflow.Context) {
	name := %s("ACTIVITY", "MyActivity")
	%s
	workflow.ExecuteActivity(ctx, name)
}
`, helper, reassignment))
}

func findTemporalDispatchMeta(result *parser.ExtractionResult) map[string]any {
	if result == nil {
		return nil
	}
	defer result.ReleaseTree()
	for _, edge := range result.Edges {
		if edge == nil || edge.Kind != graph.EdgeCalls || edge.Meta == nil {
			continue
		}
		if edge.Meta["via"] == "temporal.stub" && edge.Meta["temporal_kind"] == "activity" {
			return edge.Meta
		}
	}
	return nil
}

func temporalDispatchMeta(t *testing.T, result *parser.ExtractionResult) map[string]any {
	t.Helper()
	meta := findTemporalDispatchMeta(result)
	if meta == nil {
		t.Fatal("Temporal activity dispatch edge not found")
	}
	return meta
}

func TestGoExtractorTemporalHelperOptionsExtendBuiltins(t *testing.T) {
	ext := NewGoExtractor()

	builtIn, err := ext.Extract("builtin.go", temporalOptionSource("GetEnvOrDefault", ""))
	if err != nil {
		t.Fatal(err)
	}
	if got := temporalDispatchMeta(t, builtIn)["temporal_env_source"]; got != "allowlist" {
		t.Fatalf("built-in source = %#v", got)
	}

	ordinary, err := ext.Extract("ordinary.go", temporalOptionSource("CorporateHelper", ""))
	if err != nil {
		t.Fatal(err)
	}
	ordinaryMeta := temporalDispatchMeta(t, ordinary)
	if got := ordinaryMeta["temporal_env_source"]; got != nil {
		t.Fatalf("ordinary Extract unexpectedly used local options: %#v", got)
	}

	opts := parser.NewExtractionOptions([]string{"CorporateHelper"})
	configured, err := ext.ExtractWithOptions("configured.go", temporalOptionSource("CorporateHelper", ""), opts)
	if err != nil {
		t.Fatal(err)
	}
	configuredMeta := temporalDispatchMeta(t, configured)
	if got := configuredMeta["temporal_env_source"]; got != "allowlist" {
		t.Fatalf("configured source = %#v", got)
	}
	if got := configuredMeta["temporal_name"]; got != "MyActivity" {
		t.Fatalf("configured target = %#v", got)
	}

	constSource := []byte(`package sample
import "go.temporal.io/sdk/workflow"
const DefaultActivity = "MyActivity"
func Run(ctx workflow.Context) {
	name := CorporateHelper("ACTIVITY", DefaultActivity)
	workflow.ExecuteActivity(ctx, name)
}
`)
	withConst, err := ext.ExtractWithOptions("const.go", constSource, opts)
	if err != nil {
		t.Fatal(err)
	}
	constMeta := temporalDispatchMeta(t, withConst)
	if got := constMeta["temporal_env_source"]; got != "const_ref" {
		t.Fatalf("constant source = %#v", got)
	}
	if got := constMeta["temporal_default_const"]; got != "DefaultActivity" {
		t.Fatalf("constant reference = %#v", got)
	}

	// A repo-local allow-list entry matches its call site case-insensitively,
	// exactly like a built-in goEnvHelperNames entry does. The YAML author
	// must not have to reproduce the call site's capitalisation: a near-miss
	// used to fail silently, dropping the dispatch to the hidden heuristic
	// tier (or off the graph entirely for a name without an "env" marker).
	for _, tc := range []struct{ entry, callSite string }{
		{entry: "corporatehelper", callSite: "CorporateHelper"},
		{entry: "CorporateHelper", callSite: "corporatehelper"},
	} {
		mixedCase, err := ext.ExtractWithOptions(
			"case.go",
			temporalOptionSource(tc.callSite, ""),
			parser.NewExtractionOptions([]string{tc.entry}),
		)
		if err != nil {
			t.Fatal(err)
		}
		if got := temporalDispatchMeta(t, mixedCase)["temporal_env_source"]; got != "allowlist" {
			t.Fatalf("entry %q vs call site %q: source = %#v, want %q",
				tc.entry, tc.callSite, got, "allowlist")
		}
	}
}

func TestGoExtractorTemporalGenericEnvPromotionAndReassignment(t *testing.T) {
	ext := NewGoExtractor()
	src := temporalOptionSource("CorporateEnv", "")

	heuristic, err := ext.Extract("heuristic.go", src)
	if err != nil {
		t.Fatal(err)
	}
	if got := temporalDispatchMeta(t, heuristic)["temporal_env_source"]; got != "heuristic" {
		t.Fatalf("heuristic source = %#v", got)
	}

	opts := parser.NewExtractionOptions([]string{"CorporateEnv"})
	promoted, err := ext.ExtractWithOptions("promoted.go", src, opts)
	if err != nil {
		t.Fatal(err)
	}
	if got := temporalDispatchMeta(t, promoted)["temporal_env_source"]; got != "allowlist" {
		t.Fatalf("promoted source = %#v", got)
	}

	invalidated, err := ext.ExtractWithOptions("invalidated.go", temporalOptionSource("CorporateEnv", "name = choose()"), opts)
	if err != nil {
		t.Fatal(err)
	}
	if got := temporalDispatchMeta(t, invalidated)["temporal_env_source"]; got != nil {
		t.Fatalf("later reassignment retained env source = %#v", got)
	}
}

func TestGoExtractorTemporalOptionsConcurrentIsolation(t *testing.T) {
	ext := NewGoExtractor()
	type tc struct {
		helper string
		other  string
	}
	cases := []tc{{helper: "RepoAHelper", other: "RepoBHelper"}, {helper: "RepoBHelper", other: "RepoAHelper"}}

	var wg sync.WaitGroup
	errs := make(chan error, len(cases)*10)
	for _, testCase := range cases {
		testCase := testCase
		for i := 0; i < 10; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				opts := parser.NewExtractionOptions([]string{testCase.helper})
				own, err := ext.ExtractWithOptions(testCase.helper+".go", temporalOptionSource(testCase.helper, ""), opts)
				if err != nil {
					errs <- err
					return
				}
				ownMeta := findTemporalDispatchMeta(own)
				if ownMeta == nil {
					errs <- fmt.Errorf("%s dispatch edge missing", testCase.helper)
					return
				}
				if got := ownMeta["temporal_env_source"]; got != "allowlist" {
					errs <- fmt.Errorf("%s source = %#v", testCase.helper, got)
					return
				}
				foreign, err := ext.ExtractWithOptions(testCase.other+".go", temporalOptionSource(testCase.other, ""), opts)
				if err != nil {
					errs <- err
					return
				}
				foreignMeta := findTemporalDispatchMeta(foreign)
				if foreignMeta == nil {
					errs <- fmt.Errorf("%s dispatch edge missing", testCase.other)
					return
				}
				if got := foreignMeta["temporal_env_source"]; got != nil {
					errs <- fmt.Errorf("%s contaminated %s: %#v", testCase.helper, testCase.other, got)
				}
			}()
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
}
