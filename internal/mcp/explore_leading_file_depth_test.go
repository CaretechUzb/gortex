package mcp

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search/rerank"
)

func leadingDepthNode(id, name, file string) *graph.Node {
	return &graph.Node{
		ID:        id,
		Name:      name,
		QualName:  name,
		Kind:      graph.KindFunction,
		FilePath:  file,
		StartLine: 7,
		EndLine:   21,
		Meta:      map[string]any{"signature": "func " + name + "()"},
	}
}

func leadingDepthHydrate(node *graph.Node) exploreTarget {
	return exploreTarget{node: node, source: "func " + node.Name + "() {}"}
}

func leadingDepthIDs(targets []exploreTarget) []string {
	ids := make([]string, 0, len(targets))
	for _, target := range targets {
		if target.node == nil {
			ids = append(ids, "")
			continue
		}
		ids = append(ids, target.node.ID)
	}
	return ids
}

// leadingDepthPool builds a ranked pool whose leading file owns global ranks
// one and three plus three siblings ranked past the final window, with every
// other rank taken by a distinct file.
func leadingDepthPool() (pool []*rerank.Candidate, leading, breadth []*graph.Node) {
	add := func(node *graph.Node) {
		pool = append(pool, &rerank.Candidate{Node: node, TextRank: len(pool), VectorRank: -1})
	}
	first := leadingDepthNode("repo/lead.go::LeadPrimary", "LeadPrimary", "repo/lead.go")
	second := leadingDepthNode("repo/lead.go::LeadSecond", "LeadSecond", "repo/lead.go")
	leading = append(leading, first, second)
	// One breadth file per remaining window slot, so the assembled window is
	// exactly the default page regardless of how wide the page constant is.
	for index := 0; index < exploreDefaultMaxSymbols-2; index++ {
		breadth = append(breadth, leadingDepthNode(
			fmt.Sprintf("repo/other_%d.go::Breadth%d", index, index),
			fmt.Sprintf("Breadth%d", index),
			fmt.Sprintf("repo/other_%d.go", index),
		))
	}
	for index := 0; index < 3; index++ {
		leading = append(leading, leadingDepthNode(
			fmt.Sprintf("repo/lead.go::LeadSibling%d", index),
			fmt.Sprintf("LeadSibling%d", index),
			"repo/lead.go",
		))
	}
	add(leading[0])
	add(breadth[0])
	add(leading[1])
	for _, node := range breadth[1:] {
		add(node)
	}
	for _, node := range leading[2:] {
		add(node)
	}
	return pool, leading, breadth
}

// leadingDepthWindow is the final window per-file diversification leaves: the
// leading file's first two rows plus one row per other file.
func leadingDepthWindow(leading, breadth []*graph.Node) []exploreTarget {
	targets := []exploreTarget{
		{node: leading[0]},
		{node: breadth[0]},
		{node: leading[1]},
	}
	for _, node := range breadth[1:] {
		targets = append(targets, exploreTarget{node: node})
	}
	return targets
}

func TestLeadingFileDepthAdmitsDemotedSiblingsIntoLocalizationPage(t *testing.T) {
	pool, leading, breadth := leadingDepthPool()
	targets := leadingDepthWindow(leading, breadth)
	if len(targets) != exploreDefaultMaxSymbols {
		t.Fatalf("fixture window = %d rows, want %d", len(targets), exploreDefaultMaxSymbols)
	}

	got := reserveExploreLeadingFileDepthTargets(
		pool, targets, exploreDefaultMaxSymbols, nil, leadingDepthHydrate,
	)
	if len(got) != exploreDefaultMaxSymbols {
		t.Fatalf("depth allocation changed page width to %d, want %d", len(got), exploreDefaultMaxSymbols)
	}
	admitted := map[string]bool{}
	for _, target := range got {
		if target.node != nil {
			admitted[target.node.ID] = true
		}
	}
	// The first two siblings, ranked immediately past the window and demoted
	// below every other file.
	for _, node := range leading[2:4] {
		if !admitted[node.ID] {
			t.Fatalf("leading-file sibling %q missing from page: %v", node.ID, leadingDepthIDs(got))
		}
	}
	if admitted[breadth[len(breadth)-2].ID] || admitted[breadth[len(breadth)-1].ID] {
		t.Fatalf("depth rows did not come from the breadth tail: %v", leadingDepthIDs(got))
	}
	for index := 0; index < localizationDirectEvidenceReserve; index++ {
		if got[index].node == nil || got[index].node.ID != targets[index].node.ID {
			t.Fatalf("head row %d = %q, want %q", index, leadingDepthIDs(got)[index], targets[index].node.ID)
		}
	}
	// Every admitted sibling arrived via the depth allocator and must be
	// marked and hydrated. (The displaceable tail can be wider than the
	// sibling pool, so retained breadth rows may legitimately sit beside
	// them.)
	siblings := map[string]bool{}
	for _, node := range leading[2:] {
		siblings[node.ID] = true
	}
	for _, target := range got {
		if target.node == nil || !siblings[target.node.ID] {
			continue
		}
		if !target.leadingFileDepth || target.source == "" {
			t.Fatalf("depth row %q is unmarked or unhydrated", target.node.ID)
		}
	}
	again := reserveExploreLeadingFileDepthTargets(
		pool, targets, exploreDefaultMaxSymbols, nil, leadingDepthHydrate,
	)
	if fmt.Sprint(leadingDepthIDs(got)) != fmt.Sprint(leadingDepthIDs(again)) {
		t.Fatal("depth allocation is not deterministic")
	}
}

