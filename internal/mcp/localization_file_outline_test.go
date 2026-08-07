package mcp

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search/rerank"
)

const outlineLeadingFile = "repo/lead.go"

func outlineDeclaration(name string, line int) *graph.Node {
	return &graph.Node{
		ID:        outlineLeadingFile + "::" + name,
		Name:      name,
		QualName:  name,
		Kind:      graph.KindFunction,
		FilePath:  outlineLeadingFile,
		StartLine: line,
		EndLine:   line + 4,
		Meta:      map[string]any{"signature": "func " + name + "()"},
	}
}

func outlineDeclaredFile(count int) []*graph.Node {
	nodes := make([]*graph.Node, 0, count)
	for index := 0; index < count; index++ {
		nodes = append(nodes, outlineDeclaration(fmt.Sprintf("Declared%02d", index), index+1))
	}
	return nodes
}

func outlinePool(nodes ...*graph.Node) []*rerank.Candidate {
	pool := make([]*rerank.Candidate, 0, len(nodes))
	for index, node := range nodes {
		pool = append(pool, &rerank.Candidate{Node: node, TextRank: index, VectorRank: -1})
	}
	return pool
}

func outlineEnvelope(t *testing.T, result *mcpgo.CallToolResult) localizationExploreEnvelope {
	t.Helper()
	if result == nil || result.IsError {
		t.Fatalf("localization result = %#v", result)
	}
	text, ok := singleTextContent(result)
	if !ok {
		t.Fatalf("localization result content = %#v", result.Content)
	}
	var envelope localizationExploreEnvelope
	if err := json.Unmarshal([]byte(text), &envelope); err != nil {
		t.Fatalf("decode localization envelope: %v", err)
	}
	return envelope
}

func outlineEnvelopeBytes(t *testing.T, result *mcpgo.CallToolResult) int {
	t.Helper()
	text, ok := singleTextContent(result)
	if !ok {
		t.Fatalf("localization result content = %#v", result.Content)
	}
	return len(text)
}

func TestRefiningPageCarriesTheLeadingFileOutline(t *testing.T) {
	declared := outlineDeclaredFile(6)
	preferred, alternate := declared[0], declared[1]
	targets := []exploreTarget{
		{node: preferred, source: "func Declared00() { executeAll() }"},
		{node: alternate, source: "func Declared01() { executeOne() }"},
	}
	enumerated := 0
	outline := localizationLeadingFileOutlineProvider(
		outlinePool(preferred, alternate), targets, nil,
		func(file string) []*graph.Node {
			if file != outlineLeadingFile {
				t.Fatalf("enumerated file = %q, want %q", file, outlineLeadingFile)
			}
			enumerated++
			return declared
		},
	)
	result, completion, _, _ := buildLocalizationRefinementResultForTaskWithOutline(
		preferred.ID, "find the declared implementation", targets,
		localizationDefaultBudgetTokens, exploreLocalizationRefinementRoutes(targets), outline,
	)
	if completion.State != localizationStateNeedsRefinement {
		t.Fatalf("completion state = %q, want %q", completion.State, localizationStateNeedsRefinement)
	}
	envelope := outlineEnvelope(t, result)
	if envelope.Outline == nil {
		t.Fatal("refining page carries no leading-file outline")
	}
	if enumerated != 1 {
		t.Fatalf("graph enumerations = %d, want exactly one per page", enumerated)
	}
	if envelope.Outline.File != outlineLeadingFile || envelope.Outline.Declared != len(declared) {
		t.Fatalf("outline = %#v, want %d declarations in %q",
			envelope.Outline, len(declared), outlineLeadingFile)
	}
	ranked := make(map[string]bool, len(envelope.Evidence))
	for _, row := range envelope.Evidence {
		ranked[row.Name] = true
	}
	unranked := 0
	for _, row := range envelope.Outline.Rows {
		if row.Name == "" || row.Line <= 0 {
			t.Fatalf("outline row is not a locator: %#v", row)
		}
		if !ranked[row.Name] {
			unranked++
		}
	}
	if unranked == 0 {
		t.Fatal("outline names only the symbols the ranked page already carries")
	}
	encoded, err := json.Marshal(envelope.Outline)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "func ") {
		t.Fatalf("outline carries signatures or bodies: %s", encoded)
	}
}

