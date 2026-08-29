package contracts

import (
	"slices"
	"testing"

	"github.com/zzet/gortex/internal/frameworkgate"
)

// allowTestPass registers a pass that records whether it was consulted,
// so a test can prove an excluded pass costs nothing at all — not even
// its Detect pre-filter.
func allowTestPass(name string, detected *bool) fakeRoutePass {
	return fakeRoutePass{
		name: name, langs: []string{"go"},
		detect: func(string, []byte) bool { *detected = true; return true },
		extract: func(*RouteExtractCtx) []Contract {
			return []Contract{{ID: "http::GET::/" + name}}
		},
	}
}

func routeIDs(cs []Contract) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.ID)
	}
	return out
}

func TestRunFrameworkRoutePasses_UnsetAllowListRunsEveryPass(t *testing.T) {
	saveRegistry(t)
	frameworkRoutePasses = nil

	var aDetected, bDetected bool
	RegisterFrameworkRoutePass(allowTestPass("alpha", &aDetected))
	RegisterFrameworkRoutePass(allowTestPass("beta", &bDetected))

	out := runFrameworkRoutePasses(&RouteExtractCtx{
		FilePath: "f.go", Lang: "go", Src: []byte("x"),
		H: &HTTPExtractor{},
	})
	if got := routeIDs(out); len(got) != 2 {
		t.Errorf("an unset allow-list must run every pass, got %q", got)
	}
	if !aDetected || !bDetected {
		t.Error("every pass must be consulted when no allow-list is configured")
	}
}

func TestRunFrameworkRoutePasses_AllowListExcludesUnnamedPass(t *testing.T) {
	saveRegistry(t)
	frameworkRoutePasses = nil

	var aDetected, bDetected bool
	RegisterFrameworkRoutePass(allowTestPass("alpha", &aDetected))
	RegisterFrameworkRoutePass(allowTestPass("beta", &bDetected))

	out := runFrameworkRoutePasses(&RouteExtractCtx{
		FilePath: "f.go", Lang: "go", Src: []byte("x"),
		H: &HTTPExtractor{AllowedFrameworks: frameworkgate.New([]string{"alpha"})},
	})
	got := routeIDs(out)
	if !slices.Contains(got, "http::GET::/alpha") {
		t.Errorf("the allowed pass must run, got %q", got)
	}
	if slices.Contains(got, "http::GET::/beta") {
		t.Errorf("an unnamed pass must not run, got %q", got)
	}
	// The exclusion is checked ahead of Detect, so the excluded pass
	// does not even pay for its content pre-filter.
	if bDetected {
		t.Error("an excluded pass must not have its Detect called")
	}
	if !aDetected {
		t.Error("the allowed pass must still be detected normally")
	}
}

func TestRunFrameworkRoutePasses_AllowListPrefixWildcard(t *testing.T) {
	saveRegistry(t)
	frameworkRoutePasses = nil

	var a, b, c bool
	RegisterFrameworkRoutePass(allowTestPass("odoo", &a))
	RegisterFrameworkRoutePass(allowTestPass("odoo-web", &b))
	RegisterFrameworkRoutePass(allowTestPass("django", &c))

	out := runFrameworkRoutePasses(&RouteExtractCtx{
		FilePath: "f.go", Lang: "go", Src: []byte("x"),
		H: &HTTPExtractor{AllowedFrameworks: frameworkgate.New([]string{"odoo*"})},
	})
	got := routeIDs(out)
	if !slices.Contains(got, "http::GET::/odoo") || !slices.Contains(got, "http::GET::/odoo-web") {
		t.Errorf("odoo* must admit both odoo passes, got %q", got)
	}
	if slices.Contains(got, "http::GET::/django") {
		t.Errorf("odoo* must not admit django, got %q", got)
	}
}

func TestRunFrameworkRoutePasses_NoneSentinelRunsNothing(t *testing.T) {
	saveRegistry(t)
	frameworkRoutePasses = nil

	var detected bool
	RegisterFrameworkRoutePass(allowTestPass("alpha", &detected))

	out := runFrameworkRoutePasses(&RouteExtractCtx{
		FilePath: "f.go", Lang: "go", Src: []byte("x"),
		H: &HTTPExtractor{AllowedFrameworks: frameworkgate.AllowNone()},
	})
	if len(out) != 0 {
		t.Errorf("the none sentinel must run no pass, got %q", routeIDs(out))
	}
	if detected {
		t.Error("the none sentinel must not consult any Detect")
	}
}

// ApplicableFrameworkRoutePasses is the inventory read behind
// `analyze route_frameworks`; policy lives at the driver, so the registry
// query must stay total regardless of configuration.
func TestApplicableFrameworkRoutePasses_IgnoresAllowList(t *testing.T) {
	saveRegistry(t)
	frameworkRoutePasses = nil

	var detected bool
	RegisterFrameworkRoutePass(allowTestPass("alpha", &detected))

	if got := ApplicableFrameworkRoutePasses("go"); len(got) != 1 {
		t.Errorf("the registry query must list every registered pass, got %d", len(got))
	}
}
