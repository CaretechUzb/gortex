package contracts

import (
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

const htmxFixture = `<!doctype html>
<html><body>
<tr><button hx-get="/ui/parts/{{.P.ID}}/exp">Export</button></tr>
<tr><button hx-post="/ui/parts/{{.P.ID}}/reset?t=1">Reset</button></tr>
<tr><button hx-delete='/ui/parts/{{.P.ID}}' hx-get="/health">Check</button></tr>
<a hx-get="#section">jump</a>
<a hx-post="javascript:void(0)">noop</a>
<a hx-put="{{ if .Edit }}/edit{{ else }}/new{{ end }}">dyn</a>
<a hx-get="">empty</a>
<div hx-get="/ui/parts/{{.P.ID}}/exp">dup same path other element</div>
<a hx-get="?sort=mpn&dir=asc">sort</a>
<!-- <button hx-get="/commented/route"> -->
<!--
<button hx-get="/also/commented">inside multi-line comment</button>
-->
<a hx-get="/after-comment-block">after multi-line comment</a>
<a hx-get="/api/{{if .V2}}/v2{{else}}/v1{{end}}/items">cond</a>
<a hx-get=" ?sort=mpn&dir=asc">leading-space sort</a>
<a hx-get="JavaScript:void(0)">JS uri</a>
<button data-hx-get="/health2">data- prefixed</button>
</body></html>
`

func TestHtmxExtractor_SupportedLanguages(t *testing.T) {
	got := (&HtmxExtractor{}).SupportedLanguages()
	want := []string{"html", "gotmpl", "templ"}
	if len(got) != len(want) {
		t.Fatalf("SupportedLanguages() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("SupportedLanguages()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestHtmxExtractor_Extract(t *testing.T) {
	fileNodes := []*graph.Node{{ID: "ui/internal/templates/parts.html", Kind: graph.KindFile}}
	out := (&HtmxExtractor{}).Extract("ui/internal/templates/parts.html", []byte(htmxFixture), fileNodes, nil)
	if len(out) != 7 {
		t.Fatalf("got %d contracts, want 7 (5 distinct ids + the line-3/line-10 re-occurrence pair + data-hx pin; none from skip or commented shapes): %+v", len(out), out)
	}

	byID := make(map[string]Contract)
	for _, c := range out {
		byID[c.ID] = c
	}

	// The full expected ID set. Export + the duplicate div share one
	// contract ID (dup is a second occurrence of the same route; both
	// recorded, keyed by ID here). /after-comment-block proves the
	// multi-line comment strip (line 16, true to the file), /health2
	// pins the data-hx-* prefix form.
	wantIDs := map[string]bool{
		"http::GET::/ui/parts/{p1}/exp":    false,
		"http::POST::/ui/parts/{p1}/reset": false,
		"http::DELETE::/ui/parts/{p1}":     false,
		"http::GET::/health":               false,
		"http::GET::/after-comment-block":  false,
		"http::GET::/health2":              false,
	}
	for id := range wantIDs {
		c, ok := byID[id]
		if !ok {
			t.Fatalf("missing contract %q; got IDs %v", id, keysOf(byID))
		}
		if c.Role != RoleConsumer || c.Type != ContractHTTP {
			t.Fatalf("%q: role=%v type=%v, want consumer/http", id, c.Role, c.Type)
		}
		if c.FilePath != "ui/internal/templates/parts.html" {
			t.Fatalf("%q: FilePath=%q", id, c.FilePath)
		}
		if c.Meta["framework"] != "htmx" || c.Meta["raw_path"] == nil {
			t.Fatalf("%q: Meta=%v", id, c.Meta)
		}
		if c.Meta["method"] == nil || c.Meta["method"] == "" {
			t.Fatalf("%q: Meta[method] empty", id)
		}
		if c.Confidence != 0.9 {
			t.Fatalf("%q: Confidence=%v, want 0.9", id, c.Confidence)
		}
	}

	// Query string stripped before normalization.
	if c := byID["http::POST::/ui/parts/{p1}/reset"]; c.Meta["raw_path"] != "/ui/parts/{{.P.ID}}/reset" {
		t.Fatalf("query not stripped: raw_path=%v", c.Meta["raw_path"])
	}

	// Skipped shapes produce nothing — including a query-only value, whose
	// stripped raw_path ("") must never reach a contract, and the
	// leading-space variant whose untrimmed form used to bypass the guard.
	for _, id := range byID {
		if id.Meta["raw_path"] == "#section" || id.Meta["raw_path"] == "" ||
			id.Meta["raw_path"] == "?sort=mpn&dir=asc" ||
			id.Meta["raw_path"] == "javascript:void(0)" ||
			id.Meta["raw_path"] == "JavaScript:void(0)" ||
			id.Meta["raw_path"] == "/commented/route" ||
			id.Meta["raw_path"] == "/also/commented" ||
			id.Meta["raw_path"] == "/api/{{if .V2}}/v2{{else}}/v1{{end}}/items" ||
			id.Meta["raw_path"] == "{{ if .Edit }}/edit{{ else }}/new{{ end }}" {
			t.Fatalf("skip-shape extracted: %+v", id)
		}
	}
	// No contract for this fixture may normalize to the root path: a
	// query-only hx value must not widen to a junk http::<VERB>::/
	// consumer that could falsely pair with a real homepage provider.
	for id := range byID {
		if strings.HasSuffix(id, "::/") {
			t.Fatalf("root-path contract %q extracted from fixture", id)
		}
	}

	// Every contract anchors on the file node (no enclosing symbol in HTML).
	for id, c := range byID {
		if c.SymbolID != "ui/internal/templates/parts.html" {
			t.Fatalf("%q: SymbolID=%q, want file-node fallback", id, c.SymbolID)
		}
	}

	// Line numbers point at the attributes and are computed from the
	// ORIGINAL source (lineNumber is 1-based), even though scanning runs
	// on the comment-stripped copy: /health sits on line 5, the
	// after-comment-block attribute on line 16 — AFTER a comment
	// spanning lines 13-15, whose equal-length space replacement keeps
	// every byte offset true to the file — and the data-hx pin on line 20.
	if c := byID["http::GET::/health"]; c.Line != 5 {
		t.Fatalf("GET /health Line=%d, want 5", c.Line)
	}
	if c := byID["http::GET::/after-comment-block"]; c.Line != 16 {
		t.Fatalf("GET /after-comment-block Line=%d, want 16 (after multi-line comment)", c.Line)
	}
	if c := byID["http::GET::/health2"]; c.Line != 20 {
		t.Fatalf("GET /health2 Line=%d, want 20 (data-hx-get pin)", c.Line)
	}
}

// TestNormalizeHtmxPath pins the extractor-local template pre-pass:
// whole-segment expressions collapse to positional params so consumer
// IDs collide with provider route IDs, while control-flow templates
// that survive normalization are rejected (ok=false) rather than
// minting a junk contract.
func TestNormalizeHtmxPath(t *testing.T) {
	cases := []struct {
		name   string
		in     string
		want   string
		wantOK bool
	}{
		{"go template param", "/ui/parts/{{.P.ID}}/exp", "/ui/parts/{p1}/exp", true},
		{"go template param with spaces", "/orders/{{ order.ID }}", "/orders/{p1}", true},
		{"jinja expression segment", "/shop/{% sku %}/edit", "/shop/{p1}/edit", true},
		{"erb segment", "/shop/<%= sku %>/edit", "/shop/{p1}/edit", true},
		{"two template params", "/a/{{.A}}/b/{{.B}}", "/a/{p1}/b/{p2}", true},
		{"declared then template param", "/w/{wid}/t/{{.TID}}", "/w/{p1}/t/{p2}", true},
		// A partial-segment expression leaves {{...}} in the normalized
		// path, so normalizeHtmxPath rejects it (no trustworthy route ID)
		// rather than minting a contract with template syntax in its ID.
		{"partial-segment template rejected", "/items-{{.ID}}", "", false},
		{"control flow survives normalization", "/api/{{if .V2}}/v2{{else}}/v1{{end}}/items", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := normalizeHtmxPath(tc.in)
			if ok != tc.wantOK {
				t.Fatalf("normalizeHtmxPath(%q) ok = %v, want %v (path %q)", tc.in, ok, tc.wantOK, got)
			}
			if got != tc.want {
				t.Fatalf("normalizeHtmxPath(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}

	// Position parity: the collapsed consumer path must equal what the
	// provider side normalizes for a declared {id} param — same {p1}
	// slot, so the matcher pairs them.
	provider, _ := NormalizeHTTPPathWithParams("/ui/parts/{id}/exp")
	consumer, ok := normalizeHtmxPath("/ui/parts/{{.P.ID}}/exp")
	if !ok || consumer != provider {
		t.Fatalf("consumer %q (ok=%v) != provider %q — matcher will orphan the route", consumer, ok, provider)
	}

	// Whole-value expressions are owned by the skip layer, not this
	// function: skipHtmxValue drops them before normalizeHtmxPath runs.
	if !skipHtmxValue("{{ if .Edit }}/edit{{ else }}/new{{ end }}") {
		t.Fatalf("whole-value expression should be skipped before normalizeHtmxPath")
	}
}

func TestHtmxConsumerIDCollidesWithProvider(t *testing.T) {
	tmpl := `<button hx-get="/ui/parts/{{.P.ID}}/exp">Export</button>`
	consumers := (&HtmxExtractor{}).Extract("ui/internal/templates/parts.html", []byte(tmpl), nil, nil)
	if len(consumers) != 1 {
		t.Fatalf("got %d contracts, want 1: %+v", len(consumers), consumers)
	}

	// Provider side: what route_ast_go / http_filebased build for a
	// declared route — same normalizer, same ID format.
	norm, _ := NormalizeHTTPPathWithParams("/ui/parts/{id}/exp")
	providerID := "http::GET::" + norm
	if consumers[0].ID != providerID {
		t.Fatalf("consumer ID %q != provider ID %q — matcher will orphan the route",
			consumers[0].ID, providerID)
	}

	// Registry round-trip: both sides land in the same workspace bucket.
	reg := NewRegistry()
	reg.AddAllScoped([]Contract{{
		ID: providerID, Type: ContractHTTP, Role: RoleProvider,
		FilePath: "ui/internal/router.go", Line: 42,
	}}, "go-parts", "", "")
	reg.AddAllScoped(consumers, "go-parts", "", "")
	bucket := reg.ByWorkspace("go-parts")
	if len(bucket) != 2 {
		t.Fatalf("workspace bucket has %d contracts, want 2", len(bucket))
	}
}

// keysOf is already declared in http_test.go (same signature, same body);
// reuse it here rather than redeclaring it in this package.
