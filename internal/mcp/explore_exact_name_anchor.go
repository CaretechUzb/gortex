package mcp

import (
	"context"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search/rerank"
)

const (
	exploreExactNameAnchorMaxTokens = 16
	exploreExactNameAnchorMinChars  = 4
	exploreExactNameAnchorMaxChars  = 64
	// A name this widely shared describes a convention (handle, execute, run),
	// not the task's subject, so it cannot anchor on its own.
	exploreExactNameAnchorMaxShared = 8
	exploreExactNameAnchorMaxNodes  = 10
	exploreExactNameAnchorOwnerScan = 32
	// Case folding is a miss-only recovery lane. Four tokens, three alternate
	// indexed spellings each, and four ranked files are hard request-wide caps.
	exploreExactNameAnchorCaseFoldMaxTokens = 4
	exploreExactNameAnchorCaseVariantMax    = 4
	exploreExactNameAnchorCaseFoldMaxFiles  = 4
)

// exploreTaskAnchors is the anchor lane the retrieval side uses: the
// task-shaped anchors first, then plain prose tokens that name an indexed
// symbol exactly. Spelling alone cannot recognise an all-lowercase identifier
// (vprintf, mount), so the graph decides — but only for the slots the
// code-shaped lane left unclaimed.
func (s *Server) exploreTaskAnchors(
	ctx context.Context,
	task string,
	ordinary []*rerank.Candidate,
	scope query.QueryOptions,
) []exploreSyntacticAnchor {
	anchors := exploreSyntacticAnchors(task)
	slots := exploreSyntacticAnchorMaxTerms - len(anchors)
	if slots <= 0 {
		return anchors
	}
	return append(anchors, s.exploreExactNameAnchors(ctx, task, anchors, ordinary, scope, slots)...)
}

// exploreExactNameAnchors resolves plain task tokens against the graph name
// index. Cost is fixed by construction: sixteen exact lookups plus at most
// three spelling variants for four misses and four ranked-file reads. The
// recovery lane never scans the corpus or hydrates source.
func (s *Server) exploreExactNameAnchors(
	ctx context.Context,
	task string,
	shaped []exploreSyntacticAnchor,
	ordinary []*rerank.Candidate,
	scope query.QueryOptions,
	slots int,
) []exploreSyntacticAnchor {
	if s == nil || slots <= 0 || ctx.Err() != nil {
		return nil
	}
	reader := s.readerFor(ctx)
	if reader == nil {
		return nil
	}
	tokens := exploreExactNameAnchorTokens(task)
	if len(tokens) == 0 {
		return nil
	}
	lookup := newExploreExactNameAnchorLookup(reader, ordinary)
	out := make([]exploreSyntacticAnchor, 0, slots)
	for _, token := range tokens {
		if ctx.Err() != nil {
			break
		}
		nodes := s.exploreExactNameAnchorNodes(ctx, token, scope, lookup)
		if len(nodes) == 0 {
			continue
		}
		anchor, ok := newExploreSyntacticAnchor(token)
		if !ok || exploreExactNameAnchorDuplicate(anchor, shaped, out) {
			continue
		}
		anchor.exactNodes = nodes
		out = append(out, anchor)
		if len(out) == slots {
			break
		}
	}
	return out
}

func exploreExactNameAnchorDuplicate(anchor exploreSyntacticAnchor, groups ...[]exploreSyntacticAnchor) bool {
	for _, group := range groups {
		for _, existing := range group {
			if exploreSyntacticAnchorEquivalent(existing, anchor) {
				return true
			}
		}
	}
	return false
}

type exploreExactNameAnchorLookup struct {
	reader         graph.Reader
	rankedNodes    []*graph.Node
	rankedFiles    []string
	pooledFiles    map[string]struct{}
	fallbackTokens int
	fileNodes      map[string][]*graph.Node
}