func TestLeadingFileDepthNeverEvictsProtectedEvidence(t *testing.T) {
	pool, leading, breadth := leadingDepthPool()
	targets := leadingDepthWindow(leading, breadth)
	// Protect every displaceable row — the tail past the direct-evidence
	// reserve — so a correct allocator has nothing it may move.
	for index := localizationDirectEvidenceReserve; index < len(targets); index++ {
		if (index-localizationDirectEvidenceReserve)%2 == 0 {
			targets[index].causalChangeOwner = true
		} else {
			targets[index].sourceLiteral = true
		}
	}

	got := reserveExploreLeadingFileDepthTargets(
		pool, targets, exploreDefaultMaxSymbols, nil, leadingDepthHydrate,
	)
	if fmt.Sprint(leadingDepthIDs(got)) != fmt.Sprint(leadingDepthIDs(targets)) {
		t.Fatalf("protected tail was displaced: %v", leadingDepthIDs(got))
	}

	targets[len(targets)-1].sourceLiteral = false
	reserved := map[string]struct{}{targets[len(targets)-1].node.ID: {}}
	got = reserveExploreLeadingFileDepthTargets(
		pool, targets, exploreDefaultMaxSymbols, reserved, leadingDepthHydrate,
	)
	for _, target := range got {
		if target.node != nil && target.node.ID == targets[len(targets)-1].node.ID {
			return
		}
	}
	t.Fatalf("externally reserved row was evicted: %v", leadingDepthIDs(got))
}

func TestLeadingFileDepthStaysInsideTheEvidencePageWindow(t *testing.T) {
	pool, leading, breadth := leadingDepthPool()
	targets := leadingDepthWindow(leading, breadth)
	for index := 0; index < 4; index++ {
		targets = append(targets, exploreTarget{node: leadingDepthNode(
			fmt.Sprintf("repo/wide_%d.go::Wide%d", index, index),
			fmt.Sprintf("Wide%d", index),
			fmt.Sprintf("repo/wide_%d.go", index),
		)})
	}

	got := reserveExploreLeadingFileDepthTargets(
		pool, targets, exploreMaxMaxSymbols, nil, leadingDepthHydrate,
	)
	if len(got) != len(targets) {
		t.Fatalf("wide request width = %d, want %d", len(got), len(targets))
	}
	for index := localizationReplayEvidenceLimit; index < len(got); index++ {
		if got[index].leadingFileDepth {
			t.Fatalf("depth row landed at %d, past the serialized page", index)
		}
	}
	depthRows := 0
	for _, target := range got[:localizationReplayEvidenceLimit] {
		if target.leadingFileDepth {
			depthRows++
		}
	}
	if depthRows == 0 {
		t.Fatalf("no depth row reached the page: %v", leadingDepthIDs(got))
	}
}

func TestLeadingFileDepthLeavesScatteredRankingUnchanged(t *testing.T) {
	var pool []*rerank.Candidate
	var targets []exploreTarget
	for index := 0; index < 15; index++ {
		node := leadingDepthNode(
			fmt.Sprintf("repo/scatter_%d.go::Scatter%d", index, index),
			fmt.Sprintf("Scatter%d", index),
			fmt.Sprintf("repo/scatter_%d.go", index),
		)
		pool = append(pool, &rerank.Candidate{Node: node, TextRank: index, VectorRank: -1})
		if index < exploreDefaultMaxSymbols {
			targets = append(targets, exploreTarget{node: node})
		}
	}
	got := reserveExploreLeadingFileDepthTargets(
		pool, targets, exploreDefaultMaxSymbols, nil, leadingDepthHydrate,
	)
	if fmt.Sprint(leadingDepthIDs(got)) != fmt.Sprint(leadingDepthIDs(targets)) {
		t.Fatalf("scattered ranking changed the page: %v", leadingDepthIDs(got))
	}
}

func TestLeadingFileDepthRowsSurviveEnvelopePacking(t *testing.T) {
	pool, leading, breadth := leadingDepthPool()
	page := reserveExploreLeadingFileDepthTargets(
		pool, leadingDepthWindow(leading, breadth), exploreDefaultMaxSymbols, nil, leadingDepthHydrate,
	)

	task := "LeadPrimary LeadSecond LeadSibling0 behavior"
	completion := newLocalizationCompletion(true, "")
	result, _, digest, packed := buildLocalizationExploreResultForTaskFinalized(
		completion, task, page, exploreMaxBudgetTokens,
	)
	if result == nil || result.IsError || digest == nil || packed.State == localizationStateInactive {
		t.Fatalf("packed depth result = (%#v, %#v, %#v)", result, digest, packed)
	}
	text, ok := singleTextContent(result)
	if !ok {
		t.Fatalf("packed result content = %#v", result.Content)
	}
	var envelope localizationExploreEnvelope
	if err := json.Unmarshal([]byte(text), &envelope); err != nil {
		t.Fatalf("decode packed depth envelope: %v", err)
	}
	if len(envelope.Files) != len(envelope.Evidence) || len(envelope.Symbols) != len(envelope.Evidence) {
		t.Fatalf("unaligned packed rows: files=%d symbols=%d evidence=%d",
			len(envelope.Files), len(envelope.Symbols), len(envelope.Evidence))
	}
	leadingRows := 0
	for index, row := range envelope.Evidence {
		if row.Rank != index+1 || envelope.Files[index] != row.File || envelope.Symbols[index] != row.ID {
			t.Fatalf("unaligned row %d: %#v", index+1, row)
		}
		if row.File == "repo/lead.go" {
			leadingRows++
		}
	}
	if leadingRows < 3 {
		t.Fatalf("packed envelope kept %d leading-file rows, want depth beyond diversification", leadingRows)
	}
}
