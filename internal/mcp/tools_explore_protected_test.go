package mcp

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search/rerank"
)

func TestReserveExploreConceptImplementationAcrossLanguages(t *testing.T) {
	const task = "Incorrect results in regex alternation prefilter matching: the optimizer produces false negatives when case-insensitive branches are combined"
	if rerank.ClassifyQuery(task) != rerank.QueryClassConcept {
		t.Fatalf("fixture must be a concept query: %q", task)
	}
	tests := []struct {
		language string
		name     string
		qualName string
	}{
		{language: "rust", name: "extract_alternation", qualName: "crate::regex::Extractor::extract_alternation"},
		{language: "go", name: "extractAlternation", qualName: "regex.Extractor.extractAlternation"},
		{language: "typescript", name: "buildAlternationPrefilter", qualName: "RegexOptimizer.buildAlternationPrefilter"},
	}
	for _, test := range tests {
		t.Run(test.language, func(t *testing.T) {
			field := &graph.Node{ID: test.language + "::regex", Name: "regex", QualName: "RegexMatcher.regex", Kind: graph.KindField, Language: test.language}
			enum := &graph.Node{ID: test.language + "::insensitive", Name: "Insensitive", QualName: "CaseMode.Insensitive", Kind: graph.KindVariable, Language: test.language}
			consumer := &graph.Node{ID: test.language + "::one_regex", Name: "one_regex", QualName: "InnerLiterals.one_regex", Kind: graph.KindMethod, Language: test.language}
			implementation := &graph.Node{ID: test.language + "::implementation", Name: test.name, QualName: test.qualName, Kind: graph.KindMethod, Language: test.language}
			candidates := []*rerank.Candidate{
				{Node: field, VectorRank: 0, TextRank: -1},
				{Node: enum, VectorRank: 1, TextRank: -1},
				{Node: consumer, VectorRank: 2, TextRank: 1},
				{Node: implementation, VectorRank: -1, TextRank: 2},
			}
			got, protectedID := reserveExploreConceptImplementation(task, rerank.QueryClassConcept, candidates, 3)
			if len(got) != len(candidates) {
				t.Fatalf("candidate count = %d, want %d", len(got), len(candidates))
			}
			if got[0].Node.ID != field.ID {
				t.Fatalf("semantic head changed: got %q want %q", got[0].Node.ID, field.ID)
			}
			if protectedID != implementation.ID || got[1].Node.ID != implementation.ID {
				t.Fatalf("protected implementation = %q at %q, want %q at rank 2", protectedID, got[1].Node.ID, implementation.ID)
			}
		})
	}
}

func TestReserveExploreConceptImplementationAddsMarginalComplement(t *testing.T) {
	tests := []struct {
		name       string
		task       string
		primary    string
		complement string
	}{
		{
			name: "replace", task: "print matched output when replace capture groups duplicate lines",
			primary: "print_matched_output", complement: "replace_all",
		},
		{
			name: "alternation", task: "case insensitive search returns false negatives for alternation branches",
			primary: "case_insensitive_search", complement: "extract_alternation",
		},
		{
			name: "ignore hidden", task: "match ignore rules while hidden whitelisted files are skipped",
			primary: "match_ignore_rules", complement: "check_hidden",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			head := &rerank.Candidate{Node: &graph.Node{
				ID: "repo/symptom.go::SymptomState", Name: "SymptomState", QualName: "SymptomState",
				Kind: graph.KindField,
			}}
			primary := &rerank.Candidate{Node: &graph.Node{
				ID: "repo/primary.go::" + test.primary, Name: test.primary,
				QualName: "Primary." + test.primary, Kind: graph.KindMethod,
			}}
			duplicate := &rerank.Candidate{Node: &graph.Node{
				ID: "repo/duplicate.go::" + test.primary, Name: test.primary,
				QualName: "Mirror." + test.primary, Kind: graph.KindMethod,
			}}
			complement := &rerank.Candidate{Node: &graph.Node{
				ID: "repo/complement.go::" + test.complement, Name: test.complement,
				QualName: "Complement." + test.complement, Kind: graph.KindMethod,
			}}
			candidates := []*rerank.Candidate{head, primary, duplicate, complement}

			got, protectedID := reserveExploreConceptImplementation(
				test.task, rerank.QueryClassConcept, candidates, 3,
			)
			if len(got) != len(candidates) {
				t.Fatalf("candidate count = %d, want %d", len(got), len(candidates))
			}
			if got[0] != head || got[1].Node.ID != primary.Node.ID || got[2].Node.ID != complement.Node.ID {
				t.Fatalf("reserved order = [%q %q %q], want head/primary/complement",
					got[0].Node.ID, got[1].Node.ID, got[2].Node.ID)
			}
			if protectedID != primary.Node.ID {
				t.Fatalf("primary protected ID = %q, want %q", protectedID, primary.Node.ID)
			}
			if got[2].Signals[exploreConceptComplementSignal] != 1 {
				t.Fatalf("complement signal = %#v", got[2].Signals)
			}
			if complement.Signals != nil {
				t.Fatal("reservation mutated the input complement candidate")
			}

			bounded, _ := reserveExploreConceptImplementation(
				test.task, rerank.QueryClassConcept, candidates, 2,
			)
			for _, candidate := range bounded {
				if candidate.Signals[exploreConceptComplementSignal] != 0 {
					t.Fatal("max_symbols=2 admitted a second concept reservation")
				}
			}
		})
	}
}

