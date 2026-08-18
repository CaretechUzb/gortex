package contracts

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// HtmxExtractor detects htmx request attributes (hx-get, hx-post, hx-put,
// hx-patch, hx-delete) in HTML template files and emits the HTTP consumer
// side of each request: the template instructs the browser to call that
// route, so a route consumed only from a template is not an orphan
// provider. Canonical IDs collide with provider route contracts through
// normalizeHtmxPath, which maps whole-segment template expressions
// ({{.P.ID}}) onto the same {p1} placeholder space as a provider's
// declared {id} params before delegating to NormalizeHTTPPathWithParams.
type HtmxExtractor struct{}

// htmxAttrRe matches the five request-issuing htmx attributes with either
// quote style. Group 1: verb; group 2: double-quoted value; group 3:
// single-quoted value. Deliberately an attribute-level scan, not an HTML
// parse — Go/Jinja/ERB templates are routinely not well-formed HTML until
// rendered ({{if}} blocks split tags mid-element).
var htmxAttrRe = regexp.MustCompile(`(?i)\bhx-(get|post|put|patch|delete)\s*=\s*(?:"([^"]*)"|'([^']*)')`)

// htmxTemplateSegment matches a path segment that is entirely a template
// expression — {{...}} (Go/Jinja values), {%...%} (Jinja statements),
// <%...%> (ERB). Interpolated path slots collapse to {tplparam} so the
// shared positional normalizer assigns {p1}, {p2}, ... identically to a
// provider's declared :id / {id} params.
var htmxTemplateSegment = regexp.MustCompile(`^\{\{.*\}\}$|^\{%.*%\}$|^<%.*%>$`)

// normalizeHtmxPath applies template-aware segment collapsing BEFORE the
// shared normalizer, so template semantics stay local to this extractor
// and provider-side (and every other consumer's) identity is untouched.
// Returns ok=false for values that cannot produce a trustworthy route ID:
// still containing template syntax after normalization (control flow
// like /api/{{if}}/v2{{else}}/v1{{end}}/items), or normalizing to root
// from an empty-ish value.
func normalizeHtmxPath(raw string) (path string, ok bool) {
	segs := strings.Split(raw, "/")
	changed := false
	for i, seg := range segs {
		if seg == "" || !htmxTemplateSegment.MatchString(seg) {
			continue
		}
		segs[i] = "{tplparam}"
		changed = true
	}
	if changed {
		raw = strings.Join(segs, "/")
	}
	norm, _ := NormalizeHTTPPathWithParams(raw)
	if strings.Contains(norm, "{{") || strings.Contains(norm, "{%") || strings.Contains(norm, "<%") {
		return "", false
	}
	return norm, true
}

// SupportedLanguages covers the registered template languages that carry
// htmx attributes: html (.html/.htm), gotmpl (.tpl/.gotmpl/.tmpl), and
// htmldjango (.djhtml).
func (e *HtmxExtractor) SupportedLanguages() []string {
	return []string{"html", "gotmpl", "htmldjango"}
}

// Extract emits one consumer contract per htmx attribute occurrence,
// deduplicated per (verb, normalized path, line).
func (e *HtmxExtractor) Extract(filePath string, src []byte, fileNodes []*graph.Node, _ []*graph.Edge) []Contract {
	var out []Contract
	text := string(src)
	lines := strings.Split(text, "\n")
	seen := make(map[string]struct{})
	for _, m := range htmxAttrRe.FindAllStringSubmatchIndex(text, -1) {
		verb := strings.ToUpper(text[m[2]:m[3]])
		raw := ""
		if m[4] != -1 {
			raw = text[m[4]:m[5]]
		}
		if m[6] != -1 {
			raw = text[m[6]:m[7]]
		}
		if skipHtmxValue(raw) {
			continue
		}
		// Query strings and fragments never appear in route registrations.
		if i := strings.IndexAny(raw, "?#"); i >= 0 {
			raw = raw[:i]
		}
		// A query-only value ("?sort=mpn") strips to the empty string,
		// which NormalizeHTTPPathWithParams would widen to "/" — a junk
		// root-path consumer that can falsely pair with a real homepage
		// provider. Skip it instead.
		if raw == "" {
			continue
		}
		norm, ok := normalizeHtmxPath(raw)
		if !ok {
			continue
		}
		ln := lineNumber(lines, m[0])
		key := fmt.Sprintf("%s::%s::%d", verb, norm, ln)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, Contract{
			ID:         fmt.Sprintf("http::%s::%s", verb, norm),
			Type:       ContractHTTP,
			Role:       RoleConsumer,
			SymbolID:   htmxAnchorSymbol(fileNodes, ln),
			FilePath:   filePath,
			Line:       ln,
			Meta:       map[string]any{"framework": "htmx", "method": verb, "raw_path": raw},
			Confidence: 0.9,
		})
	}
	return out
}

// skipHtmxValue filters attribute values that are not route references:
// empty values, same-page anchors, javascript: URIs, and values that are
// (or start with) an unrendered control/interpolation expression — a
// dynamically assembled URL has no path we can match.
func skipHtmxValue(v string) bool {
	v = strings.TrimSpace(v)
	if v == "" || strings.HasPrefix(v, "#") || strings.HasPrefix(v, "javascript:") {
		return true
	}
	for _, open := range []string{"{{", "{%", "<%"} {
		if strings.HasPrefix(v, open) {
			return true
		}
	}
	return false
}

// htmxAnchorSymbol picks the graph anchor for a consumer contract: the
// enclosing template element when one encloses the attribute's line,
// otherwise the file node, otherwise "" (contract still enters the
// registry and gets a KindContract node; only the EdgeConsumes edge is
// skipped by the indexer when SymbolID is empty).
func htmxAnchorSymbol(fileNodes []*graph.Node, ln int) string {
	if sid := findEnclosingSymbol(fileNodes, ln); sid != "" {
		return sid
	}
	for _, n := range fileNodes {
		if n.Kind == graph.KindFile {
			return n.ID
		}
	}
	return ""
}
