package mcp

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/zzet/gortex/internal/graph"
)

const (
	exploreDefaultSignatureCap     = 1024
	exploreDefaultParamCap         = 16
	exploreDefaultSourceCap        = 32 * 1024
	exploreDefaultOwnerBaseCap     = 3
	exploreDefaultOwnerEdgeCap     = 16
	exploreDefaultOwnerEndpointCap = 32
	exploreDefaultOwnerFileCap     = 3
	exploreDefaultOwnerFileNodeCap = 64
	exploreDefaultOwnerTotalCap    = 96
	// exploreDefaultOwnerFallbackSLO bounds the ranked-callable fallback that
	// runs when no base constructor/type pair was already ranked. It is a
	// SILENT bound: every check inside
	// exploreDivergentDefaultBasesFromRankedCallables returns "no bases" on
	// expiry, so the caller simply promotes nothing and the response is a
	// smaller symbol set with no error and no warning.
	//
	// 25ms is ample for the batched inbound lookup + node hydration on an idle
	// machine and is routinely exceeded on a loaded one, where those SQLite
	// reads are descheduled. That makes WHICH owner gets promoted a function
	// of machine load — fine for production (the fallback is best-effort by
	// design) and fatal for a test that asserts the promoted identity, which
	// is why promoteExploreDivergentDefaultOwner takes an overridable slice.
	exploreDefaultOwnerFallbackSLO = 25 * time.Millisecond
)

type exploreParameterDefault struct {
	name  string
	value string
}

type exploreDivergentDefaultOwner struct {
	constructor *graph.Node
	owner       *graph.Node
	baseCtor    *graph.Node
	baseOwner   *graph.Node
	parameter   string
	consumerID  string
}

type exploreDivergentDefaultBase struct {
	constructor *graph.Node
	owner       *graph.Node
	defaults    []exploreParameterDefault
	consumerID  string
}

type exploreDivergentDefaultProjection struct {
	callers   map[string][]string
	extenders map[string][]string
	nodes     map[string]*graph.Node
}

// promoteExploreDivergentDefaultOwner recognizes one high-confidence causal
// shape through a bounded, exact-ID projection from the graph store:
//
//	child constructor --calls--> base constructor
//	child type        --extends-> base type
//
// Both constructors must expose the same query-relevant parameter, with a
// neutral base default and a concrete child default. The projection starts only
// from already-ranked base constructor/type pairs, performs one batched inbound
// lookup and one batched node hydration, and fails closed at every cap. A unique
// match replaces its represented base pair before being moved to the head, so
// promotion never evicts an unrelated protected candidate.
// The optional fallbackSLO overrides the wall-clock slice the ranked-callable
// fallback may spend. Omitted (or zero) means the production default. Only a
// caller that ASSERTS which owner gets promoted passes one, so its answer
// cannot depend on machine load — see the note on
// exploreDefaultOwnerFallbackSLO. Left variadic so the fifteen call sites that
// never reach the fallback stay untouched.
func promoteExploreDivergentDefaultOwner(
	ctx context.Context,
	task string,
	targets []exploreTarget,
	reader graph.Reader,
	scope graph.LocalizationNodeScope,
	maxSymbols int,
	readSource func(*graph.Node) string,
	fallbackSLO ...time.Duration,
) []exploreTarget {
	if reader == nil || readSource == nil || len(targets) == 0 || !exploreQueryIsConceptTask(task) {
		return targets
	}
	taskTerms := exploreTerminalTerms(task)
	bases, ok := exploreDivergentDefaultBases(taskTerms, targets)
	if !ok {
		return targets
	}
	if len(bases) == 0 {
		slo := exploreDefaultOwnerFallbackSLO
		if len(fallbackSLO) > 0 && fallbackSLO[0] > 0 {
			slo = fallbackSLO[0]
		}
		deadline := time.Now().Add(slo)
		bases, ok = exploreDivergentDefaultBasesFromRankedCallables(
			ctx, taskTerms, targets, reader, scope, deadline,
		)
		if !ok || len(bases) == 0 {
			return targets
		}
	}
	projection, ok := projectExploreDivergentDefaultOwners(ctx, reader, bases)
	if !ok {
		return targets
	}
	match, ok := findExploreDivergentDefaultOwner(bases, projection)
	if !ok {
		return targets
	}

	constructor, _ := exploreTargetByID(targets, match.constructor.ID)
	constructor.node = match.constructor
	constructor.divergentDefaultOwner = true
	if constructor.source == "" {
		constructor.source = readSource(match.constructor)
	}
	if !exploreConstructorForwardsParameter(constructor.source, match.parameter, match.baseOwner.Name) {
		return targets
	}
	owner, _ := exploreTargetByID(targets, match.owner.ID)
	owner.node = match.owner
	owner.divergentDefaultType = true
	if len(constructor.callees) == 0 {
		constructor.callees = []*graph.Node{match.baseCtor}
	}
	if len(owner.callees) == 0 {
		owner.callees = []*graph.Node{match.baseOwner}
	}

	replaced, ok := placeExploreDivergentDefaultOwner(task, targets, match, constructor, owner, maxSymbols)
	if !ok {
		return targets
	}
	return prioritizeExploreTargetPair(replaced, match.constructor.ID, match.owner.ID)
}

