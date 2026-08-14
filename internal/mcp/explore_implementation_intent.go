package mcp

import (
	"context"
	"strings"
	"unicode"

	"github.com/zzet/gortex/internal/graph"
)

// Implementation-intent localization.
//
// A query asking where something is IMPLEMENTED must return the concrete
// code that changes, not only the interface that names it. Ranking cannot
// guarantee that: an interface declaration legitimately dominates the text
// and centrality signals for its own name, so the concrete members that a
// fix actually edits can fall below the envelope cut. When the query
// expresses implementation intent, strong interface/abstract seeds in the
// ranked head expand through bounded implements/overrides relations, and a
// result whose head is exclusively abstract declarations refuses to declare
// answer_ready.
//
// Every rule here is generic: intent detection is plain English vocabulary,
// expansion walks graph relations, and nothing names a repository, an
// issue, or benchmark vocabulary.

const (
	// exploreImplSeedScan bounds how deep into the ranked head seeds are
	// considered; expansion never reorders the head above this line.
	exploreImplSeedScan = 5
	// exploreImplPerSeed bounds implementors admitted per abstract seed.
	exploreImplPerSeed = 4
	// exploreImplFileReserve is the number of DISTINCT implementation files
	// the expansion reserves in the final candidate set.
	exploreImplFileReserve = 3

	exploreImplOwnerRelationLimit    = 8
	exploreImplImplementorLimit      = 16
	exploreImplMemberRefinementLimit = 256
)

var exploreImplementationIntentTerms = []string{
	"implementation", "implementations", "implement", "implements",
	"implemented", "implementing", "implementor", "implementors",
	"implementer", "implementers", "concrete", "override", "overrides",
	"overriding", "overridden", "subclass", "subclasses",
}

// exploreImplementationIntent reports whether the task text asks for the
// implementing/concrete side of an abstraction. Deliberately a plain word
// scan: the terminal-terms shaping drops or restems exactly the vocabulary
// this must detect.
func exploreImplementationIntent(task string) bool {
	for _, field := range strings.FieldsFunc(strings.ToLower(task), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		for _, intent := range exploreImplementationIntentTerms {
			if field == intent {
				return true
			}
		}
	}
	return false
}

// exploreBoundedAbstractSeed reports whether a ranked node is an abstract
// declaration an implementation-intent query must expand: an interface, or
// an interface member whose owner is resolved through a complete bounded
// member_of projection.
func exploreBoundedAbstractSeed(
	ctx context.Context,
	reader graph.Reader,
	declarationScope graph.LocalizationNodeScope,
	n *graph.Node,
) (owner *graph.Node, abstract, complete bool) {
	if n == nil {
		return nil, false, true
	}
	if n.Kind == graph.KindInterface {
		return n, true, true
	}
	if n.Kind != graph.KindMethod && n.Kind != graph.KindFunction {
		return nil, false, true
	}
	relations, complete := exploreBoundedEdgeIdentities(
		ctx, reader, []string{n.ID}, []graph.EdgeKind{graph.EdgeMemberOf},
		exploreImplOwnerRelationLimit, exploreBoundedOutgoing,
	)
	if !complete {
		return nil, false, false
	}
	edges := relations[n.ID]
	ownerIDs := make([]string, 0, len(edges))
	for _, edge := range edges {
		ownerIDs = append(ownerIDs, edge.To)
	}
	owners, complete := exploreNodesByIDsBounded(ctx, reader, ownerIDs, exploreImplOwnerRelationLimit)
	if !complete {
		return nil, false, false
	}
	for _, edge := range edges {
		owner := owners[edge.To]
		if owner == nil || owner.ID != edge.To {
			return nil, false, false
		}
		if !declarationScope.Allows(owner) {
			continue
		}
		if owner.Kind == graph.KindInterface {
			return owner, true, ctx.Err() == nil
		}
	}
	return nil, false, ctx.Err() == nil
}

