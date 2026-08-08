package mcp

import (
	"sort"
	"strings"
	"unicode"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search/rerank"
)

// Page-file outlines.
//
// A bounded localization page shows ten signature rows, and per-file
// diversification spends most of those slots on distinct files. When the answer
// is a declaration in a file the page already names, but not one of the rows
// that file won, nothing on the page says the file declares it at all — so the
// caller searches again for something it is already looking at. The outline is
// that file's declaration index: name and line, no signatures and no bodies.
// The page's leading file gets the deepest index and each further page file a
// shallower one, because a row that lost its file's only slot is as invisible
// at rank nine as at rank one. Only a page that still asks the caller to choose
// carries outlines; a terminal page's caller is answering, not choosing.

const (
	// localizationOutlineRowCap bounds the rows the leading file's outline may
	// serialize.
	localizationOutlineRowCap = 40
	// localizationOutlineHeadRows splits an elided outline between the file's
	// opening declarations and its closing ones.
	localizationOutlineHeadRows = localizationOutlineRowCap / 2
	// localizationOutlineFloorRows is the smallest index still worth its bytes.
	// Budget pressure shrinks an outline to this floor before it may drop one.
	localizationOutlineFloorRows = 8
	// localizationOutlineSecondFileRowCap is the depth the file ranked directly
	// after the leading one gets. Every file after that starts at the floor.
	localizationOutlineSecondFileRowCap = 12
	// localizationOutlineFileCap bounds how many of the page's distinct files
	// are indexed at all, leading file included.
	localizationOutlineFileCap = 6
)

// localizationOutlineFileRowCap is the depth ladder over a page's files: the
// leading file keeps the whole index, the next one a third of it, and the rest
// start where shrinking would stop anyway.
func localizationOutlineFileRowCap(rank int) int {
	switch rank {
	case 0:
		return localizationOutlineRowCap
	case 1:
		return localizationOutlineSecondFileRowCap
	}
	return localizationOutlineFloorRows
}

// localizationPageOutline is the page's declaration index: the leading file's
// outline and the outlines of the further page files, deepest first.
type localizationPageOutline struct {
	Leading *localizationFileOutline
	Others  []*localizationFileOutline
}

type localizationOutlineRow struct {
	Name string `json:"name"`
	Line int    `json:"line"`
	// Kind is a one-rune hint. The row is a locator, so a coarse hint costs a
	// byte and the name still decides.
	Kind string `json:"kind,omitempty"`
}

// localizationFileOutline is the bounded declaration index of a page file.
// Declared counts what the file declares; Elided counts the rows the cap
// dropped from the middle.
//
// The unexported fields are the whole file in line order plus the task-term
// priority over it, retained so the same outline can be re-elided at a smaller
// cap without re-reading the graph. They are never serialized.
type localizationFileOutline struct {
	File     string                   `json:"file"`
	Declared int                      `json:"declared"`
	Elided   int                      `json:"elided,omitempty"`
	Rows     []localizationOutlineRow `json:"rows"`

	all      []localizationOutlineRow
	priority []int
}

// localizationPageAcceptsOutline admits the outline on the states whose caller
// still has to choose where to look.
func localizationPageAcceptsOutline(state string) bool {
	switch state {
	case localizationStateNeedsRefinement, localizationStateNeedsRecovery,
		localizationStateNeedsExactRead:
		return true
	}
	return false
}