// placeExploreDivergentDefaultOwner keeps every ranked consumer intact. When
// the base constructor/type pair was ranked, the proven child pair replaces
// it. The callable-owner fallback may discover an unranked base pair; when the
// result is already at capacity it admits the proof only by evicting the
// lowest-ranked unprotected candidates. The consuming callable, explicit and
// literal anchors, artifacts (kept outside this slice), and implementation
// evidence are never sacrificed.
func placeExploreDivergentDefaultOwner(task string, targets []exploreTarget, match exploreDivergentDefaultOwner, constructor, owner exploreTarget, maxSymbols int) ([]exploreTarget, bool) {
	if maxSymbols <= 0 {
		maxSymbols = len(targets)
	}
	missing := 0
	constructorIndex := exploreTargetIndex(targets, match.constructor.ID)
	baseConstructorIndex := exploreTargetIndex(targets, match.baseCtor.ID)
	if constructorIndex < 0 && (baseConstructorIndex < 0 || targets[baseConstructorIndex].sourceRange) {
		missing++
	}
	ownerIndex := exploreTargetIndex(targets, match.owner.ID)
	baseOwnerIndex := exploreTargetIndex(targets, match.baseOwner.ID)
	if ownerIndex < 0 && (baseOwnerIndex < 0 || targets[baseOwnerIndex].sourceRange) {
		missing++
	}
	overflow := len(targets) + missing - maxSymbols
	if overflow < 0 {
		overflow = 0
	}
	evict := make(map[int]struct{}, overflow)
	for index := len(targets) - 1; index >= 0 && len(evict) < overflow; index-- {
		if exploreDivergentDefaultAdmissionProtected(task, targets[index], match) {
			continue
		}
		evict[index] = struct{}{}
	}
	if len(evict) != overflow {
		return nil, false
	}

	replaced := make([]exploreTarget, 0, len(targets)-len(evict)+missing)
	for index, target := range targets {
		if _, drop := evict[index]; !drop {
			replaced = append(replaced, target)
		}
	}
	place := func(candidate exploreTarget, candidateID, baseID string) {
		if index := exploreTargetIndex(replaced, candidateID); index >= 0 {
			replaced[index] = candidate
			return
		}
		if index := exploreTargetIndex(replaced, baseID); index >= 0 {
			if !replaced[index].sourceRange {
				replaced[index] = candidate
				return
			}
		}
		replaced = append(replaced, candidate)
	}
	place(constructor, match.constructor.ID, match.baseCtor.ID)
	place(owner, match.owner.ID, match.baseOwner.ID)
	return replaced, true
}

func exploreDivergentDefaultAdmissionProtected(task string, target exploreTarget, match exploreDivergentDefaultOwner) bool {
	if target.node == nil {
		return true
	}
	id := target.node.ID
	if id == match.consumerID || id == match.constructor.ID || id == match.owner.ID ||
		id == match.baseCtor.ID || id == match.baseOwner.ID {
		return true
	}
	return target.divergentDefaultOwner || target.divergentDefaultType || target.conceptImplementation ||
		target.sourceRange || target.exactContent || target.exactContentAmbiguous || target.sourceLiteral ||
		exploreLocalizationExplicitAnchor(task, target.node)
}

func preserveExploreDivergentDefaultOrder(targets []exploreTarget) []exploreTarget {
	constructorID, ownerID := "", ""
	for _, target := range targets {
		if target.node == nil {
			continue
		}
		if target.divergentDefaultOwner {
			constructorID = target.node.ID
		}
		if target.divergentDefaultType {
			ownerID = target.node.ID
		}
	}
	if constructorID == "" || ownerID == "" {
		return targets
	}
	return prioritizeExploreTargetPair(targets, constructorID, ownerID)
}

func exploreDivergentDefaultOwnerSymbol(targets []exploreTarget) string {
	for _, target := range targets {
		if target.divergentDefaultOwner && target.node != nil {
			return target.node.ID
		}
	}
	return ""
}

func prioritizeExploreTargetPair(targets []exploreTarget, firstID, secondID string) []exploreTarget {
	first, firstFound := exploreTargetByID(targets, firstID)
	second, secondFound := exploreTargetByID(targets, secondID)
	if !firstFound || !secondFound {
		return targets
	}
	ordered := make([]exploreTarget, 0, len(targets))
	ordered = append(ordered, first, second)
	for _, target := range targets {
		if target.node == nil || target.node.ID == firstID || target.node.ID == secondID {
			continue
		}
		ordered = append(ordered, target)
	}
	if len(ordered) != len(targets) {
		return targets
	}
	return ordered
}

