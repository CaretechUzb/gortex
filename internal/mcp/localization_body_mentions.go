package mcp

import (
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

const (
	localizationBodyMentionCap        = 8
	localizationProvenanceBodyMention = "body_mention"
)

type localizationBodyMentionCandidate struct {
	node       *graph.Node
	sameFile   bool
	overlap    int
	longest    int
	direct     bool
	ownerRank  int
	ownerIndex int
}

// promoteLocalizationBodyMentions converts declarations already visible inside
// packed source into citeable SUPPORTING rows. Every identity comes from the
// request-local graph declaration cache; source text alone can never fabricate
// a row. Tentative rows are admitted only when both envelope and retained digest
// budgets can carry them.
func promoteLocalizationBodyMentions(
	task string,
	envelope localizationExploreEnvelope,
	cache *localizationFileDeclarationCache,
	maxBytes int,
	digest *localizationEvidenceDigest,
) (localizationExploreEnvelope, *localizationEvidenceDigest) {
	if cache == nil || cache.reader == nil || len(envelope.Evidence) == 0 {
		return envelope, digest
	}
	files := make([]string, 0, len(envelope.Files))
	seenFiles := make(map[string]struct{}, len(envelope.Files))
	seenIDs := make(map[string]struct{}, len(envelope.Evidence))
	for _, row := range envelope.Evidence {
		if row.ID != "" {
			seenIDs[row.ID] = struct{}{}
		}
		file := strings.TrimSpace(row.File)
		if file == "" {
			continue
		}
		if _, seen := seenFiles[file]; seen {
			continue
		}
		seenFiles[file] = struct{}{}
		files = append(files, file)
	}
	declarations := make(map[string][]*graph.Node, len(files))
	for _, file := range files {
		declarations[file] = cache.definitions(file)
	}

	taskTerms := exploreTerminalTerms(task)
	candidates := make(map[string]localizationBodyMentionCandidate)
	for ownerIndex, owner := range envelope.Evidence {
		if strings.TrimSpace(owner.Source) == "" {
			continue
		}
		lowerSource := strings.ToLower(owner.Source)
		for _, file := range files {
			for _, node := range declarations[file] {
				if !localizationBodyMentionNodeEligible(node) {
					continue
				}
				if _, visible := seenIDs[node.ID]; visible {
					continue
				}
				if !exploreLowerTextHasExactLiteral(lowerSource, node.Name) {
					continue
				}
				overlap, longest := exploreDraftTermOverlap(taskTerms, node)
				candidate := localizationBodyMentionCandidate{
					node:       node,
					sameFile:   nodeDisplayPath(node) == owner.File,
					overlap:    overlap,
					longest:    longest,
					direct:     localizationBodyMentionDirect(owner, node.ID),
					ownerRank:  owner.Rank,
					ownerIndex: ownerIndex,
				}
				if previous, exists := candidates[node.ID]; !exists ||
					localizationBodyMentionLess(candidate, previous) {
					candidates[node.ID] = candidate
				}
			}
		}
	}
	ordered := make([]localizationBodyMentionCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		ordered = append(ordered, candidate)
	}
	sort.SliceStable(ordered, func(first, second int) bool {
		return localizationBodyMentionLess(ordered[first], ordered[second])
	})

	admitted := 0
	for _, mention := range ordered {
		if admitted == localizationBodyMentionCap {
			break
		}
		node := mention.node
		row := localizationEvidence{
			Rank:       len(envelope.Evidence) + 1,
			ID:         node.ID,
			Name:       compactLocalizationField(node.Name, localizationMaxNameRunes),
			Kind:       string(node.Kind),
			File:       nodeDisplayPath(node),
			Line:       node.StartLine,
			Provenance: localizationProvenanceBodyMention,
		}
		candidate := envelope
		candidate.Files = append(append([]string(nil), envelope.Files...), row.File)
		candidate.Symbols = append(append([]string(nil), envelope.Symbols...), row.ID)
		candidate.Evidence = append(append([]localizationEvidence(nil), envelope.Evidence...), row)

		candidateDigest := newLocalizationEvidenceDigestForTask(task, candidate)
		if !localizationBodyMentionDigestContains(candidateDigest, row.ID) {
			continue
		}
		contract := localizationContractReconciledWithDigest(candidate.Completion, candidateDigest)
		candidate.Completion = contract.Completion
		candidate.Terminal = contract.Terminal
		if !localizationEnvelopeFits(candidate, maxBytes) {
			continue
		}
		envelope, digest = candidate, candidateDigest
		seenIDs[row.ID] = struct{}{}
		admitted++
	}
	return envelope, digest
}

func localizationBodyMentionNodeEligible(node *graph.Node) bool {
	return node != nil && node.ID != "" && strings.TrimSpace(node.Name) != "" &&
		nodeDisplayPath(node) != "" && node.StartLine > 0 &&
		exploreLocalizableKind(node.Kind) && !isNonDefinitionNode(node.Kind)
}

func localizationBodyMentionDirect(owner localizationEvidence, id string) bool {
	for _, neighbor := range owner.Callers {
		if neighbor == id {
			return true
		}
	}
	for _, neighbor := range owner.Callees {
		if neighbor == id {
			return true
		}
	}
	return false
}

func localizationBodyMentionLess(left, right localizationBodyMentionCandidate) bool {
	if left.sameFile != right.sameFile {
		return left.sameFile
	}
	if left.overlap != right.overlap {
		return left.overlap > right.overlap
	}
	if left.longest != right.longest {
		return left.longest > right.longest
	}
	if left.direct != right.direct {
		return left.direct
	}
	if left.ownerRank != right.ownerRank {
		return left.ownerRank < right.ownerRank
	}
	if left.ownerIndex != right.ownerIndex {
		return left.ownerIndex < right.ownerIndex
	}
	if left.node.StartLine != right.node.StartLine {
		return left.node.StartLine < right.node.StartLine
	}
	return left.node.ID < right.node.ID
}

func localizationBodyMentionDigestContains(digest *localizationEvidenceDigest, id string) bool {
	if digest == nil || id == "" {
		return false
	}
	for _, row := range digest.Evidence {
		if row.ID == id && row.Provenance == localizationProvenanceBodyMention {
			return true
		}
	}
	return false
}
