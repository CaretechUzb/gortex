package main

import (
	"bytes"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestScoreCountsAndThresholds(t *testing.T) {
	c := check{Name: "query", Feature: "f", Expected: []string{"a", "b"}}
	r := score(c, []string{"a", "c", "c"})
	if r.Metrics.TP != 1 || r.Metrics.FP != 1 || r.Metrics.FN != 1 || r.Metrics.F1 != .5 {
		t.Fatalf("wrong set metrics: %+v", r)
	}
	if strings.Join(r.Missing, ",") != "b" || strings.Join(r.Unexpected, ",") != "c" {
		t.Fatalf("wrong diagnostic labels: %+v", r)
	}
	negative := score(check{Expected: []string{}}, []string{"invented"})
	if negative.Metrics.Precision != 0 || negative.Metrics.FP != 1 {
		t.Fatalf("negative case failed to penalize invention: %+v", negative)
	}
	empty := score(check{}, nil)
	if empty.Metrics.Precision != 1 || empty.Metrics.Recall != 1 || math.IsNaN(empty.Metrics.F1) {
		t.Fatalf("bad empty-set convention: %+v", empty)
	}
	aggregate := empty.Metrics
	aggregate.add(metrics{FP: 1, FN: 1})
	if aggregate.F1 != 0 {
		t.Fatalf("empty check left a stale perfect F1: %+v", aggregate)
	}
	// Per-feature gates must catch a small broken feature even when a larger
	// feature makes the micro-average look healthy.
	report := report{Features: []featureResult{{feature: feature{ID: "bad"}, Checks: 1, Metrics: r.Metrics}}, FeatureCoverage: .5}
	applyThresholds(&report, .9, .9, .8)
	if report.Passed || len(report.Failures) != 3 {
		t.Fatalf("threshold gate: %+v", report.Failures)
	}
}

func TestBuiltinSuiteAndDeterminism(t *testing.T) {
	a, err := evaluate(builtinSuite())
	if err != nil {
		t.Fatal(err)
	}
	applyThresholds(&a, 1, 1, .8)
	if !a.Passed {
		var output bytes.Buffer
		render(&output, a)
		t.Fatal(output.String())
	}
	if a.LabeledFeatures != 27 || a.StaticFeatures != 33 || len(a.Checks) < 50 {
		t.Fatalf("unexpected coverage: %+v", a)
	}
	b, err := evaluate(builtinSuite())
	if err != nil {
		t.Fatal(err)
	}
	a.ExtractionMS, a.SynthesisMS, b.ExtractionMS, b.SynthesisMS = 0, 0, 0, 0
	applyThresholds(&b, 1, 1, .8)
	if canonical(a) != canonical(b) {
		t.Fatal("report is not reproducible after removing timings")
	}
}

func TestBenchmarkDetectsMissingAndUnexpectedLinks(t *testing.T) {
	for _, tc := range []struct {
		name, path, old, replacement string
		fp, fn                       bool
	}{
		{"missing inheritance", "demo/models.py", "_inherit = 'res.partner'", "_inherit = 'missing.partner'", false, true},
		{"unexpected field", "demo/views.xml", "<form><field name=\"total\"/>", "<form><field name=\"partner_name\"/><field name=\"total\"/>", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := builtinSuite()
			for i := range s.Files {
				if s.Files[i].Path == tc.path {
					s.Files[i].Source = strings.Replace(s.Files[i].Source, tc.old, tc.replacement, 1)
				}
			}
			r, err := evaluate(s)
			if err != nil {
				t.Fatal(err)
			}
			applyThresholds(&r, 1, 1, 0)
			if r.Passed || (tc.fp && r.Metrics.FP == 0) || (tc.fn && r.Metrics.FN == 0) {
				t.Fatalf("benchmark missed deliberate mutation: %+v", r.Metrics)
			}
		})
	}
}

func TestSuiteValidation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*suite)
	}{
		{"no checks", func(s *suite) { s.Checks = nil }},
		{"missing labels", func(s *suite) { s.Checks[0].Expected = nil }},
		{"unknown feature", func(s *suite) { s.Checks[0].Feature = "typo" }},
		{"runtime claim", func(s *suite) { s.Checks[0].Feature = "runtime-state" }},
		{"duplicate check", func(s *suite) { s.Checks = append(s.Checks, s.Checks[0]) }},
		{"duplicate query", func(s *suite) { c := s.Checks[0]; c.Name = "different name"; s.Checks = append(s.Checks, c) }},
		{"unknown file", func(s *suite) { s.Checks[0].File = "typo.py" }},
		{"unknown subject file", func(s *suite) { s.Checks[0].Subject = "typo.py::Item" }},
		{"mismatched subject file", func(s *suite) { s.Checks[0].File = "base/models.py" }},
		{"duplicate label", func(s *suite) { s.Checks[0].Expected = append(s.Checks[0].Expected, s.Checks[0].Expected[0]) }},
		{"duplicate file", func(s *suite) { s.Files = append(s.Files, s.Files[0]) }},
		{"bad kind", func(s *suite) { s.Checks[0].Kind = "typo" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := builtinSuite()
			tc.mutate(&s)
			if validateSuite(s) == nil {
				t.Fatal("invalid suite accepted")
			}
		})
	}
}

func writeFixture(t *testing.T, root, path, source string) {
	t.Helper()
	path = filepath.Join(root, path)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(source), 0644); err != nil {
		t.Fatal(err)
	}
}
func TestCorpusAuditAndLimits(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "demo/__manifest__.py", `{'name': 'Demo'}`)
	writeFixture(t, root, "demo/model.py", "from odoo import fields, models\nclass Demo(models.Model):\n    _name = 'demo.model'\n    value = fields.Char()\n")
	writeFixture(t, root, "demo/view.xml", `<odoo><record id="view" model="ir.ui.view"><field name="model">demo.model</field><field name="arch" type="xml"><form><field name="value"/></form></field></record></odoo>`)
	writeFixture(t, root, ".git/ignored.py", "not parsed")
	writeFixture(t, root, "node_modules/ignored.js", "not parsed")
	if err := os.Symlink(filepath.Join(root, "demo/model.py"), filepath.Join(root, "symlink.py")); err != nil {
		t.Fatal(err)
	}
	r, err := auditCorpus([]string{root}, 100, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if r.Errors != 0 || r.Files != 3 || r.Symlinks != 1 || r.SkippedDirectories != 2 || r.Relations["view_field"] != 1 || r.References["model"].Bound != 1 {
		t.Fatalf("bad corpus report: %+v", r)
	}
	// ir.ui.view's core declaration is intentionally outside this corpus.
	// Missing dependencies are availability gaps, never labeled false negatives.
	if r.References["record_model"].Unbound != 1 {
		t.Fatalf("missing dependency not surfaced: %+v", r.References)
	}
	again, err := auditCorpus([]string{root}, 100, 1<<20)
	if err != nil || again.SourceSHA256 != r.SourceSHA256 {
		t.Fatalf("unstable fingerprint: %v", err)
	}
	limited, err := auditCorpus([]string{root}, 1, 1<<20)
	if err != nil || !limited.Truncated || limited.Files != 1 {
		t.Fatalf("bad file limit: %+v %v", limited, err)
	}
	oversized, err := auditCorpus([]string{root}, 100, 5)
	if err != nil || oversized.OversizedFiles != 3 {
		t.Fatalf("bad byte limit: %+v %v", oversized, err)
	}
	if _, err := auditCorpus([]string{root, filepath.Join(root, "demo")}, 100, 1<<20); err == nil {
		t.Fatal("overlap accepted")
	}
	writeFixture(t, root, "demo/broken.xml", "<odoo><record>")
	broken, err := auditCorpus([]string{root}, 100, 1<<20)
	if err != nil || broken.Errors != 1 {
		t.Fatalf("parse error not surfaced: %+v %v", broken, err)
	}
	gate := report{Corpus: &broken}
	applyThresholds(&gate, 1, 1, 0)
	if gate.Passed {
		t.Fatal("broken corpus passed")
	}
}

func TestCommandJSONAndExitCodes(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"-json", "-"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s %s", code, &stdout, &stderr)
	}
	var r report
	if err := json.Unmarshal(stdout.Bytes(), &r); err != nil || !r.Passed {
		t.Fatalf("invalid machine report: %v", err)
	}
	for _, args := range [][]string{{"-min-recall", "NaN"}, {"-min-precision", "1.1"}, {"-max-files", "0"}, {"unexpected"}} {
		stdout.Reset()
		stderr.Reset()
		if code := run(args, &stdout, &stderr); code != 2 {
			t.Fatalf("%v: expected usage error, got %d", args, code)
		}
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"-min-feature-coverage", "1"}, &stdout, &stderr); code != 1 {
		t.Fatalf("coverage gate exit = %d", code)
	}
	root := t.TempDir()
	// Custom suites replace the built-in inventory; missing sections must not
	// silently retain defaults and inflate coverage.
	writeFixture(t, root, "bad.json", `{"version":"empty"}`)
	if code := run([]string{"-suite", filepath.Join(root, "bad.json")}, &stdout, &stderr); code != 2 {
		t.Fatalf("partial custom suite exit = %d", code)
	}
	s := builtinSuite()
	raw, _ := json.Marshal(s)
	writeFixture(t, root, "suite.json", string(raw))
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"-suite", filepath.Join(root, "suite.json"), "-json", "-"}, &stdout, &stderr); code != 0 {
		t.Fatalf("custom suite exit %d: %s", code, &stderr)
	}
}
