package mcp

import (
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/zzet/gortex/internal/graph"
)

const (
	localizationBodyMentionCap                = 8
	localizationBodyMentionSourceCap          = 12
	localizationBodyMentionFileCap            = 10
	localizationBodyMentionDeclarationCap     = 256
	localizationBodyMentionCheckCap           = 2048
	localizationBodyMentionTokenCap           = 2048
	localizationBodyMentionTokenByteCap       = 256
	localizationBodyMentionTotalSourceByteCap = 64 << 10
	localizationProvenanceBodyMention         = "body_mention"
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

type localizationBodyMentionSource struct {
	owner      localizationEvidence
	ownerIndex int
	file       string
	tokens     map[string]struct{}
}

// promoteLocalizationBodyMentions converts declarations already visible inside
// packed source into citeable SUPPORTING rows. Every identity comes from the
// request-local graph declaration cache; source text alone can never fabricate
// a row. Source bytes, files, declarations, comparisons, candidates, and output
// are all independently capped. A truncated scan remains supporting-only and
// therefore cannot establish or extend terminal authority.
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
	seenIDs := make(map[string]struct{}, len(envelope.Evidence))
	for _, row := range envelope.Evidence {
		if row.ID != "" {
			seenIDs[row.ID] = struct{}{}
		}
	}
	sources := localizationBodyMentionSources(envelope)
	if len(sources) == 0 {
		return envelope, digest
	}
	files := localizationBodyMentionFiles(envelope)
	declarations := make(map[string][]*graph.Node, len(files))
	for _, file := range files {
		declarations[file] = cache.boundedDefinitions(file, localizationBodyMentionDeclarationCap)
	}

	taskTerms := exploreTerminalTerms(task)
	candidates := make([]localizationBodyMentionCandidate, 0, localizationBodyMentionCap)
	checks := 0
scan:
	for _, source := range sources {
		for pass := 0; pass < 2; pass++ {
			for _, file := range files {
				sameFile := file == source.file
				if sameFile != (pass == 0) {
					continue
				}
				for _, node := range declarations[file] {
					if !localizationBodyMentionNodeEligible(node) {
						continue
					}
					if _, visible := seenIDs[node.ID]; visible {
						continue
					}
					if checks >= localizationBodyMentionCheckCap {
						break scan
					}
					checks++
					name, ok := localizationBodyMentionIdentifier(node.Name)
					if !ok {
						continue
					}
					if _, mentioned := source.tokens[name]; !mentioned {
						continue
					}
					overlap, longest := exploreDraftTermOverlap(taskTerms, node)
					candidate := localizationBodyMentionCandidate{
						node:       node,
						sameFile:   sameFile,
						overlap:    overlap,
						longest:    longest,
						direct:     localizationBodyMentionDirect(source.owner, node.ID),
						ownerRank:  source.owner.Rank,
						ownerIndex: source.ownerIndex,
					}
					candidates = localizationBodyMentionKeepBest(candidates, candidate)
				}
			}
		}
	}

	admitted := 0
	for _, mention := range candidates {
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

			supportingOnly: true,
		}
		candidate := envelope
		// Files, Symbols, and Evidence are positional arrays. Repeated file
		// paths are intentional: row N must align across all three arrays.
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

func localizationBodyMentionSources(envelope localizationExploreEnvelope) []localizationBodyMentionSource {
	sources := make([]localizationBodyMentionSource, 0, min(len(envelope.Evidence)+1, localizationBodyMentionSourceCap))
	remainingBytes := localizationBodyMentionTotalSourceByteCap
	appendSource := func(owner localizationEvidence, ownerIndex int, file, content string) {
		if len(sources) >= localizationBodyMentionSourceCap || remainingBytes <= 0 || strings.TrimSpace(content) == "" {
			return
		}
		content = localizationBodyMentionBoundedSource(content, remainingBytes)
		tokens := localizationBodyMentionIdentifiers(content)
		if len(tokens) == 0 {
			return
		}
		sources = append(sources, localizationBodyMentionSource{
			owner: owner, ownerIndex: ownerIndex, file: strings.TrimSpace(file), tokens: tokens,
		})
		remainingBytes -= len(content)
	}
	// The elected source window is exact request evidence and would otherwise
	// be invisible to this pass. Scan it first so ordinary bodies cannot consume
	// the bounded source allowance before the window-only sibling is considered.
	if window := envelope.SourceWindow; window != nil {
		for index, owner := range envelope.Evidence {
			if owner.ID != window.AnchorSymbol {
				continue
			}
			appendSource(owner, index, window.Path, window.Content)
			break
		}
	}
	for index, owner := range envelope.Evidence {
		appendSource(owner, index, owner.File, owner.Source)
	}
	return sources
}

func localizationBodyMentionFiles(envelope localizationExploreEnvelope) []string {
	files := make([]string, 0, localizationBodyMentionFileCap)
	seen := make(map[string]struct{}, localizationBodyMentionFileCap)
	appendFile := func(file string) {
		file = strings.TrimSpace(file)
		if file == "" || len(files) >= localizationBodyMentionFileCap {
			return
		}
		if _, duplicate := seen[file]; duplicate {
			return
		}
		seen[file] = struct{}{}
		files = append(files, file)
	}
	if envelope.SourceWindow != nil {
		appendFile(envelope.SourceWindow.Path)
	}
	for _, row := range envelope.Evidence {
		appendFile(row.File)
	}
	return files
}

func localizationBodyMentionBoundedSource(source string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if len(source) <= limit {
		return source
	}
	cut := limit
	for cut > 0 && !utf8.ValidString(source[:cut]) {
		cut--
	}
	return source[:cut]
}

func localizationBodyMentionIdentifiers(source string) map[string]struct{} {
	identifiers := make(map[string]struct{}, min(localizationBodyMentionTokenCap, 64))
	start := -1
	appendIdentifier := func(end int) {
		if start < 0 || end <= start || len(identifiers) >= localizationBodyMentionTokenCap {
			start = -1
			return
		}
		raw := source[start:end]
		start = -1
		if len(raw) > localizationBodyMentionTokenByteCap {
			return
		}
		identifiers[strings.ToLower(raw)] = struct{}{}
	}
	for index, r := range source {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '$' {
			if start < 0 {
				start = index
			}
			continue
		}
		appendIdentifier(index)
		if len(identifiers) >= localizationBodyMentionTokenCap {
			break
		}
	}
	if len(identifiers) < localizationBodyMentionTokenCap {
		appendIdentifier(len(source))
	}
	return identifiers
}

func localizationBodyMentionIdentifier(name string) (string, bool) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > localizationBodyMentionTokenByteCap {
		return "", false
	}
	for _, r := range name {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' && r != '$' {
			return "", false
		}
	}
	return strings.ToLower(name), true
}

func localizationBodyMentionKeepBest(
	candidates []localizationBodyMentionCandidate,
	candidate localizationBodyMentionCandidate,
) []localizationBodyMentionCandidate {
	for index := range candidates {
		if candidates[index].node.ID != candidate.node.ID {
			continue
		}
		if localizationBodyMentionLess(candidate, candidates[index]) {
			candidates[index] = candidate
		}
		sort.SliceStable(candidates, func(first, second int) bool {
			return localizationBodyMentionLess(candidates[first], candidates[second])
		})
		return candidates
	}
	candidates = append(candidates, candidate)
	sort.SliceStable(candidates, func(first, second int) bool {
		return localizationBodyMentionLess(candidates[first], candidates[second])
	})
	if len(candidates) > localizationBodyMentionCap {
		candidates = candidates[:localizationBodyMentionCap]
	}
	return candidates
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