func TestTerminalPageCarriesNoOutlineAndPaysNoEnumeration(t *testing.T) {
	declared := outlineDeclaredFile(6)
	targets := []exploreTarget{{
		node: declared[0], source: "func Declared00() { executeAll() }",
		sourceLiteral: true, sourceLiteralCallee: true, exactContent: true,
	}}
	enumerated := 0
	outline := localizationLeadingFileOutlineProvider(
		outlinePool(declared[0]), targets, nil,
		func(string) []*graph.Node {
			enumerated++
			return declared
		},
	)
	result, _, _, packed := buildLocalizationExploreResultForTaskFinalizedWithOutline(
		newLocalizationCompletion(true, ""), "Declared00 executeAll", targets,
		localizationDefaultBudgetTokens, outline,
	)
	if packed.State != localizationStateAnswerReady {
		t.Fatalf("packed state = %q, want %q", packed.State, localizationStateAnswerReady)
	}
	if envelope := outlineEnvelope(t, result); envelope.Outline != nil {
		t.Fatalf("terminal page carries an outline: %#v", envelope.Outline)
	}
	if enumerated != 0 {
		t.Fatalf("terminal page paid %d graph enumerations, want none", enumerated)
	}
}

func TestOutlineGivesWayBeforeEvidenceRowsUnderATightBudget(t *testing.T) {
	declared := outlineDeclaredFile(40)
	long := strings.Repeat("quoted-\"-slash-\\-metadata-", 24)
	neighbors := make([]*graph.Node, 0, 12)
	for index := 0; index < 12; index++ {
		neighbors = append(neighbors, &graph.Node{ID: fmt.Sprintf("repo/neighbor-%02d-%s", index, long)})
	}
	targets := []exploreTarget{
		{node: declared[0], source: "func Declared00() { executeAll() }", callers: neighbors, callees: neighbors},
		{node: declared[1], source: "func Declared01() { executeOne() }", callers: neighbors, callees: neighbors},
	}
	for index := 0; index < 8; index++ {
		targets = append(targets, exploreTarget{node: &graph.Node{
			ID:       fmt.Sprintf("repo/optional-%02d.go::%s", index, long),
			Name:     long,
			Kind:     graph.KindFunction,
			FilePath: fmt.Sprintf("repo/optional/%02d/%s.go", index, long),
			QualName: long,
			Meta:     map[string]any{"signature": long},
		}})
	}
	routes := exploreLocalizationRefinementRoutes(targets)
	build := func(budget int, withOutline bool) (localizationExploreEnvelope, int) {
		var outline func() *localizationFileOutline
		if withOutline {
			outline = localizationLeadingFileOutlineProvider(
				outlinePool(declared[0], declared[1]), targets, nil,
				func(string) []*graph.Node { return declared },
			)
		}
		result, completion, _, _ := buildLocalizationRefinementResultForTaskWithOutline(
			declared[0].ID, "find the declared implementation", targets, budget, routes, outline,
		)
		if completion.State != localizationStateNeedsRefinement {
			t.Fatalf("completion state = %q, want %q", completion.State, localizationStateNeedsRefinement)
		}
		return outlineEnvelope(t, result), outlineEnvelopeBytes(t, result)
	}

	tight, tightBytes := build(exploreMinBudgetTokens, true)
	// The outline is what a tight page gives back, and it gives it back by
	// degrees: fewer rows down to the floor, then nothing.
	if tight.Outline != nil {
		if rows := len(tight.Outline.Rows); rows >= len(declared) || rows < localizationOutlineFloorRows {
			t.Fatalf("tight page kept %d outline rows of %d declared, floor %d",
				rows, len(declared), localizationOutlineFloorRows)
		}
	}
	if tightBytes > exploreMinBudgetTokens*localizationEnvelopeBytesPerToken {
		t.Fatalf("tight envelope = %d bytes, budget = %d",
			tightBytes, exploreMinBudgetTokens*localizationEnvelopeBytesPerToken)
	}
	bare, bareBytes := build(exploreMinBudgetTokens, false)
	if !reflect.DeepEqual(tight.Evidence, bare.Evidence) || !reflect.DeepEqual(tight.Symbols, bare.Symbols) {
		t.Fatalf("outline cost evidence rows: %d/%d rows, %d/%d bytes",
			len(tight.Evidence), len(bare.Evidence), tightBytes, bareBytes)
	}
	// The same rows and the same outline, with room for both: only the budget
	// decided which one gave way.
	wide, _ := build(exploreMaxBudgetTokens, true)
	if wide.Outline == nil || len(wide.Outline.Rows) != len(declared) {
		t.Fatalf("a page with room to spare shortened its outline: %#v", wide.Outline)
	}
	if tight.Outline != nil && len(tight.Outline.Rows) >= len(wide.Outline.Rows) {
		t.Fatalf("the tight page gave nothing back: %d rows against %d",
			len(tight.Outline.Rows), len(wide.Outline.Rows))
	}
}

