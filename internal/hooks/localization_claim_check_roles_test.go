package hooks

import "testing"

func TestLocalizationUnstructuredClaimsInspectHeadingBodiesWithoutClaimingRoleLabels(t *testing.T) {
	tests := []struct {
		name    string
		message string
		want    []string
	}{
		{name: "ATX qualified identity", message: "# Writer.write", want: []string{"Writer.write"}},
		{name: "Setext qualified identity", message: "Writer.write\n---", want: []string{"Writer.write"}},
		{name: "quoted ATX identity", message: "> ## Writer.write", want: []string{"Writer.write"}},
		{name: "explicit simple heading", message: "# `Writer`", want: []string{"Writer"}},
		{name: "pointer receiver heading", message: "# `(*Server).handle`", want: []string{"Server.handle"}},
		{name: "bare receiver heading", message: "# (*Server).handle", want: []string{"Server.handle"}},
		{name: "receiver call heading", message: "# (*Server).handle()", want: []string{"Server.handle"}},
		{name: "package receiver heading", message: "# pkg.(*Server).handle", want: []string{"pkg.Server.handle"}},
		{name: "file receiver heading", message: "# server.go::(*Server).handle", want: []string{"server.go::Server.handle"}},
		{name: "PHP namespace heading", message: `# Vendor\Class`, want: []string{`Vendor\Class`}},
		{name: "double-colon ATX is identity", message: "# FILES::danger", want: []string{"FILES::danger"}},
		{name: "double-colon row is identity", message: "EVIDENCE::danger", want: []string{"EVIDENCE::danger"}},
		{name: "plain simple heading", message: "# Architecture"},
		{name: "API prose heading", message: "# API Design"},
		{name: "CommonMark prose heading", message: "# CommonMark behavior"},
		{name: "HTTP Setext prose heading", message: "HTTP Server\n---"},
		{name: "opening role heading", message: "### PRIMARY"},
		{name: "closing role heading", message: "### Implementation_details:"},
		{name: "empty inline roles", message: "PRIMARY:\nSUPPORTING:\nEvidence:\nImplementation_details:"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			claims, _, valid := localizationBoundedSymbolClaims(test.message)
			if !valid {
				t.Fatal("message was rejected")
			}
			assertLocalizationClaims(t, claims, test.want)
		})
	}
}

func TestLocalizationClaimsRespectStructuredRoleState(t *testing.T) {
	tests := []struct {
		name    string
		message string
		want    []string
	}{
		{name: "inline primary", message: "PRIMARY: Writer", want: []string{"Writer"}},
		{name: "primary heading", message: "# PRIMARY\nWriter", want: []string{"Writer"}},
		{name: "supporting label", message: "SUPPORTING:\nWriter", want: []string{"Writer"}},
		{name: "symbol heading", message: "# SYMBOL\nWriter", want: []string{"Writer"}},
		{name: "symbols label", message: "SYMBOLS:\nWriter", want: []string{"Writer"}},
		{name: "Setext primary heading", message: "PRIMARY\n===\nWriter", want: []string{"Writer"}},
		{name: "closing label", message: "PRIMARY:\nWriter\nFILES:\nReader", want: []string{"Writer"}},
		{name: "closing heading", message: "PRIMARY:\nWriter\n# EVIDENCE\nReader", want: []string{"Writer"}},
		{name: "non-role heading closes", message: "PRIMARY:\nWriter\n# Architecture\nReader", want: []string{"Writer"}},
		{name: "blank before content", message: "PRIMARY:\n\nWriter", want: []string{"Writer"}},
		{name: "blank after content closes", message: "PRIMARY:\nWriter\n\nReader", want: []string{"Writer"}},
		{name: "structured row wins over Setext", message: "PRIMARY:\nflush\n---", want: []string{"flush"}},
		{name: "cross-container underline cannot suppress row", message: "- PRIMARY:\n  flush\n---", want: []string{"flush"}},
		{name: "same-container Setext role", message: "- PRIMARY\n  ---\n  Writer", want: []string{"Writer"}},
		{name: "sibling underline is not Setext role", message: "- PRIMARY\n- ---\n  Writer"},
		{name: "nested closing label", message: "- PRIMARY:\n  - FILES:\n    Reader"},
		{name: "nested closing heading", message: "- PRIMARY:\n  - # DETAILS\n    Reader"},
		{name: "nested opening label", message: "- PRIMARY:\n  - SUPPORTING:\n    Writer", want: []string{"Writer"}},
		{name: "absolute-column list tab padding", message: " -\tPRIMARY:\n    flush", want: []string{"flush"}},
		{name: "receiver prose", message: "The implementation calls (*Server).handle before returning.", want: []string{"Server.handle"}},
		{name: "receiver in fence", message: "```text\n(*Server).handle\n```", want: []string{"Server.handle"}},
		{name: "prose", message: "The implementation calls Fabricated.flush before returning.", want: []string{"Fabricated.flush"}},
		{name: "bare leaf", message: "The answer relies on flush."},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			claims, _, valid := localizationBoundedSymbolClaims(test.message)
			if !valid {
				t.Fatal("message was rejected")
			}
			assertLocalizationClaims(t, claims, test.want)
		})
	}
}

func TestLocalizationClaimsRejectExcessiveContainerDepth(t *testing.T) {
	message := "> > > > > > > > > PRIMARY:\nWriter"
	claims, _, valid := localizationBoundedSymbolClaims(message)
	if valid {
		t.Fatalf("claims = %v, want bounded parser rejection", claims)
	}
}

func TestLocalizationGoReceiverClaimPreservesPackageQualifier(t *testing.T) {
	claims, _, valid := localizationBoundedSymbolClaims("# pkg.(*Server).handle")
	if !valid {
		t.Fatal("message was rejected")
	}
	assertLocalizationClaims(t, claims, []string{"pkg.Server.handle"})
	if localizationClaimMatchesEvidence(claims[0], "repo/a.go::other.Server.handle") {
		t.Fatal("package-qualified receiver authenticated a different package")
	}
	if !localizationClaimMatchesEvidence(claims[0], "repo/a.go::pkg.Server.handle") {
		t.Fatal("package-qualified receiver did not authenticate its exact evidence")
	}

	claims, _, valid = localizationBoundedSymbolClaims("# server.go::(*Server).handle")
	if !valid {
		t.Fatal("file-qualified receiver message was rejected")
	}
	assertLocalizationClaims(t, claims, []string{"server.go::Server.handle"})
	if localizationClaimMatchesEvidence(claims[0], "other.go::Server.handle") {
		t.Fatal("file-qualified receiver authenticated a different file")
	}
	if !localizationClaimMatchesEvidence(claims[0], "server.go::Server.handle") {
		t.Fatal("file-qualified receiver did not authenticate its exact evidence")
	}
}
