package contracts

import "testing"

func TestNormalizeHTTPPathWithParams_TemplateSegments(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"go template param", "/ui/parts/{{.P.ID}}/exp", "/ui/parts/{p1}/exp"},
		{"go template param with spaces", "/orders/{{ order.ID }}", "/orders/{p1}"},
		{"bare double-brace id", "/u/{{ID}}/x", "/u/{p1}/x"},
		{"two template params", "/a/{{.A}}/b/{{.B}}", "/a/{p1}/b/{p2}"},
		{"declared then template param", "/w/{wid}/t/{{.TID}}", "/w/{p1}/t/{p2}"},
		{"jinja expression segment", "/shop/{% sku %}/edit", "/shop/{p1}/edit"},
		{"erb segment", "/shop/<%= sku %>/edit", "/shop/{p1}/edit"},
		{"leading base slot stripped", "{{.Base}}/v1/users", "/v1/users"},
		{"leading base slot with slash", "/{{.Base}}/v1/users", "/v1/users"},
		{"partial-segment stays literal", "/items-{{.ID}}", "/items-{{.ID}}"},
		{"query untouched by normalizer", "/s?q={{.Q}}", "/s?q={{.Q}}"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := NormalizeHTTPPathWithParams(tc.in)
			if got != tc.want {
				t.Fatalf("NormalizeHTTPPathWithParams(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