func TestOutlineElidesTheMiddleOfALargeFile(t *testing.T) {
	declared := outlineDeclaredFile(100)
	outline := newLocalizationFileOutline(outlineLeadingFile, declared)
	if outline == nil {
		t.Fatal("large file produced no outline")
	}
	if len(outline.Rows) != localizationOutlineRowCap {
		t.Fatalf("outline rows = %d, want cap %d", len(outline.Rows), localizationOutlineRowCap)
	}
	if outline.Declared != len(declared) || outline.Elided != len(declared)-localizationOutlineRowCap {
		t.Fatalf("outline = declared %d elided %d, want %d/%d",
			outline.Declared, outline.Elided, len(declared), len(declared)-localizationOutlineRowCap)
	}
	head := outline.Rows[:localizationOutlineHeadRows]
	tail := outline.Rows[localizationOutlineHeadRows:]
	if head[0].Line != 1 || head[len(head)-1].Line != localizationOutlineHeadRows {
		t.Fatalf("outline head = lines %d..%d", head[0].Line, head[len(head)-1].Line)
	}
	if tail[len(tail)-1].Line != len(declared) {
		t.Fatalf("outline tail ends at line %d, want %d", tail[len(tail)-1].Line, len(declared))
	}
	if tail[0].Line <= head[len(head)-1].Line+1 {
		t.Fatalf("outline elided nothing between lines %d and %d", head[len(head)-1].Line, tail[0].Line)
	}
	for _, row := range outline.Rows {
		if row.Kind != "f" {
			t.Fatalf("outline row kind = %q, want the function letter", row.Kind)
		}
	}
}

func TestLocalizationBudgetLeavesRoomForOutlineBesideEvidence(t *testing.T) {
	if exploreDefaultBudgetTokens != 1600 {
		t.Fatalf("explore default budget = %d, want 1600 for non-localization paths", exploreDefaultBudgetTokens)
	}
	if localizationDefaultBudgetTokens != 2400 {
		t.Fatalf("localization default budget = %d, want 2400", localizationDefaultBudgetTokens)
	}
	declared := outlineDeclaredFile(60)
	targets := []exploreTarget{
		{node: declared[0], source: "func Declared00() { executeAll() }"},
		{node: declared[1], source: "func Declared01() { executeOne() }"},
	}
	for index := 0; index < 8; index++ {
		name := fmt.Sprintf("Breadth%02d", index)
		targets = append(targets, exploreTarget{node: &graph.Node{
			ID:        fmt.Sprintf("repo/breadth_%02d.go::%s", index, name),
			Name:      name,
			QualName:  "breadth." + name,
			Kind:      graph.KindFunction,
			FilePath:  fmt.Sprintf("repo/breadth_%02d.go", index),
			StartLine: 12,
			EndLine:   40,
			Meta:      map[string]any{"signature": "func " + name + "(ctx context.Context) error"},
		}})
	}
	outline := localizationLeadingFileOutlineProvider(
		outlinePool(declared[0], declared[1]), targets, nil,
		func(string) []*graph.Node { return declared },
	)
	result, completion, _, _ := buildLocalizationRefinementResultForTaskWithOutline(
		declared[0].ID, "find the declared implementation", targets,
		localizationDefaultBudgetTokens, exploreLocalizationRefinementRoutes(targets), outline,
	)
	if completion.State != localizationStateNeedsRefinement {
		t.Fatalf("completion state = %q", completion.State)
	}
	envelope := outlineEnvelope(t, result)
	if bytes := outlineEnvelopeBytes(t, result); bytes > localizationDefaultBudgetTokens*localizationEnvelopeBytesPerToken {
		t.Fatalf("envelope = %d bytes, budget = %d",
			bytes, localizationDefaultBudgetTokens*localizationEnvelopeBytesPerToken)
	}
	if envelope.Outline == nil || len(envelope.Outline.Rows) != localizationOutlineRowCap {
		t.Fatalf("outline = %#v, want %d rows beside the evidence", envelope.Outline, localizationOutlineRowCap)
	}
	if len(envelope.Evidence) != len(targets) {
		t.Fatalf("evidence rows = %d, want all %d ranked rows beside the outline",
			len(envelope.Evidence), len(targets))
	}
}

