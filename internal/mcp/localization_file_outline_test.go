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
	outline := localizationPageOutlineProvider(
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
	outline := localizationPageOutlineProvider(
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
		var outline func() *localizationPageOutline
		if withOutline {
			outline = localizationPageOutlineProvider(
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
	if localizationDefaultBudgetTokens != 12000 {
		t.Fatalf("localization default budget = %d, want 12000", localizationDefaultBudgetTokens)
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
	outline := localizationPageOutlineProvider(
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
	outline := localizationPageOutlineProvider(
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
	outline := localizationPageOutlineProvider(
		outlinePool(declared...), targets, nil,
		func(string) []*graph.Node { return nil },
	)()
	if outline == nil || outline.Leading == nil || outline.Leading.Declared != len(declared) {
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

// outlineFileDeclaration mirrors outlineDeclaration for a file other than the
// page's leading one.
func outlineFileDeclaration(file, name string, line int) *graph.Node {
	return &graph.Node{
		ID:        file + "::" + name,
		Name:      name,
		QualName:  name,
		Kind:      graph.KindMethod,
		FilePath:  file,
		StartLine: line,
		EndLine:   line + 4,
		Meta:      map[string]any{"signature": "func " + name + "()"},
	}
}

func TestOutlineCoversTheOtherFilesTheRowsCameFrom(t *testing.T) {
	// The ranked page leads with one file and names another only in its last
	// rows. The rows of that second file are one member each; the declaration
	// that answers the task is a sibling the rows never reach.
	const otherFile = "repo/other.go"
	const answer = "onCollisionRemoved"
	declared := outlineDeclaredFile(12)
	other := []*graph.Node{
		outlineFileDeclaration(otherFile, "OtherType", 4),
		outlineFileDeclaration(otherFile, "renderTree", 20),
		outlineFileDeclaration(otherFile, answer, 41),
		outlineFileDeclaration(otherFile, "attachChild", 60),
	}
	targets := []exploreTarget{
		{node: declared[0], source: "func Declared00() { executeAll() }"},
		{node: declared[1], source: "func Declared01() { executeOne() }"},
	}
	targets = append(targets, outlineBreadthTargets(4)...)
	targets = append(targets, exploreTarget{node: other[0]}, exploreTarget{node: other[1]})

	enumerated := make([]string, 0, 4)
	outline := localizationPageOutlineProvider(
		outlinePool(declared[0], declared[1]), targets, nil,
		func(file string) []*graph.Node {
			enumerated = append(enumerated, file)
			switch file {
			case outlineLeadingFile:
				return declared
			case otherFile:
				return other
			}
			return nil
		},
	)
	result, completion, _, _ := buildLocalizationRefinementResultForTaskWithOutline(
		declared[0].ID, "find the declared implementation", targets,
		exploreMaxBudgetTokens, exploreLocalizationRefinementRoutes(targets), outline,
	)
	if completion.State != localizationStateNeedsRefinement {
		t.Fatalf("completion state = %q, want %q", completion.State, localizationStateNeedsRefinement)
	}
	envelope := outlineEnvelope(t, result)
	if envelope.Outline == nil || envelope.Outline.File != outlineLeadingFile {
		t.Fatalf("leading outline = %#v, want %q", envelope.Outline, outlineLeadingFile)
	}
	var second *localizationFileOutline
	for _, extra := range envelope.Outlines {
		if extra != nil && extra.File == otherFile {
			second = extra
		}
	}
	if second == nil {
		t.Fatalf("outlines = %#v, want the page's other file %q", envelope.Outlines, otherFile)
	}
	if !outlineRowNamed(second, answer) {
		t.Fatalf("the other file's outline never names its unranked declaration: %#v", second.Rows)
	}
	// The leading file keeps the deepest index; the files after it are shallower.
	if len(envelope.Outlines) == 0 {
		t.Fatal("no further page files were indexed")
	}
	for _, extra := range envelope.Outlines {
		if extra.File == outlineLeadingFile {
			t.Fatalf("the leading file is indexed twice: %#v", envelope.Outlines)
		}
	}
	for _, file := range enumerated {
		if file == "" {
			t.Fatalf("enumerated an empty file: %#v", enumerated)
		}
	}
	if len(enumerated) > localizationOutlineFileCap {
		t.Fatalf("enumerated %d files, cap %d", len(enumerated), localizationOutlineFileCap)
	}
}

func TestPageOutlinesGiveBackDepthBeforeTheyGiveBackAFile(t *testing.T) {
	const otherFile = "repo/other.go"
	declared := outlineDeclaredFile(40)
	other := make([]*graph.Node, 0, 20)
	for index := 0; index < 20; index++ {
		other = append(other, outlineFileDeclaration(otherFile, fmt.Sprintf("Other%02d", index), index*3+1))
	}
	targets := []exploreTarget{
		{node: declared[0], source: "func Declared00() { executeAll() }"},
		{node: declared[1], source: "func Declared01() { executeOne() }"},
	}
	targets = append(targets, outlineBreadthTargets(4)...)
	targets = append(targets, exploreTarget{node: other[0]}, exploreTarget{node: other[1]})
	routes := exploreLocalizationRefinementRoutes(targets)

	build := func(budget int) localizationExploreEnvelope {
		outline := localizationPageOutlineProvider(
			outlinePool(declared[0], declared[1]), targets, nil,
			func(file string) []*graph.Node {
				switch file {
				case outlineLeadingFile:
					return declared
				case otherFile:
					return other
				}
				return nil
			},
		)
		result, _, _, _ := buildLocalizationRefinementResultForTaskWithOutline(
			declared[0].ID, "find the declared implementation", targets, budget, routes, outline,
		)
		if bytes := outlineEnvelopeBytes(t, result); bytes > budget*localizationEnvelopeBytesPerToken {
			t.Fatalf("envelope = %d bytes, budget = %d", bytes, budget*localizationEnvelopeBytesPerToken)
		}
		return outlineEnvelope(t, result)
	}

	roomy := build(exploreMaxBudgetTokens)
	if roomy.Outline == nil || len(roomy.Outlines) == 0 {
		t.Fatalf("roomy page = %#v / %#v, want both files indexed", roomy.Outline, roomy.Outlines)
	}
	tight := build(exploreMinBudgetTokens)
	tightRows := 0
	if tight.Outline != nil {
		tightRows += len(tight.Outline.Rows)
	}
	for _, extra := range tight.Outlines {
		tightRows += len(extra.Rows)
	}
	roomyRows := len(roomy.Outline.Rows)
	for _, extra := range roomy.Outlines {
		roomyRows += len(extra.Rows)
	}
	if tightRows >= roomyRows {
		t.Fatalf("tight page indexed %d rows against the roomy page's %d", tightRows, roomyRows)
	}
	// Whatever is left of the block still leads with the file the page led with.
	if len(tight.Outlines) > 0 && tight.Outline == nil {
		t.Fatalf("the leading file's index went before a later file's: %#v", tight.Outlines)
	}
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
		outline := localizationPageOutlineProvider(
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

	// A deliberately tight budget: the shrink path is about pressure, not
	// about whatever the localize default happens to be.
	const tightBudgetTokens = 2400
	page := build(tightBudgetTokens)
	if page.Outline == nil {
		t.Fatal("the tight budget shed the leading-file outline entirely")
	}
	if len(page.Outline.Rows) >= localizationOutlineRowCap {
		t.Fatalf("fixture is not under budget pressure: %d rows survived at the tight budget",
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

// outlineBudgetFillingTargets is a page whose ranked rows fill the localize
// default budget on their own — the shape that left no room for any index at
// all. Each row carries the qualified name, signature, and neighbour
// identifiers a real evidence row serializes.
func outlineBudgetFillingTargets(count int, deep bool) []exploreTarget {
	targets := make([]exploreTarget, 0, count)
	for index := 0; index < count; index++ {
		name := fmt.Sprintf("BreadthCandidate%02d", index)
		dir := fmt.Sprintf("repo/breadth/service/%02d", index)
		if deep {
			dir += "/subsystem/internal"
		}
		qual := dir + "/handler." + name + ".ExecuteWithRetriesAndBackoff"
		neighbors := make([]*graph.Node, 0, 3)
		for neighbor := 0; neighbor < 3; neighbor++ {
			neighbors = append(neighbors, &graph.Node{
				ID: fmt.Sprintf("%s/neighbor_%d.go::NeighbouringCandidate%02d%d.Execute", dir, neighbor, index, neighbor),
			})
		}
		targets = append(targets, exploreTarget{
			node: &graph.Node{
				ID:        dir + "/handler.go::" + name,
				Name:      name,
				QualName:  qual,
				Kind:      graph.KindFunction,
				FilePath:  dir + "/handler.go",
				StartLine: 24,
				EndLine:   96,
				Meta: map[string]any{
					"signature": "func (" + name + ") ExecuteWithRetriesAndBackoff(ctx context.Context, request *BreadthRequest, options ...BreadthOption) (*BreadthResponse, error)",
					"qualname":  qual,
				},
			},
			callers: neighbors,
			callees: neighbors,
		})
	}
	return targets
}

func TestOutlineFloorOutranksTheTrailingRowsExpansionDetail(t *testing.T) {
	const named = "computeRetryBackoff"
	declared := outlineDeclaredFile(20)
	middle := len(declared) / 2
	declared[middle] = outlineDeclaration(named, middle+1)
	task := "the retry backoff never fires after a throttled response"
	targets := []exploreTarget{
		{node: declared[0], source: "func Declared00() { executeAll() }"},
		{node: declared[1], source: "func Declared01() { executeOne() }"},
	}
	targets = append(targets, outlineBudgetFillingTargets(8, false)...)
	routes := exploreLocalizationRefinementRoutes(targets)

	build := func(withOutline bool) localizationExploreEnvelope {
		var outline func() *localizationPageOutline
		if withOutline {
			outline = localizationPageOutlineProvider(
				outlinePool(declared[0], declared[1]), targets, exploreTerminalTerms(task),
				func(file string) []*graph.Node {
					if file == outlineLeadingFile {
						return declared
					}
					return nil
				},
			)
		}
		result, completion, _, _ := buildLocalizationRefinementResultForTaskWithOutline(
			declared[0].ID, task, targets, localizationDefaultBudgetTokens, routes, outline,
		)
		if completion.State != localizationStateNeedsRefinement {
			t.Fatalf("completion state = %q, want %q", completion.State, localizationStateNeedsRefinement)
		}
		budget := localizationDefaultBudgetTokens * localizationEnvelopeBytesPerToken
		if bytes := outlineEnvelopeBytes(t, result); bytes > budget {
			t.Fatalf("envelope = %d bytes, budget = %d", bytes, budget)
		}
		return outlineEnvelope(t, result)
	}

	bare := build(false)
	if bare.Outline != nil {
		t.Fatalf("the no-outline build produced one: %#v", bare.Outline)
	}
	// The fixture only proves anything if the rows really did fill the budget:
	// a page with room to spare never has to trade anything for its index.
	if len(bare.Evidence) != len(targets) {
		t.Fatalf("bare evidence rows = %d, want all %d ranked rows", len(bare.Evidence), len(targets))
	}
	page := build(true)
	if page.Outline == nil || len(page.Outline.Rows) < localizationOutlineFloorRows {
		t.Fatalf("a page whose rows filled the budget carries no index: %#v", page.Outline)
	}
	if !outlineRowNamed(page.Outline, named) {
		t.Fatalf("the retained index misses the declaration the task names: %#v", page.Outline.Rows)
	}
	// Not one ranked row was given up for it, and none lost its identity.
	if !reflect.DeepEqual(page.Symbols, bare.Symbols) || !reflect.DeepEqual(page.Files, bare.Files) {
		t.Fatalf("the index cost ranked rows: %d/%d symbols, %d/%d files",
			len(page.Symbols), len(bare.Symbols), len(page.Files), len(bare.Files))
	}
	for index, row := range page.Evidence {
		if row.ID != bare.Evidence[index].ID || row.Line != bare.Evidence[index].Line ||
			row.File != bare.Evidence[index].File || row.Name != bare.Evidence[index].Name {
			t.Fatalf("row %d lost its identity: %#v against %#v", index, row, bare.Evidence[index])
		}
	}
	// The detail it traded came off the expendable tail, never the rows the
	// refinement contract is allowed to name.
	for index := 0; index < min(localizationDirectEvidenceReserve, len(page.Evidence)); index++ {
		if !reflect.DeepEqual(page.Evidence[index], bare.Evidence[index]) {
			t.Fatalf("row %d inside the direct-evidence reserve gave up detail: %#v against %#v",
				index, page.Evidence[index], bare.Evidence[index])
		}
	}
}

func TestFurtherFilesGiveWayBeforeTheLeadingFilesDepth(t *testing.T) {
	leading := &localizationFileOutline{File: "repo/lead.go"}
	leading.all = make([]localizationOutlineRow, 0, 30)
	for index := 0; index < 30; index++ {
		leading.all = append(leading.all, localizationOutlineRow{
			Name: fmt.Sprintf("Declared%02d", index), Line: index + 1, Kind: "f",
		})
	}
	leading.Declared = len(leading.all)
	leading.elide(localizationOutlineRowCap)
	page := &localizationPageOutline{Leading: leading}
	for _, file := range []string{"repo/second.go", "repo/third.go"} {
		other := &localizationFileOutline{File: file}
		for index := 0; index < 20; index++ {
			other.all = append(other.all, localizationOutlineRow{
				Name: fmt.Sprintf("Other%02d", index), Line: index + 1, Kind: "f",
			})
		}
		other.Declared = len(other.all)
		other.elide(localizationOutlineSecondFileRowCap)
		page.Others = append(page.Others, other)
	}

	// Pressure until nothing is left, recording the leading file's depth at the
	// moment each further file went.
	depthWhenAFileWent := []int{}
	for guard := 0; !page.empty(); guard++ {
		if guard > 200 {
			t.Fatal("relief never converged")
		}
		before := len(page.Others)
		leadingRows := 0
		if page.Leading != nil {
			leadingRows = len(page.Leading.Rows)
		}
		page.relieve()
		if len(page.Others) < before {
			depthWhenAFileWent = append(depthWhenAFileWent, leadingRows)
		}
	}
	if len(depthWhenAFileWent) != 2 {
		t.Fatalf("further files dropped %d times, want 2", len(depthWhenAFileWent))
	}
	for _, depth := range depthWhenAFileWent {
		if depth < localizationOutlineSecondFileRowCap {
			t.Fatalf("a further file survived the leading file's depth falling to %d, floor %d",
				depth, localizationOutlineSecondFileRowCap)
		}
	}
}

func TestATradeThatCannotSaveTheOutlineIsGivenBack(t *testing.T) {
	const named = "computeRetryBackoff"
	declared := outlineDeclaredFile(20)
	middle := len(declared) / 2
	declared[middle] = outlineDeclaration(named, middle+1)
	task := "the retry backoff never fires after a throttled response"
	targets := []exploreTarget{
		{node: declared[0], source: "func Declared00() { executeAll() }"},
		{node: declared[1], source: "func Declared01() { executeOne() }"},
	}
	// One expendable row on a page too tight for even a floor-sized index:
	// whatever that row gives up cannot buy the index, so it keeps it.
	targets = append(targets, outlineBudgetFillingTargets(7, true)...)
	routes := exploreLocalizationRefinementRoutes(targets)

	build := func(withOutline bool) localizationExploreEnvelope {
		var outline func() *localizationPageOutline
		if withOutline {
			outline = localizationPageOutlineProvider(
				outlinePool(declared[0], declared[1]), targets, exploreTerminalTerms(task),
				func(file string) []*graph.Node {
					if file == outlineLeadingFile {
						return declared
					}
					return nil
				},
			)
		}
		result, _, _, _ := buildLocalizationRefinementResultForTaskWithOutline(
			declared[0].ID, task, targets, localizationDefaultBudgetTokens, routes, outline,
		)
		return outlineEnvelope(t, result)
	}

	bare := build(false)
	page := build(true)
	if page.Outline != nil {
		t.Skip("the fixture is no longer tight enough to drop the index")
	}
	if !reflect.DeepEqual(page.Evidence, bare.Evidence) {
		t.Fatalf("a page that dropped its index still paid for it:\n%#v\nagainst\n%#v",
			page.Evidence, bare.Evidence)
	}
}

func TestOutlineShrinkingNeverEscapesTheProviderCache(t *testing.T) {
	declared := outlineDeclaredFile(40)
	targets := []exploreTarget{{node: declared[0]}, {node: declared[1]}}
	targets = append(targets, outlineBreadthTargets(8)...)
	outline := localizationPageOutlineProvider(
		outlinePool(declared[0], declared[1]), targets, nil,
		func(string) []*graph.Node { return declared },
	)
	routes := exploreLocalizationRefinementRoutes(targets)
	// The same provider serves every envelope one request packs. A page that
	// shrank its outline must not hand the next page a shrunken one.
	// An explicitly tight budget — the point is a page that had to shrink,
	// independent of what the localize default is tuned to.
	tight, _, _, _ := buildLocalizationRefinementResultForTaskWithOutline(
		declared[0].ID, "find the declared implementation", targets,
		2400, routes, outline,
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
	outline := localizationPageOutlineProvider(
		outlinePool(declared[0], declared[1]), targets,
		exploreTerminalTerms("the http request dispatch never retries"),
		func(string) []*graph.Node { return declared },
	)()
	if outline == nil || !outlineRowNamed(outline.Leading, named) {
		t.Fatalf("provider built an outline blind to the task: %#v", outline)
	}
}

func TestOutlineElisionLeavesHalfTheIndexToTheFileItself(t *testing.T) {
	// A task whose words happen to name half a file must not turn that file's
	// index into a list of its own query. The declaration a caller is looking
	// for is often the one the task could not name.
	declared := outlineDeclaredFile(25)
	for _, index := range []int{2, 3, 4, 5, 10, 11, 17, 18} {
		declared[index] = outlineDeclaration(fmt.Sprintf("retryBackoff%02d", index), index+1)
	}
	const named = "send"
	declared[22] = outlineDeclaration(named, 23)
	terms := exploreTerminalTerms("the retry backoff never fires")
	outline := newLocalizationFileOutlineForTerms(outlineLeadingFile, declared, terms, 12)
	if outline == nil || len(outline.Rows) != 12 {
		t.Fatalf("outline = %#v, want 12 rows", outline)
	}
	matched := 0
	for _, row := range outline.Rows {
		if strings.HasPrefix(row.Name, "retryBackoff") {
			matched++
		}
	}
	if matched > 6 {
		t.Fatalf("task-term matches took %d of 12 rows, want at most half", matched)
	}
	if outline.Rows[0].Line != 1 || outline.Rows[len(outline.Rows)-1].Line != len(declared) {
		t.Fatalf("outline spans lines %d..%d, want the file's own ends",
			outline.Rows[0].Line, outline.Rows[len(outline.Rows)-1].Line)
	}
	if !outlineRowNamed(outline, named) {
		t.Fatalf("the file's own tail lost a declaration the task could not name: %#v", outline.Rows)
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

func TestScatteredRankingIndexesThePageFilesWithoutALeadingOne(t *testing.T) {
	var pool []*rerank.Candidate
	var targets []exploreTarget
	declared := map[string][]*graph.Node{}
	for index := 0; index < 6; index++ {
		file := fmt.Sprintf("repo/scatter_%d.go", index)
		node := &graph.Node{
			ID:        file + fmt.Sprintf("::Scatter%d", index),
			Name:      fmt.Sprintf("Scatter%d", index),
			Kind:      graph.KindFunction,
			FilePath:  file,
			StartLine: 3,
		}
		pool = append(pool, &rerank.Candidate{Node: node, TextRank: index, VectorRank: -1})
		targets = append(targets, exploreTarget{node: node})
		for sibling := 0; sibling < 4; sibling++ {
			declared[file] = append(declared[file], &graph.Node{
				ID:        file + fmt.Sprintf("::Sibling%d%d", index, sibling),
				Name:      fmt.Sprintf("Sibling%d%d", index, sibling),
				Kind:      graph.KindFunction,
				FilePath:  file,
				StartLine: 10 + sibling*5,
			})
		}
	}
	enumerated := 0
	page := localizationPageOutlineProvider(pool, targets, nil, func(file string) []*graph.Node {
		enumerated++
		return declared[file]
	})()
	if page == nil {
		t.Fatal("a scattered page carries no index at all")
	}
	// Ranking never settled, so no file earns the leading slot — but the rows
	// still name files, and each of them declares siblings the rows never show.
	if page.Leading != nil {
		t.Fatalf("a scattered page elected a leading file: %#v", page.Leading)
	}
	if len(page.Others) == 0 {
		t.Fatal("a scattered page indexed none of the files its rows named")
	}
	if len(page.Others) > localizationOutlineFileCap {
		t.Fatalf("indexed %d files, cap %d", len(page.Others), localizationOutlineFileCap)
	}
	if enumerated > localizationOutlineFileCap {
		t.Fatalf("scattered ranking paid %d graph enumerations, cap %d",
			enumerated, localizationOutlineFileCap)
	}
	for _, other := range page.Others {
		if len(other.Rows) > localizationOutlineSecondFileRowCap {
			t.Fatalf("a file on a page with no leading one took the deepest slice: %#v", other)
		}
	}
}
