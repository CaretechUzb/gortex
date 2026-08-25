package contracts

import (
	"slices"
	"strings"
	"testing"
)

type fakeRoutePass struct {
	name    string
	langs   []string
	detect  func(string, []byte) bool
	extract func(*RouteExtractCtx) []Contract
}

func (p fakeRoutePass) Name() string                          { return p.name }
func (p fakeRoutePass) Languages() []string                   { return p.langs }
func (p fakeRoutePass) Detect(fp string, src []byte) bool     { return p.detect(fp, src) }
func (p fakeRoutePass) Extract(ctx *RouteExtractCtx) []Contract { return p.extract(ctx) }

// saveRegistry snapshots and restores the global registry around a test that
// mutates it.
func saveRegistry(t *testing.T) {
	t.Helper()
	saved := append([]FrameworkRoutePass(nil), frameworkRoutePasses...)
	t.Cleanup(func() { frameworkRoutePasses = saved })
}

func TestApplicableFrameworkRoutePasses_LanguageFilter(t *testing.T) {
	got := ApplicableFrameworkRoutePasses("go")
	for _, p := range got {
		if langs := p.Languages(); len(langs) > 0 && !slices.Contains(langs, "go") {
			t.Errorf("pass %q (langs %v) wrongly applicable to go", p.Name(), langs)
		}
		if p.Name() == "django" {
			t.Errorf("python-only django pass must not apply to go")
		}
	}
	// file-based has no language filter → applies to every language.
	if !slices.ContainsFunc(got, func(p FrameworkRoutePass) bool { return p.Name() == "file-based" }) {
		t.Errorf("file-based pass (all languages) should apply to go")
	}
}

func TestRegisterFrameworkRoutePass_CustomExtension(t *testing.T) {
	saveRegistry(t)
	RegisterFrameworkRoutePass(fakeRoutePass{
		name: "custom", langs: []string{"go"},
		detect: func(fp string, _ []byte) bool { return strings.HasSuffix(fp, ".custom") },
		extract: func(*RouteExtractCtx) []Contract {
			return []Contract{{ID: "http::GET::/custom", Type: ContractHTTP, Role: RoleProvider}}
		},
	})
	out := runFrameworkRoutePasses(&RouteExtractCtx{FilePath: "x.custom", Lang: "go", Src: []byte("anything")})
	if !slices.ContainsFunc(out, func(c Contract) bool { return c.ID == "http::GET::/custom" }) {
		t.Errorf("a custom registered pass should emit its route contract; got %+v", out)
	}
	// A non-matching file path: Detect returns false, no contract.
	none := runFrameworkRoutePasses(&RouteExtractCtx{FilePath: "x.go", Lang: "go", Src: []byte("anything")})
	if slices.ContainsFunc(none, func(c Contract) bool { return c.ID == "http::GET::/custom" }) {
		t.Errorf("custom pass must not fire when Detect is false")
	}
}

func TestRunFrameworkRoutePasses_PanicIsolation(t *testing.T) {
	saveRegistry(t)
	frameworkRoutePasses = nil
	RegisterFrameworkRoutePass(fakeRoutePass{
		name: "boom", langs: nil,
		detect:  func(string, []byte) bool { panic("detect boom") },
		extract: func(*RouteExtractCtx) []Contract { return nil },
	})
	RegisterFrameworkRoutePass(fakeRoutePass{
		name: "ok", langs: nil,
		detect:  func(string, []byte) bool { return true },
		extract: func(*RouteExtractCtx) []Contract { return []Contract{{ID: "http::GET::/ok", Type: ContractHTTP}} },
	})

	out := runFrameworkRoutePasses(&RouteExtractCtx{FilePath: "f", Lang: "go", Src: []byte("x")})
	if !slices.ContainsFunc(out, func(c Contract) bool { return c.ID == "http::GET::/ok" }) {
		t.Errorf("a panicking Detect must not prevent other passes from running; got %+v", out)
	}
	if names := DetectFrameworks("f", []byte("x")); !slices.Contains(names, "ok") {
		t.Errorf("DetectFrameworks should still list ok despite the boom panic; got %v", names)
	}
}

func TestRunFrameworkRoutePasses_ExtractPanicIsolation(t *testing.T) {
	saveRegistry(t)
	frameworkRoutePasses = nil
	RegisterFrameworkRoutePass(fakeRoutePass{
		name: "boom-extract", langs: nil,
		detect:  func(string, []byte) bool { return true },
		extract: func(*RouteExtractCtx) []Contract { panic("extract boom") },
	})
	RegisterFrameworkRoutePass(fakeRoutePass{
		name: "ok", langs: nil,
		detect:  func(string, []byte) bool { return true },
		extract: func(*RouteExtractCtx) []Contract { return []Contract{{ID: "http::GET::/ok", Type: ContractHTTP}} },
	})
	out := runFrameworkRoutePasses(&RouteExtractCtx{FilePath: "f", Lang: "go", Src: []byte("x")})
	if !slices.ContainsFunc(out, func(c Contract) bool { return c.ID == "http::GET::/ok" }) {
		t.Errorf("a panicking Extract must not prevent other passes from running; got %+v", out)
	}
}

func TestRegisteredFrameworkRoutePasses_CoversStructuralPasses(t *testing.T) {
	names := map[string]bool{}
	for _, p := range RegisteredFrameworkRoutePasses() {
		names[p.Name()] = true
	}
	for _, want := range []string{"file-based", "django", "drf", "rails-resources", "express-objects", "flask-decorator", "odoo"} {
		if !names[want] {
			t.Errorf("registry missing structural pass %q", want)
		}
	}
}