func TestLeadingFileOutlineEnumeratesOncePerPage(t *testing.T) {
	declared := outlineDeclaredFile(4)
	targets := []exploreTarget{{node: declared[0]}, {node: declared[1]}}
	enumerated := 0
	outline := localizationLeadingFileOutlineProvider(
		outlinePool(declared[0], declared[1]), targets, nil,
		func(string) []*graph.Node {
			enumerated++
			return declared
		},
	)
	first, second := outline(), outline()
	if first == nil || first != second {
		t.Fatalf("outline provider = (%#v, %#v), want one retained result", first, second)
	}
	if enumerated != 1 {
		t.Fatalf("graph enumerations = %d, want exactly one", enumerated)
	}
}

func TestLeadingFileOutlineFallsBackToAlreadyFetchedNodes(t *testing.T) {
	declared := outlineDeclaredFile(3)
	targets := []exploreTarget{{node: declared[0]}, {node: declared[1]}}
	outline := localizationLeadingFileOutlineProvider(
		outlinePool(declared...), targets, nil,
		func(string) []*graph.Node { return nil },
	)()
	if outline == nil || outline.Declared != len(declared) {
		t.Fatalf("outline = %#v, want the %d pool nodes of %q", outline, len(declared), outlineLeadingFile)
	}
}

func outlineRowNamed(outline *localizationFileOutline, name string) bool {
	if outline == nil {
		return false
	}
	for _, row := range outline.Rows {
		if row.Name == name {
			return true
		}
	}
	return false
}

func TestOutlineElisionKeepsTaskTermMatchingDeclarations(t *testing.T) {
	const named = "dispatchHttpRequest"
	declared := outlineDeclaredFile(60)
	// The declaration the task names sits in the middle of the file — exactly
	// where a blind head/tail elision drops it.
	middle := len(declared) / 2
	declared[middle] = outlineDeclaration(named, middle+1)

	blind := newLocalizationFileOutline(outlineLeadingFile, declared)
	if blind == nil || len(blind.Rows) != localizationOutlineRowCap {
		t.Fatalf("blind outline = %#v, want %d rows", blind, localizationOutlineRowCap)
	}
	if outlineRowNamed(blind, named) {
		t.Fatalf("fixture does not reproduce a mid-file elision: %q survived a blind cap", named)
	}

	terms := exploreTerminalTerms("the http request dispatch never retries")
	outline := newLocalizationFileOutlineForTerms(outlineLeadingFile, declared, terms, localizationOutlineRowCap)
	if outline == nil {
		t.Fatal("task-term outline is absent")
	}
	if len(outline.Rows) != localizationOutlineRowCap {
		t.Fatalf("outline rows = %d, want cap %d", len(outline.Rows), localizationOutlineRowCap)
	}
	if outline.Declared != len(declared) || outline.Elided != len(declared)-localizationOutlineRowCap {
		t.Fatalf("outline = declared %d elided %d, want %d/%d",
			outline.Declared, outline.Elided, len(declared), len(declared)-localizationOutlineRowCap)
	}
	if !outlineRowNamed(outline, named) {
		t.Fatalf("outline elided the declaration the task names: %#v", outline.Rows)
	}
	// The retained rows stay a file index: still in line order, still anchored
	// at the file's own head and tail.
	for index := 1; index < len(outline.Rows); index++ {
		if outline.Rows[index-1].Line > outline.Rows[index].Line {
			t.Fatalf("outline rows are out of file order at %d: %#v", index, outline.Rows)
		}
	}
	if outline.Rows[0].Line != 1 || outline.Rows[len(outline.Rows)-1].Line != len(declared) {
		t.Fatalf("outline spans lines %d..%d, want the whole file's ends",
			outline.Rows[0].Line, outline.Rows[len(outline.Rows)-1].Line)
	}
}