func TestReserveExploreConceptImplementationPrioritizesBehavioralLeadOverDiagnostics(t *testing.T) {
	const task = "Ancestor ignore whitelist matching changes with root path shape. Version 14.1.1 diagnostics at crates/core/flags/hiargs.rs:1108 report generic search input path behavior"
	head := &rerank.Candidate{Node: &graph.Node{
		ID: "repo/state.go::ObservedState", Name: "ObservedState", Kind: graph.KindField,
	}}
	primary := &rerank.Candidate{Node: &graph.Node{
		ID:       "repo/search.go::SearchWorker.search_input_path_diagnostic_version",
		Name:     "search_input_path_diagnostic_version",
		QualName: "SearchWorker.search_input_path_diagnostic_version",
		FilePath: "repo/search.go", Kind: graph.KindMethod,
	}}
	diagnostic := &rerank.Candidate{Node: &graph.Node{
		ID:       "repo/input.go::InputReader.read_search_input_path_log",
		Name:     "read_search_input_path_log",
		QualName: "InputReader.read_search_input_path_log",
		FilePath: "repo/input.go", Kind: graph.KindMethod,
	}}
	behavioral := &rerank.Candidate{Node: &graph.Node{
		ID:       "repo/ignore.go::Ignore.match_ancestor_ignore_path_shape",
		Name:     "match_ancestor_ignore_path_shape",
		QualName: "Ignore.match_ancestor_ignore_path_shape",
		FilePath: "repo/ignore.go", Kind: graph.KindMethod,
	}}
	whitelist := &rerank.Candidate{Node: &graph.Node{
		ID:       "repo/parents.go::Parents.apply_whitelist_root_override",
		Name:     "apply_whitelist_root_override",
		QualName: "Parents.apply_whitelist_root_override",
		FilePath: "repo/parents.go", Kind: graph.KindMethod,
	}}
	candidates := []*rerank.Candidate{head, primary, diagnostic, behavioral, whitelist}

	got, protectedID := reserveExploreConceptImplementation(
		task, rerank.QueryClassConcept, candidates, 4,
	)
	if len(got) != len(candidates) {
		t.Fatalf("candidate width = %d, want unchanged %d", len(got), len(candidates))
	}
	want := []string{head.Node.ID, primary.Node.ID, behavioral.Node.ID, whitelist.Node.ID}
	for index, id := range want {
		if got[index].Node.ID != id {
			t.Fatalf("rank %d = %q, want %q; order=%v", index+1, got[index].Node.ID, id,
				[]string{got[0].Node.ID, got[1].Node.ID, got[2].Node.ID, got[3].Node.ID})
		}
	}
	if protectedID != primary.Node.ID {
		t.Fatalf("primary protected ID = %q, want %q", protectedID, primary.Node.ID)
	}
	for _, index := range []int{2, 3} {
		if got[index].Signals[exploreConceptComplementSignal] != 1 {
			t.Fatalf("rank %d missing complement signal: %#v", index+1, got[index].Signals)
		}
	}
	if diagnostic.Signals != nil || behavioral.Signals != nil || whitelist.Signals != nil {
		t.Fatal("reservation mutated an input candidate")
	}
}