func exploreTargetByID(targets []exploreTarget, id string) (exploreTarget, bool) {
	if index := exploreTargetIndex(targets, id); index >= 0 {
		return targets[index], true
	}
	return exploreTarget{}, false
}

func exploreTargetIndex(targets []exploreTarget, id string) int {
	for index, target := range targets {
		if target.node != nil && target.node.ID == id {
			return index
		}
	}
	return -1
}

// exploreConstructorForwardsParameter confirms that the changed child default
// actually reaches the base constructor. The caller edge proves which symbol
// is invoked; this bounded lexical check proves that the shared parameter is
// present in that base/super call rather than merely declared independently by
// both constructors.
func exploreConstructorForwardsParameter(source, parameter, baseOwner string) bool {
	if source == "" || parameter == "" || len(source) > exploreDefaultSourceCap {
		return false
	}
	code := exploreMaskNonCode(source)
	for open := 0; open < len(code); open++ {
		if code[open] != '(' {
			continue
		}
		callee := exploreCallCalleeBefore(code, open)
		if !exploreBaseConstructorCallee(callee, baseOwner) {
			continue
		}
		close, ok := exploreMatchingParen(code, open)
		if !ok {
			return false
		}
		if exploreContainsIdentifier(code[open+1:close], parameter) {
			return true
		}
		open = close
	}
	return false
}

// exploreMaskNonCode preserves byte offsets while blanking comments and string
// literals. Forwarding evidence must come from executable arguments of the
// proven base-constructor call; examples in comments, exception text, and
// string interpolation are not data-flow evidence.
func exploreMaskNonCode(source string) string {
	masked := []byte(source)
	mask := func(start, end int) {
		if end > len(masked) {
			end = len(masked)
		}
		for index := start; index < end; index++ {
			if masked[index] != '\n' && masked[index] != '\r' {
				masked[index] = ' '
			}
		}
	}
	for index := 0; index < len(source); {
		switch {
		case index+1 < len(source) && source[index] == '/' && source[index+1] == '/':
			end := index + 2
			for end < len(source) && source[end] != '\n' && source[end] != '\r' {
				end++
			}
			mask(index, end)
			index = end
		case index+1 < len(source) && source[index] == '/' && source[index+1] == '*':
			end := index + 2
			for end+1 < len(source) && (source[end] != '*' || source[end+1] != '/') {
				end++
			}
			if end+1 < len(source) {
				end += 2
			} else {
				end = len(source)
			}
			mask(index, end)
			index = end
		case source[index] == '#' && (index+1 >= len(source) || source[index+1] != '['):
			end := index + 1
			for end < len(source) && source[end] != '\n' && source[end] != '\r' {
				end++
			}
			mask(index, end)
			index = end
		case source[index] == '\'' || source[index] == '"' || source[index] == '`':
			quote := source[index]
			end := index + 1
			escaped := false
			for end < len(source) {
				char := source[end]
				end++
				if escaped {
					escaped = false
					continue
				}
				if char == '\\' {
					escaped = true
					continue
				}
				if char == quote {
					break
				}
			}
			mask(index, end)
			index = end
		default:
			index++
		}
	}
	return string(masked)
}

func exploreCallCalleeBefore(source string, open int) string {
	end := open
	for end > 0 && unicode.IsSpace(rune(source[end-1])) {
		end--
	}
	start := end
	for start > 0 {
		char := source[start-1]
		if char == '_' || char == '.' || char == ':' || char == '$' || char == '(' || char == ')' ||
			char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' {
			start--
			continue
		}
		break
	}
	callee := strings.ToLower(source[start:end])
	callee = strings.ReplaceAll(callee, "()", "")
	return strings.Trim(callee, ".:$")
}

func exploreBaseConstructorCallee(callee, baseOwner string) bool {
	callee = strings.TrimSpace(callee)
	base := strings.ToLower(strings.TrimSpace(baseOwner))
	switch callee {
	case "parent::__construct", "super.__init__", "super.init", "super", "base":
		return true
	}
	return base != "" && (callee == base || strings.HasSuffix(callee, "."+base) || strings.HasSuffix(callee, "::"+base))
}

func exploreMatchingParen(source string, open int) (int, bool) {
	depth := 0
	quote := byte(0)
	escaped := false
	for index := open; index < len(source); index++ {
		char := source[index]
		if quote != 0 {
			if escaped {
				escaped = false
				continue
			}
			if char == '\\' {
				escaped = true
				continue
			}
			if char == quote {
				quote = 0
			}
			continue
		}
		switch char {
		case '\'', '"', '`':
			quote = char
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return index, true
			}
		}
	}
	return 0, false
}