// expandImplementationTargets inserts concrete implementors after abstract
// seeds in the ranked head. Interface seeds admit their implementing types;
// interface-member seeds admit the same-name members of those implementors.
// The abstract seed is never evicted, admitted implementors cover at most
// exploreImplFileReserve distinct new files, and every walk is one bounded
// relation hop — no transitive traversal.
func (s *Server) expandImplementationTargets(
	ctx context.Context,
	targets []exploreTarget,
	declarationScope graph.LocalizationNodeScope,
) []exploreTarget {
	eng := s.engineFor(ctx)
	if eng == nil || len(targets) == 0 {
		return targets
	}
	reader := eng.Reader()
	if reader == nil || ctx.Err() != nil {
		return targets
	}
	present := make(map[string]struct{}, len(targets))
	presentFiles := make(map[string]struct{}, len(targets))
	for _, target := range targets {
		if target.node != nil {
			present[target.node.ID] = struct{}{}
			presentFiles[target.node.FilePath] = struct{}{}
		}
	}

	newFiles := make(map[string]struct{}, exploreImplFileReserve)
	admit := func(node *graph.Node) bool {
		if node == nil || node.ID == "" || !declarationScope.Allows(node) || ctx.Err() != nil {
			return false
		}
		if _, duplicate := present[node.ID]; duplicate {
			return false
		}
		_, presentFile := presentFiles[node.FilePath]
		_, admittedFile := newFiles[node.FilePath]
		if !presentFile && !admittedFile {
			if len(newFiles) >= exploreImplFileReserve || ctx.Err() != nil {
				return false
			}
			newFiles[node.FilePath] = struct{}{}
		}
		if ctx.Err() != nil {
			return false
		}
		present[node.ID] = struct{}{}
		return true
	}

	implementorsOf := func(ownerID string) ([]*graph.Node, bool) {
		relations, complete := exploreBoundedEdgeIdentities(
			ctx, reader, []string{ownerID},
			[]graph.EdgeKind{graph.EdgeImplements, graph.EdgeExtends},
			exploreImplImplementorLimit, exploreBoundedIncoming,
		)
		if !complete {
			return nil, false
		}
		edges := relations[ownerID]
		implementorIDs := make([]string, 0, len(edges))
		for _, edge := range edges {
			implementorIDs = append(implementorIDs, edge.From)
		}
		nodes, complete := exploreNodesByIDsBounded(ctx, reader, implementorIDs, exploreImplImplementorLimit)
		if !complete {
			return nil, false
		}
		out := make([]*graph.Node, 0, min(exploreImplPerSeed, len(edges)))
		for _, edge := range edges {
			implementation := nodes[edge.From]
			if implementation == nil || implementation.ID != edge.From {
				return nil, false
			}
			if !declarationScope.Allows(implementation) {
				continue
			}
			if implementation.Kind == graph.KindInterface {
				continue
			}
			out = append(out, implementation)
			if len(out) == exploreImplPerSeed {
				break
			}
		}
		return out, ctx.Err() == nil
	}
	memberOf := func(typeID, name string) *graph.Node {
		relations, complete := exploreBoundedEdgeIdentities(
			ctx, reader, []string{typeID}, []graph.EdgeKind{graph.EdgeMemberOf},
			exploreImplMemberRefinementLimit, exploreBoundedIncoming,
		)
		if !complete {
			return nil
		}
		edges := relations[typeID]
		memberIDs := make([]string, 0, len(edges))
		for _, edge := range edges {
			memberIDs = append(memberIDs, edge.From)
		}
		nodes, complete := exploreNodesByIDsBounded(ctx, reader, memberIDs, exploreImplMemberRefinementLimit)
		if !complete {
			return nil
		}
		for _, edge := range edges {
			member := nodes[edge.From]
			if member == nil || member.ID != edge.From {
				return nil
			}
			if !declarationScope.Allows(member) {
				continue
			}
			if member.Name == name {
				return member
			}
		}
		return nil
	}

	expanded := make([]exploreTarget, 0, len(targets)+exploreImplPerSeed)
	for index, target := range targets {
		expanded = append(expanded, target)
		if index >= exploreImplSeedScan || target.node == nil {
			continue
		}
		owner, abstract, complete := exploreBoundedAbstractSeed(ctx, reader, declarationScope, target.node)
		if !complete {
			return targets
		}
		if !abstract {
			continue
		}
		implementations, complete := implementorsOf(owner.ID)
		if !complete {
			return targets
		}
		memberName := ""
		if owner.ID != target.node.ID {
			memberName = target.node.Name
		}
		for _, implementation := range implementations {
			concrete := implementation
			if memberName != "" {
				if member := memberOf(implementation.ID, memberName); member != nil {
					concrete = member
				}
				if ctx.Err() != nil {
					return targets
				}
			}
			if !admit(concrete) {
				continue
			}
			expanded = append(expanded, exploreTarget{
				node:  concrete,
				score: target.score * 0.98,
			})
		}
	}
	if ctx.Err() != nil {
		return targets
	}
	return expanded
}

// exploreImplementationAnswerBlocked refuses answer_ready when an
// implementation-intent query's visible head holds only abstract
// declarations: the concrete code the query asks for is not in evidence.
func exploreImplementationAnswerBlocked(
	ctx context.Context,
	task string,
	targets []exploreTarget,
	reader graph.Reader,
	declarationScope graph.LocalizationNodeScope,
) bool {
	if !exploreImplementationIntent(task) {
		return false
	}
	if reader == nil || ctx.Err() != nil {
		return len(targets) > 0
	}
	scanned := 0
	for _, target := range targets {
		if target.node == nil {
			continue
		}
		if scanned++; scanned > exploreImplSeedScan {
			break
		}
		_, abstract, complete := exploreBoundedAbstractSeed(
			ctx, reader, declarationScope, target.node,
		)
		if !complete || ctx.Err() != nil {
			return true
		}
		if !abstract {
			return false
		}
	}
	if ctx.Err() != nil {
		return scanned > 0
	}
	return scanned > 0
}