func TestReserveExploreConceptImplementationKeepsReplacementAfterDuplicateFamily(t *testing.T) {
	const task = "Matched output formatting duplicates while replace captures and trim line terminators"
	head := &rerank.Candidate{Node: &graph.Node{
		ID: "repo/state.go::ObservedState", Name: "ObservedState", Kind: graph.KindField,
	}}
	primary := &rerank.Candidate{Node: &graph.Node{
		ID:       "repo/printer.go::Matcher.matched_output_formatting_duplicates",
		Name:     "matched_output_formatting_duplicates",
		QualName: "Matcher.matched_output_formatting_duplicates",
		FilePath: "repo/printer.go", Kind: graph.KindMethod,
	}}
	duplicate := &rerank.Candidate{Node: &graph.Node{
		ID:       "repo/mirror.go::Mirror.matched_output_formatting_duplicates",
		Name:     "matched_output_formatting_duplicates",
		QualName: "Mirror.matched_output_formatting_duplicates",
		FilePath: "repo/mirror.go", Kind: graph.KindMethod,
	}}
	trim := &rerank.Candidate{Node: &graph.Node{
		ID:   "repo/trim.go::Formatter.trim_line_terminators",
		Name: "trim_line_terminators", QualName: "Formatter.trim_line_terminators",
		FilePath: "repo/trim.go", Kind: graph.KindMethod,
	}}
	replace := &rerank.Candidate{Node: &graph.Node{
		ID:   "repo/replace.go::Replacer.replace_all_captures",
		Name: "replace_all_captures", QualName: "Replacer.replace_all_captures",
		FilePath: "repo/replace.go", Kind: graph.KindMethod,
	}}
	candidates := []*rerank.Candidate{head, primary, duplicate, trim, replace}

	got, protectedID := reserveExploreConceptImplementation(
		task, rerank.QueryClassConcept, candidates, 4,
	)
	want := []string{head.Node.ID, primary.Node.ID, trim.Node.ID, replace.Node.ID}
	for index, id := range want {
		if got[index].Node.ID != id {
			t.Fatalf("rank %d = %q, want %q; order=%v", index+1, got[index].Node.ID, id,
				[]string{got[0].Node.ID, got[1].Node.ID, got[2].Node.ID, got[3].Node.ID})
		}
	}
	if protectedID != primary.Node.ID {
		t.Fatalf("primary protected ID = %q, want %q", protectedID, primary.Node.ID)
	}
	if got[2].Signals[exploreConceptComplementSignal] != 1 ||
		got[3].Signals[exploreConceptComplementSignal] != 1 {
		t.Fatalf("two complement signals missing: rank3=%#v rank4=%#v", got[2].Signals, got[3].Signals)
	}
	if duplicate.Signals != nil || trim.Signals != nil || replace.Signals != nil {
		t.Fatal("reservation mutated an input candidate")
	}
}

func TestExploreTwoComplementsPrecedeRelationsWithoutChangingContractWidth(t *testing.T) {
	node := func(id, file string) *graph.Node {
		return &graph.Node{ID: id, Name: id, QualName: id, FilePath: file, Kind: graph.KindMethod}
	}
	head := exploreTarget{node: node("head", "repo/head.go")}
	primary := exploreTarget{node: node("primary", "repo/primary.go"), conceptImplementation: true}
	complementOne := exploreTarget{node: node("complement_one", "repo/one.go"), conceptComplement: true}
	complementTwo := exploreTarget{node: node("complement_two", "repo/two.go"), conceptComplement: true}
	relation := exploreTarget{node: node("relation", "repo/relation.go"), localizationRelation: "direct_callee"}
	targets := []exploreTarget{head, primary, complementOne, relation, complementTwo}

	projected := localizationEvidenceTargetsFromDraft("", "", targets, []exploreDraftEntry{})
	projected = interleaveLocalizationDirectRelations("", "", projected)
	if len(projected) != len(targets) {
		t.Fatalf("contract width = %d, want unchanged %d", len(projected), len(targets))
	}
	want := []exploreTarget{head, primary, complementOne, complementTwo, relation}
	for index, target := range want {
		if projected[index].node.ID != target.node.ID || projected[index].node.FilePath != target.node.FilePath {
			t.Fatalf("contract row %d = %q/%q, want %q/%q", index+1,
				projected[index].node.ID, projected[index].node.FilePath,
				target.node.ID, target.node.FilePath)
		}
	}
}