func newExploreExactNameAnchorLookup(
	reader graph.Reader,
	ordinary []*rerank.Candidate,
) *exploreExactNameAnchorLookup {
	lookup := &exploreExactNameAnchorLookup{
		reader:      reader,
		pooledFiles: exploreRankedPoolFiles(ordinary),
		fileNodes:   make(map[string][]*graph.Node, exploreExactNameAnchorCaseFoldMaxFiles),
	}
	seenFiles := make(map[string]struct{}, exploreExactNameAnchorCaseFoldMaxFiles)
	for _, candidate := range ordinary {
		if candidate == nil || candidate.Node == nil {
			continue
		}
		lookup.rankedNodes = append(lookup.rankedNodes, candidate.Node)
		path := candidate.Node.FilePath
		if path == "" || len(lookup.rankedFiles) == exploreExactNameAnchorCaseFoldMaxFiles {
			continue
		}
		if _, duplicate := seenFiles[path]; duplicate {
			continue
		}
		seenFiles[path] = struct{}{}
		lookup.rankedFiles = append(lookup.rankedFiles, path)
	}
	return lookup
}

// exploreExactNameCaseVariants returns a small deterministic set of spellings
// that exact name indexes commonly contain. It never attempts the combinatorial
// interior-case search needed to invent arbitrary camelCase.
func exploreExactNameCaseVariants(token string) []string {
	if token == "" {
		return nil
	}
	variants := make([]string, 0, exploreExactNameAnchorCaseVariantMax)
	seen := make(map[string]struct{}, exploreExactNameAnchorCaseVariantMax)
	add := func(value string) {
		if value == "" || len(variants) == exploreExactNameAnchorCaseVariantMax {
			return
		}
		if _, exists := seen[value]; exists {
			return
		}
		seen[value] = struct{}{}
		variants = append(variants, value)
	}
	add(token)
	runes := []rune(token)
	if len(runes) == 0 {
		return variants
	}
	lowerFirst := append([]rune(nil), runes...)
	lowerFirst[0] = unicode.ToLower(lowerFirst[0])
	add(string(lowerFirst))
	upperFirst := append([]rune(nil), runes...)
	upperFirst[0] = unicode.ToUpper(upperFirst[0])
	add(string(upperFirst))

	lower := strings.ToLower(token)
	upper := strings.ToUpper(token)
	if token == lower || token == upper || token == string(upperFirst) {
		add(lower)
		title := []rune(lower)
		if len(title) > 0 {
			title[0] = unicode.ToUpper(title[0])
			add(string(title))
		}
		add(upper)
	}
	return variants
}

// caseFoldedMatches performs bounded miss recovery. Exact indexed spelling
// variants come first. Arbitrary interior-case matches are accepted only when
// the independent ordinary ranking already selected the node or its file.
func (lookup *exploreExactNameAnchorLookup) caseFoldedMatches(
	ctx context.Context,
	token string,
	eligible func(*graph.Node) bool,
) []*graph.Node {
	if lookup == nil || lookup.reader == nil || ctx.Err() != nil ||
		lookup.fallbackTokens == exploreExactNameAnchorCaseFoldMaxTokens {
		return nil
	}
	lookup.fallbackTokens++
	matches := make([]*graph.Node, 0, exploreExactNameAnchorMaxNodes)
	seen := make(map[string]struct{}, exploreExactNameAnchorMaxNodes)
	add := func(node *graph.Node) {
		if node == nil || node.ID == "" || len(matches) == exploreExactNameAnchorMaxNodes ||
			!strings.EqualFold(node.Name, token) || eligible == nil || !eligible(node) {
			return
		}
		if _, duplicate := seen[node.ID]; duplicate {
			return
		}
		seen[node.ID] = struct{}{}
		matches = append(matches, node)
	}

	variants := exploreExactNameCaseVariants(token)
	for _, variant := range variants[1:] {
		if ctx.Err() != nil {
			return matches
		}
		for _, node := range lookup.reader.FindNodesByName(variant) {
			add(node)
		}
	}
	if len(matches) > 0 {
		return matches
	}

	for _, node := range lookup.rankedNodes {
		add(node)
	}
	if len(matches) > 0 {
		return matches
	}

	for _, path := range lookup.rankedFiles {
		if ctx.Err() != nil {
			break
		}
		nodes, cached := lookup.fileNodes[path]
		if !cached {
			nodes = lookup.reader.GetFileNodes(path)
			lookup.fileNodes[path] = nodes
		}
		for _, node := range nodes {
			add(node)
		}
		if len(matches) > 0 {
			break
		}
	}
	return matches
}

