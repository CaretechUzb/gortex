package mcp

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// Hard terminal enforcement is deliberately narrower than answer readiness.
// The latter is a ranking decision; the former requires one of these bounded,
// production-proven evidence shapes and must survive final response packing.
const (
	localizationProvenanceSourceLiteralCallee   = "source_literal_callee"
	localizationProvenanceDivergentDefault      = "divergent_default_owner"
	localizationProvenanceDivergentDefaultType  = "divergent_default_type"
	localizationProvenanceImplementationRoute   = "implementation_route"
	localizationProvenanceImplementationTarget  = "implementation_target"
	localizationProvenanceTypedAnchorProjection = "typed_anchor_projection"
)

type localizationEvidenceProof struct {
	provenance string
	primary    string
	support    []string
}

func localizationStrongSourceLiteralCallee(target exploreTarget) bool {
	return target.sourceLiteral && target.sourceLiteralCallee && target.exactContent &&
		!target.exactContentAmbiguous && exploreHydratedProductionCallable(target)
}

func localizationStrongImplementationRoute(wrapper, implementation exploreTarget) bool {
	if !wrapper.directCalleesComplete ||
		!exploreHydratedProductionCallable(wrapper) ||
		!exploreHydratedProductionCallable(implementation) ||
		!exploreDraftGenericCandidate(wrapper.node, wrapper.source) ||
		exploreDraftGenericCandidate(implementation.node, implementation.source) {
		return false
	}
	matched := false
	for _, callee := range wrapper.callees {
		if callee == nil || callee.ID == "" || callee.ID == wrapper.node.ID ||
			exploreDraftIsTestNode(callee) ||
			(callee.Kind != graph.KindFunction && callee.Kind != graph.KindMethod) {
			continue
		}
		if callee.ID != implementation.node.ID {
			return false
		}
		matched = true
	}
	return matched
}

func localizationStrongEvidenceForCompletion(completion localizationCompletion, targets []exploreTarget) localizationEvidenceProof {
	if completion.State != localizationStateAnswerReady && completion.State != localizationStateNeedsExactRead {
		return localizationEvidenceProof{}
	}

	ownerID, ownerIndex := "", -1
	typeID := ""
	for index, target := range targets {
		if target.node == nil || target.node.ID == "" {
			continue
		}
		if target.divergentDefaultOwner && exploreHydratedProductionCallable(target) {
			ownerID, ownerIndex = target.node.ID, index
		}
		if target.divergentDefaultType {
			typeID = target.node.ID
		}
	}
	if ownerID != "" && typeID != "" &&
		((completion.ExactSymbol == "" && ownerIndex == 0) || completion.ExactSymbol == ownerID) {
		return localizationEvidenceProof{
			provenance: localizationProvenanceDivergentDefault,
			primary:    ownerID,
			support:    []string{typeID},
		}
	}

	selected := -1
	if completion.ExactSymbol != "" {
		for index, target := range targets {
			if target.node != nil && target.node.ID == completion.ExactSymbol {
				selected = index
				break
			}
		}
	} else if len(targets) > 0 {
		selected = 0
	}
	if selected >= 0 && localizationStrongSourceLiteralCallee(targets[selected]) {
		return localizationEvidenceProof{
			provenance: localizationProvenanceSourceLiteralCallee,
			primary:    targets[selected].node.ID,
		}
	}
	return localizationEvidenceProof{}
}

// localizationExactReadSatisfiedByEnvelope reports whether the single read the
// completion prescribes would only return evidence this envelope already
// carries. The authorized symbol must be packed WITH its source body, and that
// body must clear the same lead-alignment test the reserved read applies when
// it completes — so completing here accepts exactly what the round trip would.
func localizationExactReadSatisfiedByEnvelope(task string, envelope localizationExploreEnvelope) bool {
	symbol := strings.TrimSpace(envelope.Completion.ExactSymbol)
	if symbol == "" {
		return false
	}
	for _, evidence := range envelope.Evidence {
		if evidence.ID != symbol {
			continue
		}
		if strings.TrimSpace(evidence.Source) == "" || strings.TrimSpace(evidence.File) == "" {
			return false
		}
		row := localizationDigestRow{
			Rank:       evidence.Rank,
			ID:         evidence.ID,
			Name:       evidence.Name,
			QualName:   evidence.QualName,
			Kind:       evidence.Kind,
			File:       evidence.File,
			Line:       evidence.Line,
			Signature:  evidence.Signature,
			Callers:    append([]string(nil), evidence.Callers...),
			Callees:    append([]string(nil), evidence.Callees...),
			Provenance: evidence.Provenance,
		}
		return localizationReservedReadEvidenceAlignedWithLead(
			task, envelope.Completion.taskLead, symbol, []localizationDigestRow{row})
	}
	return false
}

