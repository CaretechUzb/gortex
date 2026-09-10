// Command odoo evaluates labeled Odoo facts and optionally audits source trees.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/resolver"
)

type sourceFile struct {
	Path      string `json:"path"`
	Source    string `json:"source"`
	Repo      string `json:"repo,omitempty"`
	Workspace string `json:"workspace,omitempty"`
}
type feature struct {
	ID          string `json:"id"`
	Description string `json:"description"`
	RuntimeOnly bool   `json:"runtime_only,omitempty"`
}

// A check labels the COMPLETE result set of a narrow query. Unqueried facts
// are not false positives. Node values use canonical JSON; edge values use
// "source -> target"; contract values use "handler -> contract ID".
type check struct {
	Name      string   `json:"name"`
	Feature   string   `json:"feature"`
	Kind      string   `json:"kind"`
	File      string   `json:"file,omitempty"`
	Subject   string   `json:"subject,omitempty"`
	Predicate string   `json:"predicate,omitempty"`
	Expected  []string `json:"expected"`
}
type suite struct {
	Version  string       `json:"version"`
	Features []feature    `json:"features"`
	Files    []sourceFile `json:"files"`
	Checks   []check      `json:"checks"`
}
type metrics struct {
	TP        int     `json:"true_positive"`
	FP        int     `json:"false_positive"`
	FN        int     `json:"false_negative"`
	Precision float64 `json:"precision"`
	Recall    float64 `json:"recall"`
	F1        float64 `json:"f1"`
}

func (m *metrics) finish() {
	// Empty predictions / labels have no errors in that dimension. Counts and
	// positive-label coverage accompany ratios so empty sets cannot hide gaps.
	m.Precision, m.Recall, m.F1 = 1, 1, 0
	if m.TP+m.FP > 0 {
		m.Precision = float64(m.TP) / float64(m.TP+m.FP)
	}
	if m.TP+m.FN > 0 {
		m.Recall = float64(m.TP) / float64(m.TP+m.FN)
	}
	if m.Precision+m.Recall > 0 {
		m.F1 = 2 * m.Precision * m.Recall / (m.Precision + m.Recall)
	}
}
func (m *metrics) add(other metrics) {
	m.TP += other.TP
	m.FP += other.FP
	m.FN += other.FN
	m.finish()
}

type checkResult struct {
	Name       string   `json:"name"`
	Feature    string   `json:"feature"`
	Metrics    metrics  `json:"metrics"`
	Missing    []string `json:"missing,omitempty"`
	Unexpected []string `json:"unexpected,omitempty"`
}
type featureResult struct {
	feature
	Status         string  `json:"status"`
	Checks         int     `json:"checks"`
	PositiveLabels int     `json:"positive_labels"`
	Metrics        metrics `json:"metrics"`
}
type report struct {
	SchemaVersion   int                `json:"schema_version"`
	SuiteVersion    string             `json:"suite_version"`
	SuiteSHA256     string             `json:"suite_sha256"`
	Metrics         metrics            `json:"metrics"`
	Features        []featureResult    `json:"features"`
	Checks          []checkResult      `json:"checks"`
	LabeledFeatures int                `json:"labeled_features"`
	StaticFeatures  int                `json:"static_features"`
	FeatureCoverage float64            `json:"feature_coverage"`
	ExtractionMS    float64            `json:"extraction_ms"`
	SynthesisMS     float64            `json:"synthesis_ms"`
	InvariantErrors []string           `json:"invariant_errors,omitempty"`
	Corpus          *corpusReport      `json:"corpus,omitempty"`
	Failures        []string           `json:"failures,omitempty"`
	Thresholds      map[string]float64 `json:"thresholds"`
	Passed          bool               `json:"passed"`
}
type pipeline struct {
	registry  *parser.Registry
	graph     graph.Store
	contracts []contracts.Contract
}

