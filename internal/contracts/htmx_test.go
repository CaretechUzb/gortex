package contracts

import (
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
</body></html>
`

func TestHtmxExtractor_SupportedLanguages(t *testing.T) {
	got := (&HtmxExtractor{}).SupportedLanguages()
	want := []string{"html", "gotmpl", "htmldjango"}
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

	byID := make(map[string]Contract)
	for _, c := range out {
		byID[c.ID] = c
	}

	// Export + the duplicate div share one contract ID (dup is a second
	// occurrence of the same route; both recorded, keyed by ID here).
	wantIDs := map[string]bool{
		"http::GET::/ui/parts/{p1}/exp":    false,
		"http::POST::/ui/parts/{p1}/reset": false,
		"http::DELETE::/ui/parts/{p1}":     false,
		"http::GET::/health":               false,
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
	}

	// Query string stripped before normalization.
	if c := byID["http::POST::/ui/parts/{p1}/reset"]; c.Meta["raw_path"] != "/ui/parts/{{.P.ID}}/reset" {
		t.Fatalf("query not stripped: raw_path=%v", c.Meta["raw_path"])
	}

	// Skipped shapes produce nothing.
	for _, id := range byID {
		if id.Meta["raw_path"] == "#section" || id.Meta["raw_path"] == "" ||
			id.Meta["raw_path"] == "javascript:void(0)" ||
			id.Meta["raw_path"] == "{{ if .Edit }}/edit{{ else }}/new{{ end }}" {
			t.Fatalf("skip-shape extracted: %+v", id)
		}
	}

	// Every contract anchors on the file node (no enclosing symbol in HTML).
	for id, c := range byID {
		if c.SymbolID != "ui/internal/templates/parts.html" {
			t.Fatalf("%q: SymbolID=%q, want file-node fallback", id, c.SymbolID)
		}
	}

	// Line numbers point at the attributes. /health is on fixture line 5
	// (the brief said 6; lineNumber is 1-based, see grpc.go).
	if c := byID["http::GET::/health"]; c.Line != 5 {
		t.Fatalf("GET /health Line=%d, want 5", c.Line)
	}
}

// keysOf is already declared in http_test.go (same signature, same body);
// the brief's copy would redeclare it in this package.

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
