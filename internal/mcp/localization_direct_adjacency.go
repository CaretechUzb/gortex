package mcp

import (
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

const (
	localizationDirectAdjacencyCap        = 8
	localizationDirectAdjacencyLookupCap  = localizationReplayEvidenceLimit * 6
	localizationProvenanceDirectAdjacency = "direct_adjacency"
)

type localizationDirectAdjacencyCandidate struct {
	node               *graph.Node
	ownerFile          string
	ownerIndex         int
	relationOrder      int
	direction          int
	matched            int
	longest            int
	production         bool
	callable           bool
	sameFile           bool
	taskCited          bool
	taskCitationOffset int
}

func localizationDirectAdjacencyNodeTaskCitationOffset(task string, node *graph.Node) int {
	if node == nil {
		return -1
	}
	return localizationEvidenceTaskCitationOffset(task, localizationEvidence{
		ID: node.ID, Name: node.Name, QualName: node.QualName,
	})
}

// promoteLocalizationDirectAdjacency promotes only graph-authenticated nodes
// already named by the bounded caller/callee IDs in the retained result. It
// performs one bounded batch lookup and never traverses another adjacency list.
func promoteLocalizationDirectAdjacency(
	task string,
	envelope localizationExploreEnvelope,
	reader graph.Reader,
	maxEvidence int,
	maxBytes int,
	digest *localizationEvidenceDigest,
) (localizationExploreEnvelope, *localizationEvidenceDigest) {
	if reader == nil || len(envelope.Evidence) == 0 {
		return envelope, digest
	}

	seenIDs := make(map[string]struct{}, len(envelope.Evidence)+localizationDirectAdjacencyLookupCap)
	for _, row := range envelope.Evidence {
		if id := strings.TrimSpace(row.ID); id != "" {
			seenIDs[id] = struct{}{}
		}
	}

	type relation struct {
		id            string
		ownerFile     string
		ownerIndex    int
		relationOrder int
		direction     int
	}
	relations := make([]relation, 0, localizationDirectAdjacencyLookupCap)
	lookupIDs := make([]string, 0, localizationDirectAdjacencyLookupCap)
	lookupSeen := make(map[string]struct{}, localizationDirectAdjacencyLookupCap)
	appendRelations := func(ids []string, owner localizationEvidence, ownerIndex, direction int) {
		if len(ids) > localizationMaxNeighborIDs {
			ids = ids[:localizationMaxNeighborIDs]
		}
		for relationOrder, rawID := range ids {
			// Relation contexts are independently bounded, but an identity may
			// appear beside several owners. Retain those contexts so ranking can
			// select its best same-file/direction proof while the store lookup
			// remains one deduplicated batch.
			if len(relations) >= localizationDirectAdjacencyLookupCap {
				return
			}
			id := strings.TrimSpace(rawID)
			if id == "" {
				continue
			}
			if _, exists := seenIDs[id]; exists {
				continue
			}
			if _, exists := lookupSeen[id]; !exists {
				lookupSeen[id] = struct{}{}
				lookupIDs = append(lookupIDs, id)
			}
			relations = append(relations, relation{
				id:            id,
				ownerFile:     strings.TrimSpace(owner.File),
				ownerIndex:    ownerIndex,
				relationOrder: relationOrder,
				direction:     direction,
			})
		}
	}
	for ownerIndex, owner := range envelope.Evidence {
		// Callees are usually the implementation named by a caller-shaped task;
		// callers remain equally eligible and win whenever task alignment is better.
		appendRelations(owner.Callees, owner, ownerIndex, 0)
		appendRelations(owner.Callers, owner, ownerIndex, 1)
		if len(relations) >= localizationDirectAdjacencyLookupCap {
			break
		}
	}
	if len(lookupIDs) == 0 {
		return envelope, digest
	}

	nodes := reader.GetNodesByIDs(lookupIDs)
	taskTerms := exploreTerminalTerms(task)
	candidates := make([]localizationDirectAdjacencyCandidate, 0, len(relations))
	for _, relation := range relations {
		node := nodes[relation.id]
		if node == nil || strings.TrimSpace(node.ID) != relation.id || !localizationBodyMentionNodeEligible(node) {
			continue
		}
		matched, longest := exploreDraftTermOverlap(taskTerms, node)
		kind := strings.ToLower(strings.TrimSpace(string(node.Kind)))
		taskCitationOffset := localizationDirectAdjacencyNodeTaskCitationOffset(task, node)
		candidates = append(candidates, localizationDirectAdjacencyCandidate{
			node:               node,
			ownerFile:          relation.ownerFile,
			ownerIndex:         relation.ownerIndex,
			relationOrder:      relation.relationOrder,
			direction:          relation.direction,
			matched:            matched,
			longest:            longest,
			production:         !exploreDraftIsTestNode(node),
			callable:           kind == "function" || kind == "method",
			sameFile:           relation.ownerFile != "" && nodeDisplayPath(node) == relation.ownerFile,
			taskCited:          taskCitationOffset >= 0,
			taskCitationOffset: taskCitationOffset,
		})
	}
	if len(candidates) == 0 {
		return envelope, digest
	}

	sort.SliceStable(candidates, func(first, second int) bool {
		left, right := candidates[first], candidates[second]
		if left.matched != right.matched {
			return left.matched > right.matched
		}
		if left.production != right.production {
			return left.production
		}
		if left.callable != right.callable {
			return left.callable
		}
		if left.sameFile != right.sameFile {
			return left.sameFile
		}
		if left.longest != right.longest {
			return left.longest > right.longest
		}
		if left.ownerIndex != right.ownerIndex {
			return left.ownerIndex < right.ownerIndex
		}
		if left.direction != right.direction {
			return left.direction < right.direction
		}
		if left.relationOrder != right.relationOrder {
			return left.relationOrder < right.relationOrder
		}
		return left.node.ID < right.node.ID
	})

	// PRIMARY authority follows the request's first exact identifier citation,
	// while the evidence list keeps the established graph/task-score order.
	taskCitedPrimaryID := ""
	taskCitedPrimaryOffset := -1
	for _, candidate := range candidates {
		if candidate.taskCitationOffset >= 0 &&
			(taskCitedPrimaryOffset < 0 || candidate.taskCitationOffset < taskCitedPrimaryOffset) {
			taskCitedPrimaryID = strings.TrimSpace(candidate.node.ID)
			taskCitedPrimaryOffset = candidate.taskCitationOffset
		}
	}

	promoted := 0
	taskCitedPrimaryPending := taskCitedPrimaryID != ""
	for _, candidate := range candidates {
		if promoted >= localizationDirectAdjacencyCap {
			break
		}
		node := candidate.node
		id := strings.TrimSpace(node.ID)
		if taskCitedPrimaryPending && id != taskCitedPrimaryID &&
			promoted == localizationDirectAdjacencyCap-1 {
			continue
		}
		if id == taskCitedPrimaryID {
			taskCitedPrimaryPending = false
		}
		if _, exists := seenIDs[id]; exists {
			continue
		}
		row := localizationEvidence{
			Rank:                     len(envelope.Evidence) + 1,
			ID:                       id,
			Name:                     compactLocalizationField(node.Name, localizationMaxNameRunes),
			QualName:                 compactLocalizationField(node.QualName, localizationMaxNameRunes),
			Kind:                     string(node.Kind),
			File:                     nodeDisplayPath(node),
			Line:                     node.StartLine,
			EndLine:                  node.EndLine,
			Provenance:               localizationProvenanceDirectAdjacency,
			taskCitedPrimaryEligible: candidate.taskCited && id == taskCitedPrimaryID,
		}
		if row.File == "" {
			continue
		}

		candidateEnvelope, admitted := localizationDirectAdjacencyEnvelopeWithRow(task, envelope, digest, row, maxEvidence)
		if !admitted {
			continue
		}
		candidateDigest := newLocalizationEvidenceDigestForTask(task, candidateEnvelope)
		if row.taskCitedPrimaryEligible {
			candidateEnvelope, candidateDigest = promoteLocalizationTaskCitedAdjacencyPrimary(
				task, candidateEnvelope, candidateDigest, row.ID,
			)
		}
		if !localizationDirectAdjacencyDigestContains(candidateDigest, row.ID) {
			continue
		}
		contract := localizationContractReconciledWithDigest(candidateEnvelope.Completion, candidateDigest)
		candidateEnvelope.Completion = contract.Completion
		candidateEnvelope.Terminal = contract.Terminal
		if !localizationEnvelopeFits(candidateEnvelope, maxBytes) {
			continue
		}
		envelope, digest = candidateEnvelope, candidateDigest
		seenIDs[id] = struct{}{}
		promoted++
	}
	return envelope, digest
}

func localizationDirectAdjacencyEnvelopeWithRow(
	task string,
	envelope localizationExploreEnvelope,
	digest *localizationEvidenceDigest,
	row localizationEvidence,
	maxEvidence int,
) (localizationExploreEnvelope, bool) {
	if maxEvidence <= 0 {
		return envelope, false
	}
	evidence := append([]localizationEvidence(nil), envelope.Evidence...)
	if len(evidence) >= maxEvidence {
		retainedDigest := digest
		if retainedDigest == nil {
			retainedDigest = newLocalizationEvidenceDigestForTask(task, envelope)
		}
		// Contract priorities include exact/allowed identities and every
		// implementation/proof dependency of an authorized refinement route.
		// Supporting adjacency must never narrow that live authorization merely
		// because only five of its rows fit the model-facing PRIMARY block.
		protectedIDs := localizationDigestPriorityIDs(envelope.Completion, evidence)
		if retainedDigest != nil {
			for _, presented := range localizationFinalResponseRows(task, nil, retainedDigest.Evidence) {
				if presented.primary {
					protectedIDs[strings.TrimSpace(presented.row.ID)] = struct{}{}
				}
			}
		}
		replace := -1
		for index := len(evidence) - 1; index >= 0; index-- {
			id := strings.TrimSpace(evidence[index].ID)
			if _, protected := protectedIDs[id]; protected {
				continue
			}
			if localizationEvidenceTaskCited(task, evidence[index]) {
				continue
			}
			if localizationSupportingOnlyProvenance(evidence[index].Provenance) {
				continue
			}
			replace = index
			break
		}
		if replace < 0 && row.taskCitedPrimaryEligible {
			victimID, victimOrder := localizationTaskCitedAdjacencyPrimaryVictim(task, retainedDigest, row.ID)
			for index := range evidence {
				if evidence[index].ID == victimID {
					replace = index
					row.primaryCohortOrder = victimOrder
					break
				}
			}
		}
		if replace < 0 {
			return envelope, false
		}
		evidence = append(evidence[:replace], evidence[replace+1:]...)
	}
	if len(evidence) >= maxEvidence {
		return envelope, false
	}
	evidence = append(evidence, row)
	for index := range evidence {
		evidence[index].Rank = index + 1
	}

	candidate := envelope
	candidate.Evidence = evidence
	candidate.Files = make([]string, 0, len(evidence))
	candidate.Symbols = make([]string, 0, len(evidence))
	for _, retained := range evidence {
		candidate.Files = append(candidate.Files, retained.File)
		candidate.Symbols = append(candidate.Symbols, retained.ID)
	}
	return candidate, true
}

func localizationTaskCitedAdjacencyPrimaryVictim(
	task string,
	digest *localizationEvidenceDigest,
	candidateID string,
) (string, int) {
	if digest == nil {
		return "", 0
	}
	for _, row := range digest.Evidence {
		if row.ID != candidateID && row.taskCitedPrimaryEligible && row.primaryCohortOrder > 0 {
			return "", 0
		}
	}
	for order := localizationFinalResponsePrimaryLimit; order >= 2; order-- {
		for _, row := range digest.Evidence {
			if row.primaryCohortOrder != order || row.ID == candidateID ||
				row.authorizationPriority || localizationFinalResponsePrimaryProvenance(row) ||
				localizationDigestRowIdentifierTaskCited(task, row) {
				continue
			}
			return row.ID, order
		}
	}
	return "", 0
}

func promoteLocalizationTaskCitedAdjacencyPrimary(
	task string,
	envelope localizationExploreEnvelope,
	digest *localizationEvidenceDigest,
	candidateID string,
) (localizationExploreEnvelope, *localizationEvidenceDigest) {
	candidateID = strings.TrimSpace(candidateID)
	if digest == nil || candidateID == "" {
		return envelope, digest
	}
	for _, row := range digest.Evidence {
		if row.ID == candidateID && row.taskCitedPrimaryEligible && row.primaryCohortOrder > 0 {
			return envelope, digest
		}
	}
	victimID, victimOrder := localizationTaskCitedAdjacencyPrimaryVictim(task, digest, candidateID)
	if victimID == "" {
		return envelope, digest
	}

	candidateEnvelopeIndex, victimEnvelopeIndex := -1, -1
	for index := range envelope.Evidence {
		switch envelope.Evidence[index].ID {
		case candidateID:
			candidateEnvelopeIndex = index
		case victimID:
			victimEnvelopeIndex = index
		}
	}
	candidateDigestIndex, victimDigestIndex := -1, -1
	for index := range digest.Evidence {
		switch digest.Evidence[index].ID {
		case candidateID:
			candidateDigestIndex = index
		case victimID:
			victimDigestIndex = index
		}
	}
	if candidateEnvelopeIndex < 0 || victimEnvelopeIndex < 0 ||
		candidateDigestIndex < 0 || victimDigestIndex < 0 {
		return envelope, digest
	}

	envelope.Evidence[victimEnvelopeIndex].primaryCohortOrder = 0
	envelope.Evidence[candidateEnvelopeIndex].taskCitedPrimaryEligible = true
	envelope.Evidence[candidateEnvelopeIndex].primaryCohortOrder = victimOrder
	digest.Evidence[victimDigestIndex].primaryCohortOrder = 0
	digest.Evidence[candidateDigestIndex].taskCitedPrimaryEligible = true
	digest.Evidence[candidateDigestIndex].primaryCohortOrder = victimOrder
	refreshLocalizationDigestResponses(digest, task, nil)
	return envelope, digest
}

func localizationDirectAdjacencyDigestContains(digest *localizationEvidenceDigest, id string) bool {
	if digest == nil {
		return false
	}
	for _, row := range digest.Evidence {
		if row.ID == id && row.Provenance == localizationProvenanceDirectAdjacency {
			return true
		}
	}
	return false
}

func localizationSupportingOnlyProvenance(provenance string) bool {
	return provenance == localizationProvenanceBodyMention || provenance == localizationProvenanceDirectAdjacency
}