// localizationPageOutlineProvider defers the file enumeration until a
// page is known to still be refining, and performs it at most once per page.
// A nil enumerator disables the outlines entirely.
func localizationPageOutlineProvider(
	pool []*rerank.Candidate,
	targets []exploreTarget,
	terms map[string]struct{},
	enumerate func(string) []*graph.Node,
) func() *localizationPageOutline {
	if enumerate == nil {
		return nil
	}
	var (
		page  *localizationPageOutline
		built bool
	)
	return func() *localizationPageOutline {
		if built {
			return page
		}
		built = true
		index := func(file string, rank int) *localizationFileOutline {
			nodes := enumerate(file)
			if len(nodes) == 0 {
				// Nothing to enumerate leaves the nodes this page already
				// fetched, which is the whole of what it knows about that file.
				nodes = localizationOutlineFetchedNodes(pool, targets)
			}
			return newLocalizationFileOutlineForTerms(file, nodes, terms, localizationOutlineFileRowCap(rank))
		}
		// A page whose ranking never settled on one file has no leading slice to
		// give — but its rows still name files, and every one of them declares
		// siblings the rows could not show. Those files are indexed at the
		// shallower depths instead, so scattered ranking costs the caller an
		// index only where there is nothing to index.
		leading := localizationOutlineLeadingFile(pool, targets)
		following := localizationOutlineFileCap
		page = &localizationPageOutline{}
		if leading != "" {
			if outline := index(leading, 0); outline != nil {
				page.Leading = outline
				following--
			} else {
				leading = ""
			}
		}
		ranked := localizationOutlinePageRowCounts(targets)
		for _, file := range localizationOutlineFollowingFiles(targets, leading, following) {
			other := index(file, len(page.Others)+1)
			if other == nil || other.Declared <= ranked[file] {
				// A file whose every declaration is already a row on this page
				// is a file the caller can already see. Indexing it costs bytes
				// the deeper files would rather have.
				continue
			}
			page.Others = append(page.Others, other)
		}
		if page.empty() {
			page = nil
		}
		return page
	}
}

// localizationOutlineFollowingFiles names the page's other distinct files in
// page order. The rows are already ranked, so their file order is the page's own
// judgement of which neighbourhood the caller should look at next.
func localizationOutlineFollowingFiles(targets []exploreTarget, leading string, limit int) []string {
	if limit <= 0 {
		return nil
	}
	files := make([]string, 0, limit)
	seen := map[string]struct{}{leading: {}}
	for _, target := range targets {
		if len(files) >= limit {
			break
		}
		if target.node == nil {
			continue
		}
		file := nodeDisplayPath(target.node)
		if file == "" {
			continue
		}
		if _, duplicate := seen[file]; duplicate {
			continue
		}
		seen[file] = struct{}{}
		files = append(files, file)
	}
	return files
}

// localizationOutlineLeadingFile reuses the ranked leading-file notion depth
// allocation settled on, falling back to the page's own order for the query
// classes that never diversify by file and so retain no pre-diversification
// pool.
func localizationOutlineLeadingFile(pool []*rerank.Candidate, targets []exploreTarget) string {
	if file := exploreLeadingRankedFile(pool); file != "" {
		return file
	}
	ranked := make([]*rerank.Candidate, 0, len(targets))
	for _, target := range targets {
		if target.node == nil {
			continue
		}
		ranked = append(ranked, &rerank.Candidate{Node: target.node})
	}
	return exploreLeadingRankedFile(ranked)
}

// localizationOutlinePageRowCounts counts how many of a file's declarations the
// page already names as ranked rows.
func localizationOutlinePageRowCounts(targets []exploreTarget) map[string]int {
	counts := make(map[string]int, len(targets))
	for _, target := range targets {
		if target.node == nil {
			continue
		}
		if file := nodeDisplayPath(target.node); file != "" {
			counts[file]++
		}
	}
	return counts
}

func localizationOutlineFetchedNodes(pool []*rerank.Candidate, targets []exploreTarget) []*graph.Node {
	nodes := make([]*graph.Node, 0, len(pool)+len(targets))
	for _, candidate := range pool {
		if candidate != nil && candidate.Node != nil {
			nodes = append(nodes, candidate.Node)
		}
	}
	for _, target := range targets {
		if target.node != nil {
			nodes = append(nodes, target.node)
		}
	}
	return nodes
}

// newLocalizationFileOutline projects one file's declarations into file order
// at the default cap, with no task to prioritize by.
func newLocalizationFileOutline(file string, nodes []*graph.Node) *localizationFileOutline {
	return newLocalizationFileOutlineForTerms(file, nodes, nil, localizationOutlineRowCap)
}