func TestExploreFinalReservationsProtectComplementAfterTypedProjection(t *testing.T) {
	head := &rerank.Candidate{Node: &graph.Node{ID: "head", Name: "Symptom", Kind: graph.KindField}}
	primary := &rerank.Candidate{Node: &graph.Node{ID: "primary", Name: "match_primary", Kind: graph.KindMethod}}
	complement := &rerank.Candidate{
		Node:    &graph.Node{ID: "complement", Name: "extract_alternation", Kind: graph.KindMethod},
		Signals: map[string]float64{exploreConceptComplementSignal: 1},
	}
	complementTwo := &rerank.Candidate{
		Node:    &graph.Node{ID: "complement_two", Name: "replace_all", Kind: graph.KindMethod},
		Signals: map[string]float64{exploreConceptComplementSignal: 1},
	}
	typed := &rerank.Candidate{
		Node:    &graph.Node{ID: "typed", Name: "typed_consumer", Kind: graph.KindMethod},
		Signals: map[string]float64{exploreTypedAnchorProjectionSignal: 1},
	}
	candidates := []*rerank.Candidate{head, primary, complement, complementTwo, typed}

	preTyped := exploreTypedAnchorReservedCandidateIDs(candidates, nil, primary.Node.ID)
	for _, id := range []string{complement.Node.ID, complementTwo.Node.ID} {
		if _, protected := preTyped[id]; protected {
			t.Fatalf("concept complement %q must not block stronger typed projection admission", id)
		}
	}
	if _, protected := preTyped[typed.Node.ID]; !protected {
		t.Fatal("typed projection must remain protected before final owner folding")
	}

	final := exploreFinalReservedCandidateIDs(candidates, nil, primary.Node.ID)
	for _, id := range []string{head.Node.ID, primary.Node.ID, complement.Node.ID, complementTwo.Node.ID, typed.Node.ID} {
		if _, protected := final[id]; !protected {
			t.Fatalf("final owner-folding reservation is missing %q", id)
		}
	}
}

func TestExploreAnswerDraftReservesMentionedShortSameOwnerCallee(t *testing.T) {
	parent := &graph.Node{
		ID: "literal::extract_alternation", Name: "extract_alternation",
		QualName: "crate::regex::Extractor::extract_alternation", Kind: graph.KindMethod,
		FilePath: "crates/regex/src/literal.rs", Language: "rust",
	}
	union := &graph.Node{
		ID: "literal::union", Name: "union", QualName: "crate::regex::Extractor::union",
		Kind: graph.KindMethod, FilePath: parent.FilePath, Language: "rust",
	}
	finite := &graph.Node{
		ID: "literal::is_finite", Name: "is_finite", QualName: "crate::regex::Extractor::is_finite",
		Kind: graph.KindMethod, FilePath: parent.FilePath, Language: "rust",
	}
	targets := []exploreTarget{{
		node: parent, conceptImplementation: true,
		source:  "fn extract_alternation(&self) { // union both branches; union preserves safety\n self.union(); if self.is_finite() { self.union(); } }",
		callees: []*graph.Node{finite, union},
	}}
	const task = "Incorrect results in regex alternation literal optimization: the prefilter produces false negatives when case-insensitive branches are combined"
	entries := exploreAnswerDraft(task, targets)
	structural := 0
	for _, entry := range entries {
		if !entry.structural {
			continue
		}
		structural++
		if entry.node.ID != union.ID {
			t.Fatalf("reserved structural neighbor = %q, want %q: %#v", entry.node.ID, union.ID, entries)
		}
	}
	if structural != 1 {
		t.Fatalf("structural quota = %d, want exactly one: %#v", structural, entries)
	}
	if len(entries) > exploreDraftTotalLimit {
		t.Fatalf("draft exceeded bounded cardinality: %d", len(entries))
	}

	reads := 0
	materialized := materializeExploreStructuralSourceWithReader(
		context.Background(), task, targets, query.QueryOptions{},
		func(context.Context, *graph.Node) string {
			reads++
			return "fn union(&self) { /* SHORT_CAUSAL_BODY */ }"
		},
	)
	if reads != 1 {
		t.Fatalf("promoted source reads = %d, want exactly one", reads)
	}
	if len(materialized) != len(targets)+1 || materialized[len(materialized)-1].node.ID != union.ID {
		t.Fatalf("materialized targets = %#v, want one appended short causal callee", materialized)
	}
	if !strings.Contains(materialized[len(materialized)-1].source, "SHORT_CAUSAL_BODY") {
		t.Fatalf("short causal source was not materialized: %#v", materialized[len(materialized)-1])
	}
}