// localizationRankedEvidenceCarriesLead reports whether the packed rows mention
// what the request is actually about, using the same lead-alignment bar the
// reserved read must clear before it may end a session. Ranking can be
// confident about a page that never names the request's subject; that page
// still owes the caller its one bounded call.
func localizationRankedEvidenceCarriesLead(
	task string,
	completion localizationCompletion,
	envelope localizationExploreEnvelope,
) bool {
	rows := make([]localizationDigestRow, 0, len(envelope.Evidence))
	for _, evidence := range envelope.Evidence {
		if strings.TrimSpace(evidence.ID) == "" || strings.TrimSpace(evidence.File) == "" {
			continue
		}
		rows = append(rows, localizationDigestRow{
			Rank: evidence.Rank, ID: evidence.ID, Name: evidence.Name,
			QualName: evidence.QualName, Kind: evidence.Kind, File: evidence.File,
			Line: evidence.Line, Signature: evidence.Signature,
		})
	}
	return localizationReservedReadEvidenceAlignedWithLead(task, completion.taskLead, "", rows)
}

func localizationFinalizeCompletionEvidence(
	task string,
	completion localizationCompletion,
	targets []exploreTarget,
	envelope localizationExploreEnvelope,
) localizationCompletion {
	// Never trust an upstream or caller-supplied verdict. The policy is the
	// sole producer of enforceability for initial localization responses.
	completion.Enforceable = false
	completion.enforceableOnAnswerReady = false
	proof := localizationStrongEvidenceForCompletion(completion, targets)
	if !localizationEvidenceProofVisible(proof, envelope) {
		// A ranked head that is answer-ready stays terminal without one of the
		// hard provenance shapes: its evidence and ready-to-emit answer are
		// already packed here, so an extra bounded call only re-reads what the
		// caller can see. The proof keeps gating hard enforcement. Evidence that
		// does not carry the request's lead is the exception — unproven AND
		// unaligned is where ranked confidence has been wrong, so that page
		// keeps its one bounded call.
		if completion.State == localizationStateAnswerReady &&
			(len(envelope.Evidence) > 0 || len(envelope.Symbols) > 0) {
			recovery := newLocalizationRecoveryCompletion()
			recovery.digest = completion.digest
			return recovery
		}
		return completion
	}
	switch completion.State {
	case localizationStateAnswerReady:
		completion.Enforceable = true
	case localizationStateNeedsExactRead:
		completion.enforceableOnAnswerReady = true
	}
	return completion
}

func localizationBoundRouteEvidence(
	routes map[string]localizationRefinementRoute,
	envelope localizationExploreEnvelope,
) map[string]localizationRefinementRoute {
	for symbol, route := range routes {
		if !route.enforceable {
			continue
		}
		proof := localizationEvidenceProof{
			provenance: localizationProvenanceSourceLiteralCallee,
			primary:    symbol,
		}
		switch {
		case route.proofSymbol != "":
			proof.provenance = localizationProvenanceImplementationRoute
			proof.primary = route.proofSymbol
			proof.support = []string{symbol}
		case route.implementationSymbol != "":
			proof.provenance = localizationProvenanceImplementationRoute
			proof.support = []string{route.implementationSymbol}
		}
		if !localizationEvidenceProofVisible(proof, envelope) {
			route.enforceable = false
			routes[symbol] = route
		}
	}
	return routes
}

func localizationEvidenceProofVisible(proof localizationEvidenceProof, envelope localizationExploreEnvelope) bool {
	if proof.provenance == "" || proof.primary == "" {
		return false
	}
	visible := make(map[string]string, len(envelope.Evidence))
	for _, evidence := range envelope.Evidence {
		if evidence.ID != "" {
			visible[evidence.ID] = evidence.Provenance
		}
	}
	if visible[proof.primary] != proof.provenance {
		return false
	}
	for _, support := range proof.support {
		expected := ""
		switch proof.provenance {
		case localizationProvenanceDivergentDefault:
			expected = localizationProvenanceDivergentDefaultType
		case localizationProvenanceImplementationRoute:
			expected = localizationProvenanceImplementationTarget
		}
		if support == "" || visible[support] != expected {
			return false
		}
	}
	return true
}

func localizationTargetProvenance(completion localizationCompletion, target exploreTarget) string {
	if target.divergentDefaultOwner {
		return localizationProvenanceDivergentDefault
	}
	if target.divergentDefaultType {
		return localizationProvenanceDivergentDefaultType
	}
	if target.node == nil {
		return ""
	}
	// Refinement routes need both distinct role markers on the packed wire.
	// Give that paired proof priority over any independent literal role the
	// same target may also carry.
	for symbol, route := range completion.refinementRoutes {
		if !route.enforceable {
			continue
		}
		if route.proofSymbol != "" {
			if target.node.ID == route.proofSymbol {
				return localizationProvenanceImplementationRoute
			}
			if target.node.ID == symbol {
				return localizationProvenanceImplementationTarget
			}
		}
		if target.node.ID == symbol && route.implementationSymbol != "" {
			return localizationProvenanceImplementationRoute
		}
		if target.node.ID == route.implementationSymbol {
			return localizationProvenanceImplementationTarget
		}
	}
	if localizationStrongSourceLiteralCallee(target) {
		return localizationProvenanceSourceLiteralCallee
	}
	if target.typedAnchorProjection {
		return localizationProvenanceTypedAnchorProjection
	}
	return ""
}