// exploreExactNameAnchorNodes returns localizable declarations whose indexed
// name matches the task token. An exact case bucket is authoritative only when
// it contains a declaration eligible for this request's repo/session scope;
// foreign homonyms cannot suppress bounded case-fold recovery.
func (s *Server) exploreExactNameAnchorNodes(
	ctx context.Context,
	token string,
	scope query.QueryOptions,
	lookup *exploreExactNameAnchorLookup,
) []*graph.Node {
	eligibleNode := func(node *graph.Node) bool {
		return node != nil && exploreSyntacticAnchorEligibleNode(node) &&
			scope.ScopeAllows(node) && s.nodeInSessionScope(ctx, node)
	}
	selectMatches := func(matches []*graph.Node, exact bool) ([]*graph.Node, bool) {
		shared := 0
		eligible := make([]*graph.Node, 0, exploreExactNameAnchorMaxNodes)
		pooled := make([]*graph.Node, 0, exploreExactNameAnchorMaxNodes)
		for _, node := range matches {
			if node == nil || (exact && node.Name != token) || (!exact && !strings.EqualFold(node.Name, token)) ||
				!eligibleNode(node) {
				continue
			}
			shared++
			if len(eligible) < exploreExactNameAnchorMaxNodes {
				eligible = append(eligible, node)
			}
			if _, ranked := lookup.pooledFiles[node.FilePath]; ranked && len(pooled) < exploreExactNameAnchorMaxNodes {
				pooled = append(pooled, node)
			}
		}
		if shared > exploreExactNameAnchorMaxShared {
			// The ranked pool is independent evidence that one of the homonyms is
			// the one the task means; without it an ambiguous name stays unanchored.
			return pooled, true
		}
		return eligible, shared > 0
	}

	if exact, found := selectMatches(lookup.reader.FindNodesByName(token), true); found {
		return exact
	}
	matches := lookup.caseFoldedMatches(ctx, token, eligibleNode)
	selected, _ := selectMatches(matches, false)
	return selected
}

func exploreRankedPoolFiles(candidates []*rerank.Candidate) map[string]struct{} {
	files := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		if candidate == nil || candidate.Node == nil || candidate.Node.FilePath == "" {
			continue
		}
		files[candidate.Node.FilePath] = struct{}{}
	}
	return files
}

// exploreExactNameAnchorTokens returns the bounded, deduplicated prose tokens
// worth one name lookup each, then the tokens mined from code blocks. Tokens
// the author wrote as prose rank first and the shared budget is unchanged, so
// mining can only spend the lookups prose left over. Code-shaped tokens are
// excluded on purpose: they already own the syntactic lane, and re-admitting
// them here would spend the budget re-deriving anchors that exist.
func exploreExactNameAnchorTokens(task string) []string {
	seen := make(map[string]struct{}, exploreExactNameAnchorMaxTokens)
	out := make([]string, 0, exploreExactNameAnchorMaxTokens)
	for _, lane := range [2][]string{exploreUnquotedCodeTokens(task), exploreFencedCodeTokens(task)} {
		for _, raw := range lane {
			token := strings.Trim(raw, "-_.:()[]{}<>,;'\"")
			if !exploreExactNameAnchorToken(token) {
				continue
			}
			key := strings.ToLower(token)
			if _, duplicate := seen[key]; duplicate {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, token)
			if len(out) == exploreExactNameAnchorMaxTokens {
				return out
			}
		}
	}
	return out
}

func exploreExactNameAnchorToken(token string) bool {
	if len(token) < exploreExactNameAnchorMinChars || len(token) > exploreExactNameAnchorMaxChars {
		return false
	}
	first, _ := utf8.DecodeRuneInString(token)
	if !unicode.IsLetter(first) {
		return false
	}
	for _, r := range token {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			return false
		}
	}
	if exploreCodeShapedToken(token) {
		return false
	}
	lower := strings.ToLower(token)
	if _, stop := assistStopWords[lower]; stop {
		return false
	}
	if _, generic := exploreTerminalGenericTerms[exploreTerminalTermRoot(lower)]; generic {
		return false
	}
	return !exploreSyntacticAnchorNoise(lower)
}