// outlineBreadthTargets is the ranked breadth a real localization page carries
// beside its leading file: distinct files, each with the qualified name,
// signature, and neighbor identifiers an evidence row serializes.
func outlineBreadthTargets(count int) []exploreTarget {
	targets := make([]exploreTarget, 0, count)
	for index := 0; index < count; index++ {
		name := fmt.Sprintf("BreadthCandidate%02d", index)
		qual := fmt.Sprintf("repo/breadth/service/%02d/handler.BreadthCandidate%02d.Execute", index, index)
		neighbors := make([]*graph.Node, 0, 3)
		for neighbor := 0; neighbor < 3; neighbor++ {
			neighbors = append(neighbors, &graph.Node{
				ID: fmt.Sprintf("repo/breadth/service/%02d/neighbor_%d.go::Neighbor%02d%d", index, neighbor, index, neighbor),
			})
		}
		targets = append(targets, exploreTarget{
			node: &graph.Node{
				ID:        fmt.Sprintf("repo/breadth/service/%02d/handler.go::%s", index, name),
				Name:      name,
				QualName:  qual,
				Kind:      graph.KindFunction,
				FilePath:  fmt.Sprintf("repo/breadth/service/%02d/handler.go", index),
				StartLine: 24,
				EndLine:   96,
				Meta: map[string]any{
					"signature": "func (" + name + ") Execute(ctx context.Context, request *BreadthRequest, options ...BreadthOption) (*BreadthResponse, error)",
					"qualname":  qual,
				},
			},
			callers: neighbors,
			callees: neighbors,
		})
	}
	return targets
}

func TestOutlineShrinksRatherThanVanishingWhenTheBudgetBinds(t *testing.T) {
	const named = "computeRetryBackoff"
	declared := outlineDeclaredFile(40)
	// The answer is a declaration of the file the page already leads with, in
	// the middle of it — the shape the outline exists to catch.
	middle := len(declared) / 2
	declared[middle] = outlineDeclaration(named, middle+1)
	task := "the retry backoff never fires after a throttled response"
	targets := []exploreTarget{
		{node: declared[0], source: "func Declared00() { executeAll() }"},
		{node: declared[1], source: "func Declared01() { executeOne() }"},
	}
	targets = append(targets, outlineBreadthTargets(8)...)
	routes := exploreLocalizationRefinementRoutes(targets)

	build := func(budget int) localizationExploreEnvelope {
		outline := localizationLeadingFileOutlineProvider(
			outlinePool(declared[0], declared[1]), targets, exploreTerminalTerms(task),
			func(string) []*graph.Node { return declared },
		)
		result, completion, _, _ := buildLocalizationRefinementResultForTaskWithOutline(
			declared[0].ID, task, targets, budget, routes, outline,
		)
		if completion.State != localizationStateNeedsRefinement {
			t.Fatalf("completion state = %q, want %q", completion.State, localizationStateNeedsRefinement)
		}
		if bytes := outlineEnvelopeBytes(t, result); bytes > budget*localizationEnvelopeBytesPerToken {
			t.Fatalf("envelope = %d bytes, budget = %d", bytes, budget*localizationEnvelopeBytesPerToken)
		}
		return outlineEnvelope(t, result)
	}

	roomy := build(exploreMaxBudgetTokens)
	if roomy.Outline == nil || len(roomy.Outline.Rows) != localizationOutlineRowCap {
		t.Fatalf("roomy page outline = %#v, want the full %d rows", roomy.Outline, localizationOutlineRowCap)
	}

	page := build(localizationDefaultBudgetTokens)
	if page.Outline == nil {
		t.Fatal("the default budget shed the leading-file outline entirely")
	}
	if len(page.Outline.Rows) >= localizationOutlineRowCap {
		t.Fatalf("fixture is not under budget pressure: %d rows survived at the default budget",
			len(page.Outline.Rows))
	}
	if len(page.Outline.Rows) < localizationOutlineFloorRows {
		t.Fatalf("outline shrank past its floor: %d rows, floor %d",
			len(page.Outline.Rows), localizationOutlineFloorRows)
	}
	if page.Outline.Declared != len(declared) {
		t.Fatalf("outline declared = %d, want the file's %d", page.Outline.Declared, len(declared))
	}
	if !outlineRowNamed(page.Outline, named) {
		t.Fatalf("shrinking dropped the declaration the task names: %#v", page.Outline.Rows)
	}
	if len(page.Evidence) != len(roomy.Evidence) {
		t.Fatalf("evidence rows = %d, want the %d the same page carries with room to spare",
			len(page.Evidence), len(roomy.Evidence))
	}
}