func exploreContainsIdentifier(source, identifier string) bool {
	for offset := 0; offset < len(source); {
		index := strings.Index(strings.ToLower(source[offset:]), strings.ToLower(identifier))
		if index < 0 {
			return false
		}
		index += offset
		end := index + len(identifier)
		before := index == 0 || !exploreIdentifierByte(source[index-1])
		after := end == len(source) || !exploreIdentifierByte(source[end])
		if before && after {
			return true
		}
		offset = index + 1
	}
	return false
}

func exploreIdentifierByte(value byte) bool {
	return value == '_' || value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' || value >= '0' && value <= '9'
}

func exploreDivergentDefaultBases(taskTerms map[string]struct{}, targets []exploreTarget) ([]exploreDivergentDefaultBase, bool) {
	direct := make(map[string]exploreTarget, len(targets))
	for _, target := range targets {
		if target.node != nil && target.node.ID != "" {
			direct[target.node.ID] = target
		}
	}
	bases := make([]exploreDivergentDefaultBase, 0, exploreDefaultOwnerBaseCap)
	seen := make(map[string]struct{}, exploreDefaultOwnerBaseCap)
	for _, target := range targets {
		baseCtor := target.node
		if !exploreConstructorNode(baseCtor) {
			continue
		}
		baseOwnerID, _ := graph.EnclosingFromID(baseCtor.ID, baseCtor.Kind)
		baseOwnerTarget, found := direct[baseOwnerID]
		if !found || baseOwnerTarget.node == nil || baseOwnerTarget.node.Kind != graph.KindType {
			continue
		}
		defaults := parseExploreParameterDefaults(baseCtor.RetrievalMetadata().Signature)
		relevant := defaults[:0]
		for _, candidate := range defaults {
			if isNeutralExploreDefault(candidate.value) && exploreDefaultParameterRelevantTerms(taskTerms, candidate.name) {
				relevant = append(relevant, candidate)
			}
		}
		if len(relevant) == 0 {
			continue
		}
		if _, duplicate := seen[baseCtor.ID]; duplicate {
			continue
		}
		seen[baseCtor.ID] = struct{}{}
		bases = append(bases, exploreDivergentDefaultBase{
			constructor: baseCtor,
			owner:       baseOwnerTarget.node,
			defaults:    relevant,
		})
		if len(bases) > exploreDefaultOwnerBaseCap {
			return nil, false
		}
	}
	return bases, true
}

type exploreRankedCallableOwner struct {
	callable *graph.Node
	ownerID  string
	owner    *graph.Node
}