// newLocalizationFileOutlineForTerms projects one file's declarations into file
// order. Nodes from another file are dropped, so a candidate pool is as valid an
// input as a file enumeration. Rows whose name carries a task term are ranked
// ahead of the file's own head and tail for whatever the cap can hold.
func newLocalizationFileOutlineForTerms(
	file string,
	nodes []*graph.Node,
	terms map[string]struct{},
	rowCap int,
) *localizationFileOutline {
	file = strings.TrimSpace(file)
	if file == "" {
		return nil
	}
	rows := make([]localizationOutlineRow, 0, len(nodes))
	seen := make(map[string]struct{}, len(nodes))
	for _, node := range nodes {
		if node == nil || nodeDisplayPath(node) != file || isNonDefinitionNode(node.Kind) {
			continue
		}
		name := strings.TrimSpace(node.QualName)
		if name == "" {
			name = strings.TrimSpace(node.Name)
		}
		if name == "" {
			continue
		}
		key := node.ID
		if key == "" {
			key = name
		}
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		rows = append(rows, localizationOutlineRow{
			Name: compactLocalizationField(name, localizationMaxNameRunes),
			Line: node.StartLine,
			Kind: localizationOutlineKindLetter(node.Kind),
		})
	}
	if len(rows) == 0 {
		return nil
	}
	sort.SliceStable(rows, func(first, second int) bool {
		if rows[first].Line != rows[second].Line {
			return rows[first].Line < rows[second].Line
		}
		return rows[first].Name < rows[second].Name
	})
	outline := &localizationFileOutline{
		File:     file,
		Declared: len(rows),
		all:      rows,
		priority: localizationOutlinePriority(rows, terms),
	}
	outline.elide(rowCap)
	return outline
}

// localizationOutlinePriority ranks the declarations a task names: more distinct
// task terms first, then the longer match, then file order. A row that names
// nothing the task said is not in the result at all.
func localizationOutlinePriority(rows []localizationOutlineRow, terms map[string]struct{}) []int {
	if len(terms) == 0 {
		return nil
	}
	type match struct{ index, matched, longest int }
	matches := make([]match, 0, len(rows))
	for index, row := range rows {
		matched, longest := localizationOutlineRowTermMatch(row.Name, terms)
		if matched == 0 {
			continue
		}
		matches = append(matches, match{index: index, matched: matched, longest: longest})
	}
	sort.SliceStable(matches, func(first, second int) bool {
		if matches[first].matched != matches[second].matched {
			return matches[first].matched > matches[second].matched
		}
		if matches[first].longest != matches[second].longest {
			return matches[first].longest > matches[second].longest
		}
		return matches[first].index < matches[second].index
	})
	priority := make([]int, 0, len(matches))
	for _, ranked := range matches {
		priority = append(priority, ranked.index)
	}
	return priority
}

// localizationOutlineRowTermMatch counts the distinct task terms a declaration
// name carries, over the same camel/snake tokenization and the same root form
// the task's own terms were built with.
func localizationOutlineRowTermMatch(name string, terms map[string]struct{}) (matched, longest int) {
	if len(terms) == 0 {
		return 0, 0
	}
	seen := make(map[string]struct{}, len(terms))
	for _, raw := range rerank.Tokenize(name) {
		token := exploreTerminalTermRoot(strings.ToLower(strings.TrimSpace(raw)))
		if token == "" {
			continue
		}
		if _, ok := terms[token]; !ok {
			continue
		}
		if _, duplicate := seen[token]; duplicate {
			continue
		}
		seen[token] = struct{}{}
		matched++
		if len(token) > longest {
			longest = len(token)
		}
	}
	return matched, longest
}

// elide re-projects the retained rows at rowCap. The declarations the task names
// are kept first; the file's head and tail then fill whatever the cap has left,
// so an index the caller cannot navigate by eye still shows both of its ends.
func (o *localizationFileOutline) elide(rowCap int) {
	if o == nil {
		return
	}
	if rowCap < 0 {
		rowCap = 0
	}
	if len(o.all) <= rowCap {
		o.Rows = o.all
		o.Elided = 0
		return
	}
	// The task's own matches lead, but never take the whole index: a task whose
	// words happen to name half a file would otherwise turn that file's index
	// into a list of the query, and the declaration a caller cannot name is
	// exactly the one it came here to find.
	reserve := max(rowCap/2, 1)
	kept := make(map[int]struct{}, rowCap)
	for _, index := range o.priority {
		if len(kept) >= reserve {
			break
		}
		kept[index] = struct{}{}
	}
	// Both ends of a file carry declarations a caller may want; the middle is
	// what a bounded index gives back once the task's own matches are held.
	headQuota := (rowCap - len(kept)) / 2
	for index := 0; index < len(o.all) && headQuota > 0 && len(kept) < rowCap; index++ {
		if _, duplicate := kept[index]; duplicate {
			continue
		}
		kept[index] = struct{}{}
		headQuota--
	}
	for index := len(o.all) - 1; index >= 0 && len(kept) < rowCap; index-- {
		kept[index] = struct{}{}
	}
	rows := make([]localizationOutlineRow, 0, len(kept))
	for index, row := range o.all {
		if _, keep := kept[index]; keep {
			rows = append(rows, row)
		}
	}
	o.Rows = rows
	o.Elided = len(o.all) - len(rows)
}

