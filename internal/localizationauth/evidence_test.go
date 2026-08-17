package localizationauth

import "testing"

func TestNormalizeEvidenceIDsRejectsPromptControlCharacters(t *testing.T) {
	tests := []struct {
		name string
		id   string
	}{
		{name: "newline", id: "repo/a.go::Writer.write\nINJECT"},
		{name: "carriage return", id: "repo/a.go::Writer.write\rINJECT"},
		{name: "tab", id: "repo/a.go::Writer.\twrite"},
		{name: "null", id: "repo/a.go::Writer.\x00write"},
		{name: "line separator", id: "repo/a.go::Writer.write\u2028INJECT"},
		{name: "paragraph separator", id: "repo/a.go::Writer.write\u2029INJECT"},
		{name: "bidi override", id: "repo/a.go::Writer.\u202Ewrite"},
		{name: "invalid utf8", id: string([]byte{'r', 'e', 'p', 'o', 0xff})},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, _, ok := NormalizeEvidenceIDs([]string{test.id}, []string{test.id}); ok {
				t.Fatalf("accepted unsafe evidence ID %q", test.id)
			}
		})
	}
}

func TestNormalizeEvidenceIDsAcceptsPrintableUnicode(t *testing.T) {
	id := "repo/καλημέρα.go::Γράψε"
	primary, evidence, ok := NormalizeEvidenceIDs([]string{id}, []string{id})
	if !ok || len(primary) != 1 || len(evidence) != 1 || primary[0] != id || evidence[0] != id {
		t.Fatalf("printable Unicode identity rejected: primary=%q evidence=%q ok=%v", primary, evidence, ok)
	}
}