// exploreDivergentDefaultBasesFromRankedCallables is the strictly bounded
// recovery path for a common ranking gap: the consuming method is present but
// its constructor/type pair is not. It follows only the callable's exact
// enclosing owner, hydrates those owners in one batch, and reads each distinct
// owner file through a capped metadata-free projection. It never scans a
// repository or source tree, and ambiguous/oversized projections are rejected.
func exploreDivergentDefaultBasesFromRankedCallables(
	ctx context.Context,
	taskTerms map[string]struct{},
	targets []exploreTarget,
	reader graph.Reader,
	scope graph.LocalizationNodeScope,
	deadline time.Time,
) ([]exploreDivergentDefaultBase, bool) {
	if ctx == nil {
		ctx = context.Background()
	}
	if reader == nil || len(taskTerms) == 0 || ctx.Err() != nil || time.Now().After(deadline) {
		return nil, false
	}
	readCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	owners := make([]exploreRankedCallableOwner, 0, exploreDefaultOwnerFileCap)
	seenOwners := make(map[string]struct{}, exploreDefaultOwnerFileCap)
	for _, target := range targets {
		if readCtx.Err() != nil {
			return nil, false
		}
		callable := target.node
		if callable == nil || (callable.Kind != graph.KindMethod && callable.Kind != graph.KindFunction) ||
			exploreConstructorNode(callable) || exploreDraftIsTestNode(callable) {
			continue
		}
		ownerID, _ := graph.EnclosingFromID(callable.ID, callable.Kind)
		if ownerID == "" {
			continue
		}
		overlap, longest := exploreDraftTermOverlap(taskTerms, callable)
		relevant := overlap >= 2 || overlap == 1 && longest >= 5
		if !relevant {
			continue
		}
		if _, duplicate := seenOwners[ownerID]; duplicate {
			continue
		}
		seenOwners[ownerID] = struct{}{}
		owners = append(owners, exploreRankedCallableOwner{callable: callable, ownerID: ownerID})
		if len(owners) > exploreDefaultOwnerFileCap {
			return nil, false
		}
	}
	if len(owners) == 0 {
		return nil, true
	}

	ownerIDs := make([]string, 0, len(owners))
	for _, candidate := range owners {
		ownerIDs = append(ownerIDs, candidate.ownerID)
	}
	hydrated, complete := exploreNodesByIDsBounded(readCtx, reader, ownerIDs, exploreDefaultOwnerFileCap)
	if !complete {
		return nil, false
	}
	filePaths := make([]string, 0, len(owners))
	seenFiles := make(map[string]struct{}, len(owners))
	coherent := owners[:0]
	for _, candidate := range owners {
		candidate.owner = hydrated[candidate.ownerID]
		if !exploreRankedCallableOwnerCoherent(candidate.callable, candidate.owner) {
			continue
		}
		coherent = append(coherent, candidate)
		if _, duplicate := seenFiles[candidate.owner.FilePath]; duplicate {
			continue
		}
		seenFiles[candidate.owner.FilePath] = struct{}{}
		filePaths = append(filePaths, candidate.owner.FilePath)
	}
	if len(coherent) == 0 || len(filePaths) == 0 || len(filePaths) > exploreDefaultOwnerFileCap {
		return nil, len(coherent) == 0
	}

	budget := localizationFileBudgetFor(ctx)
	fileCandidates := make(map[string][]string, len(filePaths))
	constructorIDs := make([]string, 0, exploreDefaultOwnerTotalCap)
	seenConstructors := make(map[string]struct{}, exploreDefaultOwnerTotalCap)
	totalRows := 0
	for _, path := range filePaths {
		if readCtx.Err() != nil {
			return nil, false
		}
		remaining := exploreDefaultOwnerTotalCap - totalRows
		pageLimit := exploreDefaultOwnerFileNodeCap
		if remaining < pageLimit {
			pageLimit = remaining
		}
		if pageLimit <= 0 {
			// A one-row sentinel distinguishes an empty remaining file from a
			// total-cap overflow without opening an unbounded scan.
			pageLimit = 1
		}
		page, complete := boundedLocalizationFileNodes(
			readCtx, reader, budget, path, graph.LocalizationNodeScope{}, pageLimit,
		)
		if !complete {
			return nil, false
		}
		rows := page.Total
		if len(page.Nodes) > rows {
			rows = len(page.Nodes)
		}
		if rows > exploreDefaultOwnerFileNodeCap || totalRows+rows > exploreDefaultOwnerTotalCap {
			return nil, false
		}
		totalRows += rows
		for _, summary := range page.Nodes {
			if !exploreConstructorNode(summary) || summary.ID == "" {
				continue
			}
			if _, duplicate := seenConstructors[summary.ID]; duplicate {
				continue
			}
			seenConstructors[summary.ID] = struct{}{}
			constructorIDs = append(constructorIDs, summary.ID)
			fileCandidates[path] = append(fileCandidates[path], summary.ID)
		}
	}

	constructors, complete := exploreNodesByIDsBounded(
		readCtx, reader, constructorIDs, exploreDefaultOwnerTotalCap,
	)
	if !complete {
		return nil, false
	}
	bases := make([]exploreDivergentDefaultBase, 0, len(coherent))
	for _, candidate := range coherent {
		if !scope.Allows(candidate.owner) {
			continue
		}
		var viable []exploreDivergentDefaultBase
		for _, id := range fileCandidates[candidate.owner.FilePath] {
			node := constructors[id]
			if !exploreConstructorNode(node) || exploreDraftIsTestNode(node) || !scope.Allows(node) {
				continue
			}
			ownerID, _ := graph.EnclosingFromID(node.ID, node.Kind)
			if ownerID != candidate.owner.ID || !exploreRankedOwnerConstructorCoherent(candidate.callable, candidate.owner, node) {
				continue
			}
			defaults := parseExploreParameterDefaults(node.RetrievalMetadata().Signature)
			relevantDefaults := make([]exploreParameterDefault, 0, len(defaults))
			for _, parameter := range defaults {
				if isNeutralExploreDefault(parameter.value) && exploreDefaultParameterRelevantTerms(taskTerms, parameter.name) {
					relevantDefaults = append(relevantDefaults, parameter)
				}
			}
			if len(relevantDefaults) > 0 {
				viable = append(viable, exploreDivergentDefaultBase{
					constructor: node,
					owner:       candidate.owner,
					defaults:    relevantDefaults,
					consumerID:  candidate.callable.ID,
				})
			}
		}
		if len(viable) > 1 {
			return nil, false
		}
		if len(viable) == 1 {
			bases = append(bases, viable[0])
			if len(bases) > exploreDefaultOwnerBaseCap {
				return nil, false
			}
		}
	}
	return bases, readCtx.Err() == nil
}

func exploreRankedCallableOwnerCoherent(callable, owner *graph.Node) bool {
	if callable == nil || owner == nil || owner.Kind != graph.KindType || owner.ID == "" ||
		callable.FilePath == "" || callable.FilePath != owner.FilePath ||
		exploreDraftIsTestNode(callable) || exploreDraftIsTestNode(owner) {
		return false
	}
	ownerID, _ := graph.EnclosingFromID(callable.ID, callable.Kind)
	return ownerID == owner.ID && exploreNodesShareExactScope(callable, owner)
}

func exploreRankedOwnerConstructorCoherent(callable, owner, constructor *graph.Node) bool {
	return exploreRankedCallableOwnerCoherent(callable, owner) && constructor != nil &&
		constructor.FilePath == owner.FilePath && exploreNodesShareExactScope(owner, constructor)
}

