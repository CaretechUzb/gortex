package mcp

import (
	"context"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search/rerank"
)

func exploreQualifiedAnchorParts(qualifiedName string) (owner, member string, ok bool) {
	dot := strings.LastIndexByte(qualifiedName, '.')
	if dot <= 0 || dot == len(qualifiedName)-1 {
		return "", "", false
	}
	owner = exploreQualifiedIdentifierLeaf(qualifiedName[:dot])
	member = exploreQualifiedIdentifierLeaf(qualifiedName[dot+1:])
	return owner, member, owner != "" && member != ""
}

func exploreQualifiedLeafMatchesNode(node *graph.Node, leaf string) bool {
	if node == nil || leaf == "" {
		return false
	}
	for _, identifier := range []string{node.Name, node.QualName, node.ID} {
		if strings.EqualFold(exploreQualifiedIdentifierLeaf(identifier), leaf) {
			return true
		}
	}
	return false
}

// exploreRankedQualifiedOwnerFiles intersects exact owner declarations with
// the ordinary result order. Thus a common owner name cannot authorize a new
// file: independent retrieval must already have ranked that file.
func (s *Server) exploreRankedQualifiedOwnerFiles(
	ctx context.Context,
	reader graph.Reader,
	owner string,
	ordinary []*rerank.Candidate,
	scope query.QueryOptions,
) []string {
	ownerFiles := make(map[string]struct{})
	scanned := 0
	for _, node := range reader.FindNodesByName(owner) {
		if scanned == exploreExactNameAnchorOwnerScan {
			break
		}
		scanned++
		if node == nil || node.Name != owner || node.FilePath == "" ||
			(node.Kind != graph.KindType && node.Kind != graph.KindInterface) ||
			!scope.ScopeAllows(node) || !s.nodeInSessionScope(ctx, node) {
			continue
		}
		ownerFiles[node.FilePath] = struct{}{}
	}
	if len(ownerFiles) == 0 {
		return nil
	}
	files := make([]string, 0, exploreQualifiedLeafMaxFiles)
	seen := make(map[string]struct{}, exploreQualifiedLeafMaxFiles)
	for _, candidate := range ordinary {
		if candidate == nil || candidate.Node == nil {
			continue
		}
		path := candidate.Node.FilePath
		if _, ownerFile := ownerFiles[path]; !ownerFile {
			continue
		}
		if _, duplicate := seen[path]; duplicate {
			continue
		}
		seen[path] = struct{}{}
		files = append(files, path)
		if len(files) == exploreQualifiedLeafMaxFiles {
			break
		}
	}
	return files
}

func (s *Server) exploreQualifiedLeafCandidate(
	ctx context.Context,
	reader graph.Reader,
	anchor exploreSyntacticAnchor,
	ordinary []*rerank.Candidate,
	scope query.QueryOptions,
	usedIDs, usedFiles map[string]struct{},
) *rerank.Candidate {
	owner, member, ok := exploreQualifiedAnchorParts(anchor.qualifiedName)
	if !ok || ctx.Err() != nil {
		return nil
	}
	files := s.exploreRankedQualifiedOwnerFiles(ctx, reader, owner, ordinary, scope)
	if len(files) == 0 {
		return nil
	}
	var fallback *rerank.Candidate
	seen := make(map[string]struct{}, exploreSyntacticAnchorFetch)
	matched := 0
	for _, path := range files {
		if ctx.Err() != nil {
			break
		}
		for _, node := range reader.GetFileNodes(path) {
			if node == nil || !exploreQualifiedLeafMatchesNode(node, member) ||
				!exploreSyntacticAnchorEligibleNode(node) || !scope.ScopeAllows(node) ||
				!s.nodeInSessionScope(ctx, node) {
				continue
			}
			if _, duplicate := seen[node.ID]; duplicate {
				continue
			}
			seen[node.ID] = struct{}{}
			if _, used := usedIDs[node.ID]; used {
				continue
			}
			candidate := &rerank.Candidate{Node: node, VectorRank: -1}
			if fallback == nil {
				fallback = candidate
			}
			if _, repeatedFile := usedFiles[node.FilePath]; !repeatedFile {
				return candidate
			}
			matched++
			if matched == exploreSyntacticAnchorFetch {
				return fallback
			}
		}
	}
	return fallback
}