// exploreDottedQualifiedMention recognises the dynamic-language spelling of a
// qualified member so Owner.member resolves through the same exact graph lookup
// as Owner::member. The owner segment must be type-shaped and the mention must
// not be a file: tree.go and package.json name files, not members.
func exploreDottedQualifiedMention(raw string) (string, bool) {
	if strings.Contains(raw, "::") || strings.ContainsAny(raw, "/\\ \t") {
		return "", false
	}
	dot := strings.LastIndexByte(raw, '.')
	if dot <= 0 || dot == len(raw)-1 {
		return "", false
	}
	if exploreSourceExtension(raw[dot:]) || exploreArtifactFile(raw) {
		return "", false
	}
	owner, member := raw[:dot], raw[dot+1:]
	if segment := strings.LastIndexByte(owner, '.'); segment >= 0 {
		owner = owner[segment+1:]
	}
	if !exploreQualifiedOwnerSegment(owner) || !exploreQualifiedMemberSegment(member) {
		return "", false
	}
	return owner + "." + member, true
}

func exploreQualifiedOwnerSegment(owner string) bool {
	first, _ := utf8.DecodeRuneInString(owner)
	return len(owner) >= 3 && unicode.IsUpper(first) && exploreIdentifierSegment(owner)
}

func exploreQualifiedMemberSegment(member string) bool {
	first, _ := utf8.DecodeRuneInString(member)
	// A capitalised tail is a nested namespace (Monolog.Handler.HipChatHandler),
	// not a member of the segment before it.
	return len(member) >= 3 && (unicode.IsLower(first) || first == '_') && exploreIdentifierSegment(member)
}

func exploreIdentifierSegment(segment string) bool {
	for _, r := range segment {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' {
			return false
		}
	}
	return segment != ""
}

// exploreExactAnchorCandidate resolves an anchor straight from the graph when
// the task spelled a name the index already carries — the exact-name nodes
// discovered during anchor selection, or the parser's member name for a
// qualified mention. Both routes still pass the ordinary scope, kind and
// diversity filters.
func (s *Server) exploreExactAnchorCandidate(
	ctx context.Context,
	anchor exploreSyntacticAnchor,
	ordinary []*rerank.Candidate,
	scope query.QueryOptions,
	usedIDs, usedFiles map[string]struct{},
) *rerank.Candidate {
	if len(anchor.exactNodes) > 0 {
		candidates := make([]*rerank.Candidate, 0, len(anchor.exactNodes))
		for _, node := range anchor.exactNodes {
			candidates = append(candidates, &rerank.Candidate{Node: node, VectorRank: -1})
		}
		if got := exploreSyntacticAnchorCandidate(anchor, candidates, scope, usedIDs, usedFiles); got != nil {
			return got
		}
	}
	return s.exploreExactQualifiedAnchorCandidate(ctx, anchor, ordinary, scope, usedIDs, usedFiles)
}

// exploreQualifiedAnchorOwnerCandidate resolves the declaring type named by a
// qualified mention that already resolved to its member. One bounded name-index
// lookup per qualified anchor (at most exploreSyntacticAnchorMaxTerms per task);
// the owner declared beside the member wins so a homonym elsewhere in the tree
// cannot displace it.
func (s *Server) exploreQualifiedAnchorOwnerCandidate(
	ctx context.Context,
	anchor exploreSyntacticAnchor,
	member *graph.Node,
	scope query.QueryOptions,
	usedIDs map[string]struct{},
) *rerank.Candidate {
	if s == nil || s.graph == nil || member == nil || anchor.qualifiedName == "" || ctx.Err() != nil {
		return nil
	}
	dot := strings.LastIndexByte(anchor.qualifiedName, '.')
	if dot <= 0 {
		return nil
	}
	owner := anchor.qualifiedName[:dot]
	if segment := strings.LastIndexByte(owner, '.'); segment >= 0 {
		owner = owner[segment+1:]
	}
	if owner == "" || owner == member.Name {
		return nil
	}
	var best *graph.Node
	scanned := 0
	for _, node := range s.graph.FindNodesByName(owner) {
		if node == nil || node.Name != owner ||
			(node.Kind != graph.KindType && node.Kind != graph.KindInterface) {
			continue
		}
		if !scope.ScopeAllows(node) || !s.nodeInSessionScope(ctx, node) {
			continue
		}
		if scanned == exploreExactNameAnchorOwnerScan {
			break
		}
		scanned++
		if _, used := usedIDs[node.ID]; used {
			continue
		}
		if best == nil || (node.FilePath == member.FilePath && best.FilePath != member.FilePath) {
			best = node
		}
	}
	if best == nil {
		return nil
	}
	return &rerank.Candidate{Node: best, VectorRank: -1}
}