func exploreNodesShareExactScope(left, right *graph.Node) bool {
	if left == nil || right == nil || left.RepoPrefix != right.RepoPrefix ||
		left.WorkspaceID != right.WorkspaceID || left.ProjectID != right.ProjectID {
		return false
	}
	return left.Language == "" || right.Language == "" || strings.EqualFold(left.Language, right.Language)
}

// projectExploreDivergentDefaultOwners reads only direct inbound adjacency for
// the bounded ranked seed set. Unlike query.Engine.GetCallers, graph.Store
// preserves the relationship kind and full node metadata needed by this proof.
// Any oversized or partially hydrated projection is rejected rather than
// interpreted as evidence that no competing child exists.
func projectExploreDivergentDefaultOwners(
	ctx context.Context,
	reader graph.Reader,
	bases []exploreDivergentDefaultBase,
) (exploreDivergentDefaultProjection, bool) {
	if ctx == nil {
		ctx = context.Background()
	}
	if reader == nil || len(bases) == 0 || len(bases) > exploreDefaultOwnerBaseCap || ctx.Err() != nil {
		return exploreDivergentDefaultProjection{}, false
	}
	constructorIDs := make([]string, 0, len(bases))
	ownerIDs := make([]string, 0, len(bases))
	for _, base := range bases {
		if base.constructor == nil || base.constructor.ID == "" || base.owner == nil || base.owner.ID == "" {
			return exploreDivergentDefaultProjection{}, false
		}
		constructorIDs = append(constructorIDs, base.constructor.ID)
		ownerIDs = append(ownerIDs, base.owner.ID)
	}
	callers, complete := exploreIncomingSourcesBounded(
		ctx, reader, constructorIDs, graph.EdgeCalls, exploreDefaultOwnerEdgeCap,
	)
	if !complete {
		return exploreDivergentDefaultProjection{}, false
	}
	extenders, complete := exploreIncomingSourcesBounded(
		ctx, reader, ownerIDs, graph.EdgeExtends, exploreDefaultOwnerEdgeCap,
	)
	if !complete {
		return exploreDivergentDefaultProjection{}, false
	}
	projection := exploreDivergentDefaultProjection{
		callers:   make(map[string][]string, len(bases)),
		extenders: make(map[string][]string, len(bases)),
	}
	endpointIDs := make(map[string]struct{}, exploreDefaultOwnerEndpointCap)
	for _, base := range bases {
		constructorID := base.constructor.ID
		ownerID := base.owner.ID
		if callers.Truncated[constructorID] || extenders.Truncated[ownerID] {
			return exploreDivergentDefaultProjection{}, false
		}
		projection.callers[constructorID] = append([]string(nil), callers.Sources[constructorID]...)
		projection.extenders[ownerID] = append([]string(nil), extenders.Sources[ownerID]...)
		for _, id := range projection.callers[constructorID] {
			endpointIDs[id] = struct{}{}
		}
		for _, id := range projection.extenders[ownerID] {
			endpointIDs[id] = struct{}{}
		}
		if len(endpointIDs) > exploreDefaultOwnerEndpointCap {
			return exploreDivergentDefaultProjection{}, false
		}
	}
	ids := make([]string, 0, len(endpointIDs))
	for id := range endpointIDs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	projection.nodes, complete = exploreNodesByIDsBounded(
		ctx, reader, ids, exploreDefaultOwnerEndpointCap,
	)
	if !complete {
		return exploreDivergentDefaultProjection{}, false
	}
	return projection, true
}

func findExploreDivergentDefaultOwner(bases []exploreDivergentDefaultBase, projection exploreDivergentDefaultProjection) (exploreDivergentDefaultOwner, bool) {

	matches := make([]exploreDivergentDefaultOwner, 0, 1)
	seen := make(map[string]struct{})
	childDefaults := make(map[string][]exploreParameterDefault)
	for _, base := range bases {
		for _, childCtorID := range projection.callers[base.constructor.ID] {
			childCtor := projection.nodes[childCtorID]
			if !exploreConstructorNode(childCtor) {
				continue
			}
			childOwnerID, _ := graph.EnclosingFromID(childCtor.ID, childCtor.Kind)
			if !exploreStringSliceContains(projection.extenders[base.owner.ID], childOwnerID) {
				continue
			}
			childOwner := projection.nodes[childOwnerID]
			if childOwner == nil || childOwner.Kind != graph.KindType ||
				!exploreDefaultOwnerNodesCoherent(base.constructor, childCtor, base.owner, childOwner) {
				continue
			}
			defaults, parsed := childDefaults[childCtor.ID]
			if !parsed {
				defaults = parseExploreParameterDefaults(childCtor.RetrievalMetadata().Signature)
				childDefaults[childCtor.ID] = defaults
			}
			for _, baseDefault := range base.defaults {
				childDefault, found := exploreParameterDefaultByName(defaults, baseDefault.name)
				if !found || !isConcreteExploreDefault(childDefault.value) {
					continue
				}
				key := childCtor.ID + "\x00" + childOwner.ID + "\x00" + strings.ToLower(baseDefault.name)
				if _, duplicate := seen[key]; duplicate {
					continue
				}
				seen[key] = struct{}{}
				matches = append(matches, exploreDivergentDefaultOwner{
					constructor: childCtor,
					owner:       childOwner,
					baseCtor:    base.constructor,
					baseOwner:   base.owner,
					parameter:   baseDefault.name,
					consumerID:  base.consumerID,
				})
			}
		}
	}
	if len(matches) != 1 {
		return exploreDivergentDefaultOwner{}, false
	}
	return matches[0], true
}