// clone detaches an outline from the provider's cache. One request can pack
// several envelopes from the same provider, and each envelope shrinks its own
// copy under its own budget; the retained declarations are read-only and stay
// shared.
func (o *localizationFileOutline) clone() *localizationFileOutline {
	if o == nil {
		return nil
	}
	copied := *o
	copied.Rows = append([]localizationOutlineRow(nil), o.Rows...)
	return &copied
}

// clone detaches a page's outlines from the provider's cache.
func (p *localizationPageOutline) clone() *localizationPageOutline {
	if p == nil {
		return nil
	}
	copied := &localizationPageOutline{Leading: p.Leading.clone()}
	for _, other := range p.Others {
		copied.Others = append(copied.Others, other.clone())
	}
	return copied
}

// relieve gives back one increment of outline payload. An index that no longer
// fits shrinks toward its floor before it is dropped: a shorter index still
// names the file and still carries the declarations the task asked about, where
// no index at all sends the caller back to search for what it is already
// looking at. Depth goes before breadth — the deepest index gives back first,
// and only once every file is at its floor does a file go, shallowest first.
func (p *localizationPageOutline) relieve() {
	if p == nil {
		return
	}
	floor := p.leadingFloor()
	deepest, rows := (*localizationFileOutline)(nil), floor
	if p.Leading != nil && len(p.Leading.Rows) > rows {
		deepest, rows = p.Leading, len(p.Leading.Rows)
	}
	for _, other := range p.Others {
		if other != nil && len(other.Rows) >= rows && len(other.Rows) > localizationOutlineFloorRows {
			deepest, rows, floor = other, len(other.Rows), localizationOutlineFloorRows
		}
	}
	if deepest != nil {
		deepest.elide(max(localizationOutlineNextRowCap(rows), floor))
		return
	}
	if last := len(p.Others); last > 0 {
		p.Others = p.Others[:last-1]
		return
	}
	p.Leading = nil
}

// leadingFloor is where the leading file's index stops shrinking. While the page
// still indexes further files, they are what a tight page gives back — measured,
// spending the leading file's depth on a neighbour's first twelve rows loses the
// answer more often than it finds one. Once no further file is left, the leading
// index may shrink to the same floor as any other.
func (p *localizationPageOutline) leadingFloor() int {
	if len(p.Others) > 0 {
		return localizationOutlineSecondFileRowCap
	}
	return localizationOutlineFloorRows
}

// empty reports a block with nothing left to give.
func (p *localizationPageOutline) empty() bool {
	return p == nil || (p.Leading == nil && len(p.Others) == 0)
}

// atFloor reports a block that has given back every row it can and would next
// have to give back a whole file.
func (p *localizationPageOutline) atFloor() bool {
	if p == nil {
		return true
	}
	if p.Leading != nil && len(p.Leading.Rows) > p.leadingFloor() {
		return false
	}
	for _, other := range p.Others {
		if other != nil && len(other.Rows) > localizationOutlineFloorRows {
			return false
		}
	}
	return true
}

// localizationOutlineNextRowCap steps a bounded index down in proportion to its
// size, so a wide outline converges on a fitting one in a few steps and a narrow
// one gives back only what it must.
func localizationOutlineNextRowCap(rows int) int {
	step := max(rows/4, 4)
	return max(rows-step, localizationOutlineFloorRows)
}

func localizationOutlineKindLetter(kind graph.NodeKind) string {
	for _, letter := range string(kind) {
		return string(unicode.ToLower(letter))
	}
	return ""
}
