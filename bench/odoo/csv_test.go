package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestCorpusReportsCSVRecovery(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "demo/__manifest__.py", `{'name': 'Demo'}`)
	writeFixture(t, root, "demo/ir.model.access.csv", "id,name,model_id:id\nshort,Short\nextra,Extra,model_demo,unmapped\nbare\",Bare,model_demo\nlast,Last,model_demo\n")
	r, err := auditCorpus([]string{root}, 100, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if r.Errors != 0 || r.CSVRecoveryRows != 3 || len(r.CSVRecoverySamples) != 3 || r.Kinds["ir.model.access"] != 4 {
		t.Fatalf("recovery was hidden or lost rows: %+v", r)
	}
	if !strings.Contains(strings.Join(r.CSVRecoverySamples, "\n"), "nonstandard_quotes") {
		t.Fatal("quote diagnostic missing from report")
	}
	gate := report{Corpus: &r}
	applyThresholds(&gate, 1, 1, 0)
	if !gate.Passed {
		t.Fatalf("recoverable data still failed audit: %v", gate.Failures)
	}
	var output bytes.Buffer
	renderCorpus(&output, r)
	if !strings.Contains(output.String(), "CSV rows recovered with diagnostics: 3") {
		t.Fatal("human report hides recovery")
	}
}
