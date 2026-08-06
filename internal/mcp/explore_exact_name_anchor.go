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
// index. Cost is fixed by construction: at most exploreExactNameAnchorMaxTokens
// name lookups per task, each one sharded-map hit, with no per-candidate store
// query and no source hydration.
func (s *Server) exploreExactNameAnchors(
	ctx context.Context,
	task string,
	shaped []exploreSyntacticAnchor,
	ordinary []*rerank.Candidate,
	scope query.QueryOptions,
	slots int,
) []exploreSyntacticAnchor {
	if s == nil || s.graph == nil || slots <= 0 || ctx.Err() != nil {
		return nil
	}
	tokens := exploreExactNameAnchorTokens(task)
	if len(tokens) == 0 {
		return nil
	}
	pooledFiles := exploreRankedPoolFiles(ordinary)
	out := make([]exploreSyntacticAnchor, 0, slots)
	for _, token := range tokens {
		if ctx.Err() != nil {
			break
		}
		nodes := s.exploreExactNameAnchorNodes(ctx, token, scope, pooledFiles)
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

// exploreExactNameAnchorNodes returns the localizable declarations whose name is
// the task token verbatim. Case matters: `Mount` and `mount` are different
// symbols, and a case-insensitive match would readmit exactly the prose noise
// this lane exists to exclude.
func (s *Server) exploreExactNameAnchorNodes(
	ctx context.Context,
	token string,
	scope query.QueryOptions,
	pooledFiles map[string]struct{},
) []*graph.Node {
	matches := s.graph.FindNodesByName(token)
	if len(matches) == 0 {
		return nil
	}
	shared := 0
	eligible := make([]*graph.Node, 0, exploreExactNameAnchorMaxNodes)
	pooled := make([]*graph.Node, 0, exploreExactNameAnchorMaxNodes)
	for _, node := range matches {
		if node == nil || node.Name != token {
			continue
		}
		shared++
		if !exploreSyntacticAnchorEligibleNode(node) || !scope.ScopeAllows(node) ||
			!s.nodeInSessionScope(ctx, node) {
			continue
		}
		if len(eligible) < exploreExactNameAnchorMaxNodes {
			eligible = append(eligible, node)
		}
		if _, ranked := pooledFiles[node.FilePath]; ranked && len(pooled) < exploreExactNameAnchorMaxNodes {
			pooled = append(pooled, node)
		}
	}
	if shared > exploreExactNameAnchorMaxShared {
		// The ranked pool is independent evidence that one of the homonyms is the
		// one the task means; without it an ambiguous name stays unanchored.
		return pooled
	}
	return eligible
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
// worth one name lookup each. Code-shaped tokens are excluded on purpose: they
// already own the syntactic lane, and re-admitting them here would spend the
// budget re-deriving anchors that exist.
func exploreExactNameAnchorTokens(task string) []string {
	seen := make(map[string]struct{}, exploreExactNameAnchorMaxTokens)
	out := make([]string, 0, exploreExactNameAnchorMaxTokens)
	for _, raw := range exploreUnquotedCodeTokens(task) {
		token := strings.Trim(raw, "-_.:()[]{}<>,;'\"")
		if !exploreExactNameAnchorToken(token) {
			continue
		}
		if _, duplicate := seen[token]; duplicate {
			continue
		}
		seen[token] = struct{}{}
		out = append(out, token)
		if len(out) == exploreExactNameAnchorMaxTokens {
			break
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
	return s.exploreExactQualifiedAnchorCandidate(ctx, anchor, scope, usedIDs, usedFiles)
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
		if scanned == exploreExactNameAnchorOwnerScan {
			break
		}
		scanned++
		if node == nil || node.Name != owner ||
			(node.Kind != graph.KindType && node.Kind != graph.KindInterface) {
			continue
		}
		if !scope.ScopeAllows(node) || !s.nodeInSessionScope(ctx, node) {
			continue
		}
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