func exploreStringSliceContains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

func exploreDefaultOwnerNodesCoherent(baseCtor, childCtor, baseOwner, childOwner *graph.Node) bool {
	if baseCtor == nil || childCtor == nil || baseOwner == nil || childOwner == nil ||
		childCtor.FilePath == "" || childCtor.FilePath != childOwner.FilePath ||
		baseCtor.FilePath == "" || baseCtor.FilePath != baseOwner.FilePath ||
		childOwner.ID == baseOwner.ID || exploreDraftIsTestNode(childCtor) || exploreDraftIsTestNode(childOwner) {
		return false
	}
	if baseCtor.RepoPrefix != childCtor.RepoPrefix || baseCtor.WorkspaceID != childCtor.WorkspaceID || baseCtor.ProjectID != childCtor.ProjectID ||
		baseCtor.RepoPrefix != baseOwner.RepoPrefix || baseCtor.WorkspaceID != baseOwner.WorkspaceID || baseCtor.ProjectID != baseOwner.ProjectID ||
		childCtor.RepoPrefix != childOwner.RepoPrefix || childCtor.WorkspaceID != childOwner.WorkspaceID || childCtor.ProjectID != childOwner.ProjectID {
		return false
	}
	for _, node := range []*graph.Node{baseOwner, childCtor, childOwner} {
		if baseCtor.Language != "" && node.Language != "" && !strings.EqualFold(baseCtor.Language, node.Language) {
			return false
		}
	}
	return true
}

func exploreConstructorNode(node *graph.Node) bool {
	if node == nil || (node.Kind != graph.KindMethod && node.Kind != graph.KindFunction) {
		return false
	}
	_, ownerName := graph.EnclosingFromID(node.ID, node.Kind)
	if ownerName == "" {
		return false
	}
	name := strings.ToLower(strings.TrimSpace(node.Name))
	switch name {
	case "__construct", "__init__", "constructor", "init", "initialize", "<init>":
		return true
	}
	return strings.EqualFold(exploreDefaultIdentifier(name), exploreDefaultIdentifier(ownerName))
}

func parseExploreParameterDefaults(signature string) []exploreParameterDefault {
	if len(signature) == 0 || len(signature) > exploreDefaultSignatureCap {
		return nil
	}
	parameters, ok := exploreOuterParameterList(signature)
	if !ok {
		return nil
	}
	parts, ok := splitExploreParameters(parameters)
	if !ok || len(parts) > exploreDefaultParamCap {
		return nil
	}
	defaults := make([]exploreParameterDefault, 0, len(parts))
	for _, parameter := range parts {
		assignment := exploreTopLevelAssignment(parameter)
		if assignment < 0 {
			continue
		}
		name := exploreParameterName(parameter[:assignment])
		value := strings.TrimSpace(parameter[assignment+1:])
		if name == "" || value == "" {
			continue
		}
		defaults = append(defaults, exploreParameterDefault{name: name, value: value})
	}
	return defaults
}

func exploreOuterParameterList(signature string) (string, bool) {
	open := strings.IndexByte(signature, '(')
	if open < 0 {
		return "", false
	}
	depth := 0
	quote := rune(0)
	escaped := false
	for offset, r := range signature[open:] {
		if quote != 0 {
			if escaped {
				escaped = false
				continue
			}
			if r == '\\' {
				escaped = true
				continue
			}
			if r == quote {
				quote = 0
			}
			continue
		}
		switch r {
		case '\'', '"', '`':
			quote = r
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return signature[open+1 : open+offset], true
			}
			if depth < 0 {
				return "", false
			}
		}
	}
	return "", false
}

func splitExploreParameters(parameters string) ([]string, bool) {
	parts := make([]string, 0, 8)
	start := 0
	depth := 0
	quote := rune(0)
	escaped := false
	for offset, r := range parameters {
		if quote != 0 {
			if escaped {
				escaped = false
				continue
			}
			if r == '\\' {
				escaped = true
				continue
			}
			if r == quote {
				quote = 0
			}
			continue
		}
		switch r {
		case '\'', '"', '`':
			quote = r
		case '(', '[', '{', '<':
			depth++
		case ')', ']', '}', '>':
			depth--
			if depth < 0 {
				return nil, false
			}
		case ',':
			if depth == 0 {
				parts = append(parts, strings.TrimSpace(parameters[start:offset]))
				start = offset + 1
			}
		}
	}
	if quote != 0 || depth != 0 {
		return nil, false
	}
	if tail := strings.TrimSpace(parameters[start:]); tail != "" {
		parts = append(parts, tail)
	}
	return parts, true
}