func TestExploreAnswerReadyRequiresProtectedBodyBearingImplementation(t *testing.T) {
	const task = "Incorrect results in regex alternation prefilter matching: the optimizer produces false negatives when case-insensitive branches are combined"
	field := &graph.Node{ID: "matcher::regex", Name: "regex", QualName: "RegexMatcher.regex", Kind: graph.KindField}
	if exploreAnswerReady(task, []exploreTarget{{node: field}}) {
		t.Fatal("a symptom field must not terminate ordinary concept localization")
	}

	implementation := &graph.Node{
		ID: "literal::extract_alternation", Name: "extract_alternation",
		QualName: "crate::regex::Extractor::extract_alternation", Kind: graph.KindMethod,
	}
	union := &graph.Node{ID: "literal::union", Name: "union", QualName: "crate::regex::Extractor::union", Kind: graph.KindMethod}
	protected := exploreTarget{
		node: implementation, conceptImplementation: true,
		source: "fn extract_alternation(&self) { self.union(); }", callees: []*graph.Node{union},
	}
	if !exploreAnswerReady(task, []exploreTarget{{node: field}, protected}) {
		t.Fatal("a body-bearing protected implementation with a callable causal neighbor must be answer-ready")
	}
	protected.source = ""
	if exploreAnswerReady(task, []exploreTarget{{node: field}, protected}) {
		t.Fatal("a signature-only protected callable must remain nonterminal")
	}

	uniqueLiteral := exploreTarget{node: field, exactContent: true}
	if !exploreAnswerReady("find the exact literal \"alternation sentinel\"", []exploreTarget{uniqueLiteral}) {
		t.Fatal("unique verified literal evidence must retain its terminal fast path")
	}
}

var benchmarkProtectedExploreID string

func BenchmarkReserveExploreConceptImplementation80(b *testing.B) {
	candidates := make([]*rerank.Candidate, 80)
	for i := range candidates {
		node := &graph.Node{
			ID: fmt.Sprintf("candidate::%02d", i), Name: fmt.Sprintf("config_field_%02d", i),
			QualName: fmt.Sprintf("Config.field%02d", i), Kind: graph.KindField,
		}
		if i%5 == 0 {
			node.Name = fmt.Sprintf("loadConfig%02d", i)
			node.QualName = "ConfigLoader." + node.Name
			node.Kind = graph.KindMethod
		}
		candidates[i] = &rerank.Candidate{Node: node, VectorRank: i, TextRank: -1}
	}
	candidates[79] = &rerank.Candidate{
		Node: &graph.Node{
			ID: "candidate::protected", Name: "buildAlternationPrefilter",
			QualName: "RegexOptimizer.buildAlternationPrefilter", Kind: graph.KindMethod,
		},
		VectorRank: -1, TextRank: 0,
	}
	const task = "Incorrect results in regex alternation prefilter matching: the optimizer produces false negatives when case-insensitive branches are combined"
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		got, protectedID := reserveExploreConceptImplementation(task, rerank.QueryClassConcept, candidates, 10)
		if len(got) != len(candidates) || protectedID == "" {
			b.Fatal("protected selection failed")
		}
		benchmarkProtectedExploreID = protectedID
	}
}