func TestOutlineShrinkingNeverEscapesTheProviderCache(t *testing.T) {
	declared := outlineDeclaredFile(40)
	targets := []exploreTarget{{node: declared[0]}, {node: declared[1]}}
	targets = append(targets, outlineBreadthTargets(8)...)
	outline := localizationLeadingFileOutlineProvider(
		outlinePool(declared[0], declared[1]), targets, nil,
		func(string) []*graph.Node { return declared },
	)
	routes := exploreLocalizationRefinementRoutes(targets)
	// The same provider serves every envelope one request packs. A page that
	// shrank its outline must not hand the next page a shrunken one.
	tight, _, _, _ := buildLocalizationRefinementResultForTaskWithOutline(
		declared[0].ID, "find the declared implementation", targets,
		localizationDefaultBudgetTokens, routes, outline,
	)
	roomy, _, _, _ := buildLocalizationRefinementResultForTaskWithOutline(
		declared[0].ID, "find the declared implementation", targets,
		exploreMaxBudgetTokens, routes, outline,
	)
	tightPage, roomyPage := outlineEnvelope(t, tight), outlineEnvelope(t, roomy)
	if tightPage.Outline == nil || roomyPage.Outline == nil {
		t.Fatalf("outlines = %#v tight, %#v roomy; want both pages indexed",
			tightPage.Outline, roomyPage.Outline)
	}
	tightRows := len(tightPage.Outline.Rows)
	roomyRows := len(roomyPage.Outline.Rows)
	if roomyRows != localizationOutlineRowCap || roomyRows <= tightRows {
		t.Fatalf("outline rows = %d tight, %d roomy; want the cached outline unshrunk at %d",
			tightRows, roomyRows, localizationOutlineRowCap)
	}
}

func TestLeadingFileOutlineProviderCarriesTheTaskTerms(t *testing.T) {
	const named = "dispatchHttpRequest"
	declared := outlineDeclaredFile(60)
	middle := len(declared) / 2
	declared[middle] = outlineDeclaration(named, middle+1)
	targets := []exploreTarget{{node: declared[0]}, {node: declared[1]}}
	outline := localizationLeadingFileOutlineProvider(
		outlinePool(declared[0], declared[1]), targets,
		exploreTerminalTerms("the http request dispatch never retries"),
		func(string) []*graph.Node { return declared },
	)()
	if !outlineRowNamed(outline, named) {
		t.Fatalf("provider built an outline blind to the task: %#v", outline)
	}
}

func TestOutlineElisionRanksStrongerTaskTermMatchesFirst(t *testing.T) {
	declared := outlineDeclaredFile(120)
	// More task-term matches than the cap can hold: the strongest match must
	// still survive, ahead of the rows matching a single term.
	for index := 20; index < 100; index++ {
		declared[index] = outlineDeclaration(fmt.Sprintf("retryOnce%02d", index), index+1)
	}
	declared[60] = outlineDeclaration("retryBackoffSchedule", 61)
	terms := exploreTerminalTerms("the retry backoff schedule never fires")
	outline := newLocalizationFileOutlineForTerms(outlineLeadingFile, declared, terms, localizationOutlineRowCap)
	if !outlineRowNamed(outline, "retryBackoffSchedule") {
		t.Fatalf("outline dropped the strongest task-term match: %#v", outline)
	}
}

func TestScatteredRankingCarriesNoOutline(t *testing.T) {
	var pool []*rerank.Candidate
	var targets []exploreTarget
	for index := 0; index < 6; index++ {
		node := &graph.Node{
			ID:        fmt.Sprintf("repo/scatter_%d.go::Scatter%d", index, index),
			Name:      fmt.Sprintf("Scatter%d", index),
			Kind:      graph.KindFunction,
			FilePath:  fmt.Sprintf("repo/scatter_%d.go", index),
			StartLine: 3,
		}
		pool = append(pool, &rerank.Candidate{Node: node, TextRank: index, VectorRank: -1})
		targets = append(targets, exploreTarget{node: node})
	}
	enumerated := 0
	outline := localizationLeadingFileOutlineProvider(pool, targets, nil, func(string) []*graph.Node {
		enumerated++
		return nil
	})
	if page := outline(); page != nil {
		t.Fatalf("scattered ranking produced an outline: %#v", page)
	}
	if enumerated != 0 {
		t.Fatalf("scattered ranking paid %d graph enumerations", enumerated)
	}
}