func exploreTopLevelAssignment(parameter string) int {
	depth := 0
	quote := byte(0)
	escaped := false
	for index := 0; index < len(parameter); index++ {
		char := parameter[index]
		if quote != 0 {
			if escaped {
				escaped = false
				continue
			}
			if char == '\\' {
				escaped = true
				continue
			}
			if char == quote {
				quote = 0
			}
			continue
		}
		switch char {
		case '\'', '"', '`':
			quote = char
		case '(', '[', '{', '<':
			depth++
		case ')', ']', '}', '>':
			if depth > 0 {
				depth--
			}
		case '=':
			if depth == 0 && (index+1 >= len(parameter) || parameter[index+1] != '>') &&
				(index == 0 || (parameter[index-1] != '=' && parameter[index-1] != '!' && parameter[index-1] != '<' && parameter[index-1] != '>')) {
				return index
			}
		}
	}
	return -1
}

func exploreParameterName(declaration string) string {
	declaration = strings.TrimSpace(declaration)
	if colon := exploreParameterTypeColon(declaration); colon >= 0 {
		declaration = strings.TrimSpace(declaration[:colon])
	}
	fields := strings.Fields(declaration)
	if len(fields) == 0 {
		return ""
	}
	name := strings.Trim(fields[len(fields)-1], "*$&?!.[]{}()")
	name = strings.TrimPrefix(name, "...")
	name = strings.TrimPrefix(name, "$")
	var b strings.Builder
	for _, r := range name {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func exploreParameterTypeColon(declaration string) int {
	for index := len(declaration) - 1; index >= 0; index-- {
		if declaration[index] != ':' || (index > 0 && declaration[index-1] == ':') || (index+1 < len(declaration) && declaration[index+1] == ':') {
			continue
		}
		return index
	}
	return -1
}

func exploreDefaultIdentifier(value string) string {
	var b strings.Builder
	for _, r := range value {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' {
			b.WriteRune(unicode.ToLower(r))
		}
	}
	return b.String()
}

func exploreParameterDefaultByName(defaults []exploreParameterDefault, name string) (exploreParameterDefault, bool) {
	for _, candidate := range defaults {
		if strings.EqualFold(candidate.name, name) {
			return candidate, true
		}
	}
	return exploreParameterDefault{}, false
}

func isNeutralExploreDefault(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	for len(value) >= 2 && value[0] == '(' && value[len(value)-1] == ')' {
		value = strings.TrimSpace(value[1 : len(value)-1])
	}
	switch value {
	case "null", "none", "nil", "undefined", "false", "''", "\"\"", "[]", "{}", "()", "array()":
		return true
	default:
		return false
	}
}

func isConcreteExploreDefault(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || isNeutralExploreDefault(value) {
		return false
	}
	lower := strings.ToLower(value)
	if lower == "true" {
		return true
	}
	if len(value) >= 2 && ((value[0] == '\'' && value[len(value)-1] == '\'') || (value[0] == '"' && value[len(value)-1] == '"')) {
		return strings.TrimSpace(value[1:len(value)-1]) != ""
	}
	number := strings.ReplaceAll(value, "_", "")
	parsed, err := strconv.ParseInt(number, 0, 64)
	return err == nil && parsed != 0
}

func exploreDefaultParameterRelevantTerms(terms map[string]struct{}, name string) bool {
	for _, term := range exploreDefaultIdentifierTerms(name) {
		if len(term) < 4 || exploreGenericDefaultParameterTerm(term) {
			continue
		}
		if _, found := terms[term]; found {
			return true
		}
		if strings.HasSuffix(term, "s") {
			if _, found := terms[strings.TrimSuffix(term, "s")]; found {
				return true
			}
		}
	}
	return false
}

func exploreDefaultIdentifierTerms(name string) []string {
	var terms []string
	var token []rune
	flush := func() {
		if len(token) == 0 {
			return
		}
		terms = append(terms, strings.ToLower(string(token)))
		token = token[:0]
	}
	for _, r := range name {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			flush()
			continue
		}
		if len(token) > 0 && unicode.IsUpper(r) && unicode.IsLower(token[len(token)-1]) {
			flush()
		}
		token = append(token, r)
	}
	flush()
	return terms
}

func exploreGenericDefaultParameterTerm(term string) bool {
	switch term {
	case "arg", "argument", "config", "data", "default", "file", "option", "param", "parameter", "setting", "value":
		return true
	default:
		return false
	}
}
