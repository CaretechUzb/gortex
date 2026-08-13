package hooks

import "testing"

func TestLocalizationUnstructuredClaimsIgnoreHeadingsAndRoleLabels(t *testing.T) {
	message := "# Writer.write\n### Implementation_details:\nPRIMARY:\nSUPPORTING:\nEvidence:\nImplementation_details:"
	claims, _, valid := localizationBoundedSymbolClaims(message)
	if !valid {
		t.Fatal("heading-only message was rejected")
	}
	if len(claims) != 0 {
		t.Fatalf("headings or empty role labels became claims: %v", claims)
	}
}

func TestLocalizationUnstructuredClaimsInspectRoleBodiesAndProse(t *testing.T) {
	tests := []struct {
		name    string
		message string
		want    string
	}{
		{name: "role body", message: "PRIMARY: Fabricated.flush", want: "Fabricated.flush"},
		{name: "prose", message: "The implementation calls Fabricated.flush before returning.", want: "Fabricated.flush"},
		{name: "bare leaf", message: "The answer relies on flush.", want: ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			claims, _, valid := localizationBoundedSymbolClaims(test.message)
			if !valid {
				t.Fatal("message was rejected")
			}
			if test.want == "" {
				if len(claims) != 0 {
					t.Fatalf("bare leaf policy changed: %v", claims)
				}
				return
			}
			if len(claims) != 1 || claims[0] != test.want {
				t.Fatalf("claims = %v, want %q", claims, test.want)
			}
		})
	}
}
