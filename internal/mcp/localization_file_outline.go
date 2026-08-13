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
	// localizationOutlineCompleteRows asks elide to retain the complete file.
	// The top two page files start complete and yield only under real envelope
	// pressure.
	localizationOutlineCompleteRows = -1
	// localizationOutlineRowCap remains the default for direct outline helpers;
	// page-ranked outlines use the complete sentinel below.
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
	localizationOutlineFileCap = 10
	// The page's first two files keep their complete indexes until every lower
	// ranked file has yielded its expendable depth and breadth.
	localizationOutlineProtectedFileCount = 2
)

// localizationOutlineFileRowCap is the depth ladder over a page's files: the
// leading file keeps the whole index, the next one a third of it, and the rest
// start where shrinking would stop anyway.
func localizationOutlineFileRowCap(rank int) int {
	if rank >= 0 && rank < localizationOutlineProtectedFileCount {
		return localizationOutlineCompleteRows
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
	key  string
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
	rank     int
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
			outline := newLocalizationFileOutlineForTerms(file, nodes, terms, localizationOutlineFileRowCap(rank))
			if outline != nil {
				outline.rank = rank
			}
			return outline
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
		followingFiles := localizationOutlineFollowingFiles(targets, leading, following)
		for indexInPage, file := range followingFiles {
			rank := indexInPage
			if leading != "" {
				rank++
			}
			other := index(file, rank)
			if other == nil || rank >= localizationOutlineProtectedFileCount &&
				!localizationOutlineAddsUnrankedDeclaration(other, targets) {
				// Lower-ranked files whose declarations are all already visible
				// add no navigation value. The top two remain explicit even when
				// counts happen to match: identity, not cardinality, is the proof.
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

// localizationOutlineAddsUnrankedDeclaration checks actual declaration
// identities. Counts cannot prove coverage when ranked rows repeat one identity
// or omit a different sibling.
func localizationOutlineAddsUnrankedDeclaration(outline *localizationFileOutline, targets []exploreTarget) bool {
	if outline == nil {
		return false
	}
	visible := make(map[string]struct{}, len(targets))
	for _, target := range targets {
		if target.node == nil || nodeDisplayPath(target.node) != outline.File {
			continue
		}
		key := target.node.ID
		if key == "" {
			key = strings.TrimSpace(target.node.QualName)
			if key == "" {
				key = strings.TrimSpace(target.node.Name)
			}
		}
		visible[key] = struct{}{}
	}
	for _, row := range outline.all {
		if _, covered := visible[row.key]; !covered {
			return true
		}
	}
	return false
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
			key:  key,
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
	wanted := localizationOutlineTerms(terms)
	matches := make([]match, 0, len(rows))
	for index, row := range rows {
		matched, longest := localizationOutlineRowTermMatchExpanded(row.Name, wanted)
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
	return localizationOutlineRowTermMatchExpanded(name, localizationOutlineTerms(terms))
}

func localizationOutlineRowTermMatchExpanded(name string, wanted map[string]struct{}) (matched, longest int) {
	if len(wanted) == 0 {
		return 0, 0
	}
	seen := make(map[string]struct{}, len(wanted))
	for _, raw := range rerank.Tokenize(name) {
		forms := localizationOutlineTermForms(strings.ToLower(strings.TrimSpace(raw)))
		matchedForm := ""
		for _, form := range forms {
			if _, ok := wanted[form]; ok {
				matchedForm = form
				break
			}
		}
		if matchedForm == "" {
			continue
		}
		if _, duplicate := seen[matchedForm]; duplicate {
			continue
		}
		seen[matchedForm] = struct{}{}
		matched++
		if len(matchedForm) > longest {
			longest = len(matchedForm)
		}
	}
	return matched, longest
}

func localizationOutlineTerms(terms map[string]struct{}) map[string]struct{} {
	forms := make(map[string]struct{}, len(terms)*2)
	for term := range terms {
		for _, form := range localizationOutlineTermForms(term) {
			forms[form] = struct{}{}
		}
	}
	return forms
}

// localizationOutlineTermForms is deliberately outline-local: conservative
// inflection alternatives improve declaration indexing without widening answer
// readiness or the global semantic ranker. The exact spelling always remains.
func localizationOutlineTermForms(term string) []string {
	term = strings.ToLower(strings.TrimSpace(term))
	if term == "" {
		return nil
	}
	forms := []string{term}
	for _, suffix := range []string{"ing", "ed", "er", "s"} {
		if !strings.HasSuffix(term, suffix) {
			continue
		}
		stem := strings.TrimSuffix(term, suffix)
		if len([]rune(stem)) >= 4 && stem != term {
			forms = append(forms, stem)
		}
		break
	}
	return forms
}

// elide re-projects the retained rows at rowCap. The declarations the task names
// are kept first; the file's head and tail then fill whatever the cap has left,
// so an index the caller cannot navigate by eye still shows both of its ends.
func (o *localizationFileOutline) elide(rowCap int) {
	if o == nil {
		return
	}
	if rowCap == localizationOutlineCompleteRows {
		o.Rows = o.all
		o.Elided = 0
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

// dropUnprotectedFloorFile gives back one rank-two-or-later outline after all
// of its depth has already yielded. Envelope packing uses this before stripping
// independently useful evidence detail; the protected leading pair is untouched.
func (p *localizationPageOutline) dropUnprotectedFloorFile() bool {
	if p == nil {
		return false
	}
	for index := len(p.Others) - 1; index >= 0; index-- {
		outline := p.Others[index]
		if outline == nil || p.otherRank(index) < localizationOutlineProtectedFileCount ||
			len(outline.Rows) > localizationOutlineFloorRows {
			continue
		}
		p.Others = append(p.Others[:index], p.Others[index+1:]...)
		return true
	}
	return false
}

// relieve gives back one increment of outline payload. Every rank-two-or-later
// file shrinks and then drops before either of the top two files changes. Under
// further pressure rank one yields before rank zero. This makes completeness a
// real preference while leaving the envelope's hard byte bound authoritative.
func (p *localizationPageOutline) relieve() {
	if p == nil {
		return
	}
	// Lower-ranked depth yields from the tail inward.
	for index := len(p.Others) - 1; index >= 0; index-- {
		outline := p.Others[index]
		if outline == nil || p.otherRank(index) < localizationOutlineProtectedFileCount ||
			len(outline.Rows) <= localizationOutlineFloorRows {
			continue
		}
		outline.elide(localizationOutlineNextRowCap(len(outline.Rows)))
		return
	}
	// Once all lower-ranked files reached their floor, give back their breadth
	// before touching the protected pair.
	for index := len(p.Others) - 1; index >= 0; index-- {
		outline := p.Others[index]
		if outline == nil || p.otherRank(index) < localizationOutlineProtectedFileCount {
			continue
		}
		p.Others = append(p.Others[:index], p.Others[index+1:]...)
		return
	}
	// The second-ranked file is the next expendable block.
	for index := len(p.Others) - 1; index >= 0; index-- {
		outline := p.Others[index]
		if outline == nil || p.otherRank(index) != 1 {
			continue
		}
		if len(outline.Rows) > localizationOutlineFloorRows {
			outline.elide(localizationOutlineNextRowCap(len(outline.Rows)))
		} else {
			p.Others = append(p.Others[:index], p.Others[index+1:]...)
		}
		return
	}
	if p.Leading != nil {
		if len(p.Leading.Rows) > localizationOutlineFloorRows {
			p.Leading.elide(localizationOutlineNextRowCap(len(p.Leading.Rows)))
		} else {
			p.Leading = nil
		}
	}
}

// otherRank preserves compatibility with directly-constructed outline blocks:
// an unset rank on an Others entry means its positional page rank.
func (p *localizationPageOutline) otherRank(index int) int {
	if p == nil || index < 0 || index >= len(p.Others) || p.Others[index] == nil {
		return index + 1
	}
	if rank := p.Others[index].rank; rank > 0 {
		return rank
	}
	return index + 1
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
	if p.Leading != nil && len(p.Leading.Rows) > localizationOutlineFloorRows {
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