func newPipeline() *pipeline {
	reg := parser.NewRegistry()
	languages.RegisterAll(reg)
	return &pipeline{registry: reg, graph: graph.New()}
}
func (p *pipeline) extract(f sourceFile) error {
	src := []byte(f.Source)
	lang, ok := p.registry.DetectLanguageContent(f.Path, src)
	if !ok { // Assets still need file identities for manifest glob resolution.
		p.graph.AddNode(&graph.Node{ID: f.Path, Kind: graph.KindFile, FilePath: f.Path, RepoPrefix: f.Repo, WorkspaceID: f.Workspace})
		return nil
	}
	ext, ok := p.registry.GetByLanguage(lang)
	if !ok {
		return fmt.Errorf("%s: no extractor for %s", f.Path, lang)
	}
	result, err := ext.Extract(f.Path, src)
	if err != nil {
		return fmt.Errorf("%s: %w", f.Path, err)
	}
	for _, n := range result.Nodes {
		n.RepoPrefix, n.WorkspaceID = f.Repo, f.Workspace
		// Match the metadata types returned by persisted graphs.
		raw, err := json.Marshal(n.Meta)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(raw, &n.Meta); err != nil {
			return err
		}
	}
	p.graph.AddBatch(result.Nodes, result.Edges)
	p.contracts = append(p.contracts, (&contracts.HTTPExtractor{}).Extract(f.Path, src, result.Nodes, nil)...)
	return nil
}
func (p *pipeline) synthesize() {
	selected, err := resolver.NewFrameworkSynthesizerSelection([]string{resolver.SynthOdoo})
	if err != nil {
		panic(err)
	} // Compile-time built-in selection.
	resolver.RunFrameworkSynthesizersWithSelection(p.graph, selected)
}
func canonical(v any) string { b, _ := json.Marshal(v); return string(b) }
func (p *pipeline) observe(c check) []string {
	var out []string
	if c.Kind == "contract" {
		for _, contract := range p.contracts {
			if c.File != "" && contract.FilePath != c.File {
				continue
			}
			if c.Subject != "" && contract.SymbolID != c.Subject {
				continue
			}
			if contract.Role == contracts.RoleProvider {
				out = append(out, contract.SymbolID+" -> "+contract.ID+" ["+fmt.Sprint(contract.Meta["framework"])+"]")
			}
		}
		return out
	}
	for _, n := range p.graph.AllNodes() {
		if c.File != "" && n.FilePath != c.File {
			continue
		}
		if c.Subject != "" && n.ID != c.Subject {
			continue
		}
		if c.Kind == "node" {
			if v, exists := n.Meta[c.Predicate]; exists {
				out = append(out, n.ID+" = "+canonical(v))
			}
		} else {
			for _, e := range p.graph.GetOutEdges(n.ID) {
				if e.Meta[resolver.MetaSynthesizedBy] == resolver.SynthOdoo && (c.Predicate == "" || e.Meta["odoo_relation"] == c.Predicate) {
					out = append(out, e.From+" -> "+e.To)
				}
			}
		}
	}
	return out
}
func score(c check, actual []string) checkResult {
	r := checkResult{Name: c.Name, Feature: c.Feature}
	want, got := map[string]bool{}, map[string]bool{}
	for _, v := range c.Expected {
		want[v] = true
	}
	for _, v := range actual {
		got[v] = true
	}
	for v := range want {
		if got[v] {
			r.Metrics.TP++
		} else {
			r.Metrics.FN++
			r.Missing = append(r.Missing, v)
		}
	}
	for v := range got {
		if !want[v] {
			r.Metrics.FP++
			r.Unexpected = append(r.Unexpected, v)
		}
	}
	sort.Strings(r.Missing)
	sort.Strings(r.Unexpected)
	r.Metrics.finish()
	return r
}
func validateSuite(s suite) error {
	if s.Version == "" || len(s.Files) == 0 || len(s.Checks) == 0 {
		return fmt.Errorf("suite requires version, files and checks")
	}
	features := map[string]feature{}
	for _, f := range s.Features {
		if f.ID == "" {
			return fmt.Errorf("empty feature ID")
		}
		if _, ok := features[f.ID]; ok {
			return fmt.Errorf("duplicate feature %s", f.ID)
		}
		features[f.ID] = f
	}
	files := map[string]bool{}
	for _, f := range s.Files {
		if f.Path == "" || files[f.Path] {
			return fmt.Errorf("empty or duplicate source path %q", f.Path)
		}
		files[f.Path] = true
	}
	names, queries := map[string]bool{}, map[string]bool{}
	for _, c := range s.Checks {
		if c.Expected == nil {
			return fmt.Errorf("check %q: expected must be an explicit array (use [] for negative cases)", c.Name)
		}
		f, ok := features[c.Feature]
		if !ok || f.RuntimeOnly {
			return fmt.Errorf("check %q has unknown or runtime-only feature", c.Name)
		}
		if c.Name == "" || names[c.Name] {
			return fmt.Errorf("empty or duplicate check name %q", c.Name)
		}
		names[c.Name] = true
		if c.Kind != "node" && c.Kind != "edge" && c.Kind != "contract" {
			return fmt.Errorf("check %q: invalid kind %q", c.Name, c.Kind)
		}
		if c.Kind == "node" && c.Predicate == "" {
			return fmt.Errorf("check %q: node predicate required", c.Name)
		}
		if c.Kind == "contract" && c.Predicate != "" {
			return fmt.Errorf("check %q: contract predicate must be empty", c.Name)
		}
		if c.File != "" && !files[c.File] {
			return fmt.Errorf("check %q: unknown file %s", c.Name, c.File)
		}
		if c.Subject != "" {
			found := false
			for path := range files {
				if c.Subject == path || strings.HasPrefix(c.Subject, path+"::") {
					if c.File == "" || path == c.File {
						found = true
					}
				}
			}
			if !found {
				return fmt.Errorf("check %q: subject has no matching source file", c.Name)
			}
		}
		query := strings.Join([]string{c.Kind, c.File, c.Subject, c.Predicate}, "\x00")
		if queries[query] {
			return fmt.Errorf("duplicate query in check %q", c.Name)
		}
		queries[query] = true
		seen := map[string]bool{}
		for _, v := range c.Expected {
			if seen[v] {
				return fmt.Errorf("check %q: duplicate label", c.Name)
			}
			seen[v] = true
		}
	}
	return nil
}
func invariants(p *pipeline) []string {
	var errors []string
	for _, n := range p.graph.AllNodes() {
		for _, e := range p.graph.GetOutEdges(n.ID) {
			if e.Meta[resolver.MetaSynthesizedBy] != resolver.SynthOdoo {
				continue
			}
			target := p.graph.GetNode(e.To)
			if target == nil || n.WorkspaceID != target.WorkspaceID {
				errors = append(errors, "dangling or cross-workspace edge: "+e.From+" -> "+e.To)
			}
			kind := graph.EdgeReferences
			switch e.Meta["odoo_relation"] {
			case "inherit", "inherit_id", "t-inherit", "t-extend":
				kind = graph.EdgeExtends
			case "delegates":
				kind = graph.EdgeComposes
			case "depends_on":
				kind = graph.EdgeDependsOn
			case "compute", "inverse", "search", "object", "function", "rpc":
				kind = graph.EdgeCalls
			}
			if e.Kind != kind || e.Meta["provenance"] != "framework" {
				errors = append(errors, "invalid edge type/provenance: "+e.From+" -> "+e.To)
			}
		}
	}
	seenContracts := map[string]bool{}
	for _, c := range p.contracts {
		if c.Meta["framework"] != "odoo" {
			continue
		}
		key := c.FilePath + "::" + c.SymbolID + "::" + c.ID
		if seenContracts[key] {
			errors = append(errors, "duplicate HTTP provider: "+key)
		}
		seenContracts[key] = true
		if c.Meta["odoo_options"] == nil {
			errors = append(errors, "HTTP provider lost Odoo options: "+key)
		}
	}
	sort.Strings(errors)
	return errors
}
func evaluate(s suite) (report, error) {
	r := report{SchemaVersion: 1, SuiteVersion: s.Version}
	if err := validateSuite(s); err != nil {
		return r, err
	}
	raw, _ := json.Marshal(s)
	hash := sha256.Sum256(raw)
	r.SuiteSHA256 = hex.EncodeToString(hash[:])
	p := newPipeline()
	start := time.Now()
	for _, f := range s.Files {
		if err := p.extract(f); err != nil {
			return r, err
		}
	}
	r.ExtractionMS = float64(time.Since(start).Microseconds()) / 1000
	start = time.Now()
	p.synthesize()
	r.SynthesisMS = float64(time.Since(start).Microseconds()) / 1000
	r.InvariantErrors = invariants(p)
	byFeature := map[string]*featureResult{}
	for _, f := range s.Features {
		r.Features = append(r.Features, featureResult{feature: f, Status: "untested"})
	}
	for i := range r.Features {
		byFeature[r.Features[i].ID] = &r.Features[i]
	}
	for _, c := range s.Checks {
		result := score(c, p.observe(c))
		r.Checks = append(r.Checks, result)
		r.Metrics.add(result.Metrics)
		f := byFeature[c.Feature]
		f.Checks++
		f.PositiveLabels += len(c.Expected)
		f.Metrics.add(result.Metrics)
	}
	// Repeated synthesis must preserve the full edge set, including source sites.
	before := edgeSnapshot(p)
	p.synthesize()
	if before != edgeSnapshot(p) {
		r.InvariantErrors = append(r.InvariantErrors, "synthesis changed edges on an unchanged graph")
	}
	for i := range r.Features {
		f := &r.Features[i]
		f.Metrics.finish()
		if f.RuntimeOnly {
			f.Status = "runtime_only"
			continue
		}
		r.StaticFeatures++
		if f.Checks > 0 && f.PositiveLabels > 0 {
			r.LabeledFeatures++
			f.Status = "pass"
			if f.Metrics.FP+f.Metrics.FN > 0 {
				f.Status = "fail"
			}
		} else if f.Checks > 0 {
			f.Status = "negative_only"
		}
	}
	if r.StaticFeatures > 0 {
		r.FeatureCoverage = float64(r.LabeledFeatures) / float64(r.StaticFeatures)
	}
	return r, nil
}
func edgeSnapshot(p *pipeline) string {
	var rows []string
	for _, n := range p.graph.AllNodes() {
		for _, e := range p.graph.GetOutEdges(n.ID) {
			if e.Meta[resolver.MetaSynthesizedBy] == resolver.SynthOdoo {
				rows = append(rows, canonical(e))
			}
		}
	}
	sort.Strings(rows)
	return strings.Join(rows, "\n")
}
func applyThresholds(r *report, precision, recall, coverage float64) {
	r.Thresholds = map[string]float64{"min_precision": precision, "min_recall": recall, "min_feature_coverage": coverage}
	r.Failures = nil
	for _, f := range r.Features {
		if f.Checks == 0 {
			continue
		}
		if f.Metrics.Precision < precision {
			r.Failures = append(r.Failures, fmt.Sprintf("%s precision %.4f < %.4f", f.ID, f.Metrics.Precision, precision))
		}
		if f.Metrics.Recall < recall {
			r.Failures = append(r.Failures, fmt.Sprintf("%s recall %.4f < %.4f", f.ID, f.Metrics.Recall, recall))
		}
	}
	if r.FeatureCoverage < coverage {
		r.Failures = append(r.Failures, fmt.Sprintf("feature coverage %.4f < %.4f", r.FeatureCoverage, coverage))
	}
	r.Failures = append(r.Failures, r.InvariantErrors...)
	if r.Corpus != nil && (r.Corpus.Errors > 0 || r.Corpus.Truncated || r.Corpus.OversizedFiles > 0 || len(r.Corpus.InvariantErrors) > 0) {
		r.Failures = append(r.Failures, "corpus audit incomplete or has extraction/invariant errors; see corpus report")
	}
	r.Passed = len(r.Failures) == 0
}
func render(w io.Writer, r report) {
	fmt.Fprintf(w, "Odoo quality benchmark %s (%s)\n", r.SuiteVersion, r.SuiteSHA256[:12])
	fmt.Fprintf(w, "Labeled queries: precision %.2f%%, recall %.2f%%, F1 %.2f%%; TP=%d FP=%d FN=%d\n", 100*r.Metrics.Precision, 100*r.Metrics.Recall, 100*r.Metrics.F1, r.Metrics.TP, r.Metrics.FP, r.Metrics.FN)
	fmt.Fprintf(w, "Feature inventory with positive labels: %d/%d (%.1f%%); this is fixture coverage, not all Odoo behavior.\n", r.LabeledFeatures, r.StaticFeatures, 100*r.FeatureCoverage)
	fmt.Fprintln(w, "Feature                         Status         Checks Labels Precision Recall")
	for _, f := range r.Features {
		if f.Checks == 0 {
			fmt.Fprintf(w, "%-31s %-14s %6d %6d        -      -\n", f.ID, f.Status, 0, 0)
			continue
		}
		fmt.Fprintf(w, "%-31s %-14s %6d %6d %8.1f%% %5.1f%%\n", f.ID, f.Status, f.Checks, f.PositiveLabels, 100*f.Metrics.Precision, 100*f.Metrics.Recall)
	}
	for _, c := range r.Checks {
		for _, v := range c.Missing {
			fmt.Fprintf(w, "MISSING [%s] %s\n", c.Name, v)
		}
		for _, v := range c.Unexpected {
			fmt.Fprintf(w, "UNEXPECTED [%s] %s\n", c.Name, v)
		}
	}
	fmt.Fprintf(w, "Golden timings: extraction %.1f ms, synthesis %.1f ms\n", r.ExtractionMS, r.SynthesisMS)
	if r.Corpus != nil {
		renderCorpus(w, *r.Corpus)
	}
	for _, f := range r.Failures {
		fmt.Fprintln(w, "FAIL:", f)
	}
	fmt.Fprintf(w, "Passed: %t\n", r.Passed)
}

