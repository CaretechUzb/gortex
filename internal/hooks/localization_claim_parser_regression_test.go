package hooks

import "testing"

func TestLocalizationClaimParserIgnoresBenignPunctuation(t *testing.T) {
	messages := []string{
		"Release v1.2.3 took 3.14 seconds at 12:30 from 127.0.0.1.",
		"For example, e.g. and i.e. are ordinary prose.",
	}
	for _, message := range messages {
		claims, _, valid := localizationBoundedSymbolClaims(message)
		if !valid {
			t.Fatalf("benign message was rejected: %q", message)
		}
		if len(claims) != 0 {
			t.Fatalf("benign punctuation became symbol claims: message=%q claims=%v", message, claims)
		}
	}
}

func TestLocalizationClaimParserHonorsMarkdownStructure(t *testing.T) {
	tests := []struct {
		name    string
		message string
		want    string
	}{
		{name: "atx heading", message: "# Fabricated.flush", want: "Fabricated.flush"},
		{name: "setext heading", message: "Fabricated.flush\n---", want: "Fabricated.flush"},
		{name: "container atx heading", message: "> # Fabricated.flush", want: "Fabricated.flush"},
		{name: "container setext heading", message: "> Fabricated.flush\n> ===", want: "Fabricated.flush"},
		{name: "backtick fenced heading-like code", message: "```go\n# Fabricated.flush\n```", want: "Fabricated.flush"},
		{name: "tilde fenced heading-like code", message: "~~~text\n## Fabricated.flush\n~~~~", want: "Fabricated.flush"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			claims, _, valid := localizationBoundedSymbolClaims(test.message)
			if !valid {
				t.Fatal("message was rejected")
			}
			if test.want == "" {
				if len(claims) != 0 {
					t.Fatalf("heading became a claim: %v", claims)
				}
				return
			}
			if len(claims) != 1 || claims[0] != test.want {
				t.Fatalf("claims = %v, want %q", claims, test.want)
			}
		})
	}
}

func TestLocalizationStructuredClaimsRequireIdentityRows(t *testing.T) {
	tests := []struct {
		name    string
		message string
		want    string
	}{
		{name: "bare identity row", message: "SYMBOLS:\n- Writer", want: "Writer"},
		{name: "backticked identity row", message: "SYMBOLS:\n- `Writer`", want: "Writer"},
		{name: "bare prose row", message: "SYMBOLS:\n- Writer performs the work", want: ""},
		{name: "qualified claim in prose remainder", message: "SYMBOLS:\n- This row names Fabricated.flush after context", want: "Fabricated.flush"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			claims, _, valid := localizationBoundedSymbolClaims(test.message)
			if !valid {
				t.Fatal("message was rejected")
			}
			if test.want == "" {
				if len(claims) != 0 {
					t.Fatalf("prose row produced a bare claim: %v", claims)
				}
				return
			}
			if len(claims) != 1 || claims[0] != test.want {
				t.Fatalf("claims = %v, want %q", claims, test.want)
			}
		})
	}
}

func TestLocalizationQualifiedSuffixUsesLiteralBoundaries(t *testing.T) {
	for _, test := range []struct {
		claim    string
		evidence string
	}{
		{claim: "Writer::write", evidence: "pkg::Outer::Writer::write"},
		{claim: "Writer#write", evidence: "pkg::Outer#Writer#write"},
		{claim: "Writer#write", evidence: "pkg::Outer::Writer#write"},
	} {
		if !localizationQualifiedSymbolMatches(test.claim, test.evidence) {
			t.Errorf("claim %q did not match qualified evidence %q", test.claim, test.evidence)
		}
	}
	for _, test := range []struct {
		claim    string
		evidence string
	}{
		{claim: "Writer::write", evidence: "pkg::NotWriter::write"},
		{claim: "Writer#write", evidence: "pkg::writer#write"},
		{claim: "Writer#write", evidence: "pkg::NotWriter#write"},
	} {
		if localizationQualifiedSymbolMatches(test.claim, test.evidence) {
			t.Errorf("claim %q crossed a non-boundary in %q", test.claim, test.evidence)
		}
	}
}