type rootsFlag []string

func (v *rootsFlag) String() string { return strings.Join(*v, ",") }
func (v *rootsFlag) Set(s string) error {
	if s == "" {
		return fmt.Errorf("empty corpus root")
	}
	*v = append(*v, s)
	return nil
}
func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("odoo", flag.ContinueOnError)
	fs.SetOutput(stderr)
	suitePath := fs.String("suite", "", "custom JSON suite (default: built-in, hand-labeled suite)")
	jsonPath := fs.String("json", "", "write JSON report; '-' writes JSON only to stdout")
	minPrecision := fs.Float64("min-precision", 1, "minimum precision per tested feature")
	minRecall := fs.Float64("min-recall", 1, "minimum recall per tested feature")
	minCoverage := fs.Float64("min-feature-coverage", 0, "minimum fraction of static inventory with positive labels")
	maxFiles := fs.Int("max-files", 20000, "maximum corpus candidate files across all roots")
	maxBytes := fs.Int64("max-file-bytes", 2<<20, "maximum bytes per corpus source file")
	var roots rootsFlag
	fs.Var(&roots, "corpus", "read-only source root; repeat for cross-repository resolution in one workspace")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "unexpected positional arguments")
		return 2
	}
	for _, v := range []float64{*minPrecision, *minRecall, *minCoverage} {
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 1 {
			fmt.Fprintln(stderr, "thresholds must be finite numbers in [0,1]")
			return 2
		}
	}
	if *maxFiles <= 0 || *maxBytes <= 0 {
		fmt.Fprintln(stderr, "corpus limits must be positive")
		return 2
	}
	s := builtinSuite()
	if *suitePath != "" {
		s = suite{}
		raw, err := os.ReadFile(*suitePath)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
		dec := json.NewDecoder(strings.NewReader(string(raw)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&s); err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
		var extra any
		if err := dec.Decode(&extra); err != io.EOF {
			fmt.Fprintln(stderr, "suite must contain exactly one JSON document")
			return 2
		}
	}
	r, err := evaluate(s)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if len(roots) > 0 {
		c, err := auditCorpus(roots, *maxFiles, *maxBytes)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
		r.Corpus = &c
	}
	applyThresholds(&r, *minPrecision, *minRecall, *minCoverage)
	if *jsonPath != "" {
		raw, err := json.MarshalIndent(r, "", "  ")
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
		raw = append(raw, '\n')
		if *jsonPath == "-" {
			_, err = stdout.Write(raw)
		} else {
			err = os.WriteFile(*jsonPath, raw, 0644)
		}
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
	}
	if *jsonPath != "-" {
		render(stdout, r)
	}
	if !r.Passed {
		return 1
	}
	return 0
}
func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }
