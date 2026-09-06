package tstypes

import (
	"context"
	"sort"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/semantic"
)

// nameRole records WHY a name was collected from a page's facts, and so which
// node kinds a candidate lookup for it can ever hand to a resolver. It is the
// difference between a page-sized working set and a repository-sized one: a
// name that appears only as a call's method name is resolved STRUCTURALLY —
// methodOn walks the receiver type's member_of edges — and is never looked up
// by name at all, so materialising every same-named node in the repository for
// it is pure waste. On an Odoo-shaped tree, where thousands of classes each
// declare `create` / `write` / `_compute_*`, that waste WAS the cost: a single
// page's handful of method names dragged in O(repo) method nodes, which then
// seeded the frontier walk, making the whole-repo pass quadratic in file count.
type nameRole uint8

const (
	// roleType — the name may denote a type / interface / (spec-widened)
	// supertype: resolveTypeNode and resolveSuperNode, through typeCandidates.
	roleType nameRole = 1 << iota
	// roleCallee — the name may denote a free function or method whose declared
	// return type types a receiver: callableReturnType.
	roleCallee
	// roleMember — the name may denote a member reached THROUGH a receiver.
	// Member resolution is structural (methodOn / resolveAlias walk member_of
	// from an already-resolved type), so this role needs no name candidates at
	// all. The one exception is a spec with extension functions, whose
	// cross-file extension nodes carry a member_of edge to a same-file phantom
	// of their receiver and are therefore reachable only by their own name.
	roleMember
)

// roleAll is every role at once — what namedNodes' one-shot compatibility
// lookup materialises, since that path filters by nothing.
const roleAll = roleType | roleCallee | roleMember

// candidateKinds returns the node kinds a name lookup in these roles can ever
// hand to a resolver. Every resolver filters its candidates by kind, so a
// candidate outside this set can never be selected by anything — loading it
// only inflates the page's working set and the frontier walk seeded from it.
func (a *applier) candidateKinds(roles nameRole) map[graph.NodeKind]bool {
	if kinds, ok := a.candidateKindsByRole[roles]; ok {
		return kinds
	}
	kinds := make(map[graph.NodeKind]bool, 4)
	if roles&roleType != 0 {
		for kind := range receiverTypeKinds {
			kinds[kind] = true
		}
		if a.spec != nil {
			for kind := range a.supertypeKinds() {
				kinds[kind] = true
			}
		}
	}
	if roles&roleCallee != 0 {
		kinds[graph.KindFunction] = true
		kinds[graph.KindMethod] = true
	}
	if roles&roleMember != 0 && a.spec != nil && a.spec.ExtensionFunctions {
		kinds[graph.KindMethod] = true
	}
	a.candidateKindsByRole[roles] = kinds
	return kinds
}

// preloadBounded prepares only the current fact page. Cross-file name
// candidates are fetched in one scoped batch and graph adjacency is expanded
// only from the page's own files and the candidates a receiver / supertype can
// actually resolve to. A full-repository language projection is never retained.
func (a *applier) preloadBounded(all []*fileFacts) {
	files := make([]string, 0, len(all))
	fileSet := make(map[string]struct{}, len(all))
	repoNames := make(map[string]map[string]nameRole)
	maxRounds := extendsWalkDepth + 3
	for _, facts := range all {
		if facts == nil {
			continue
		}
		if _, ok := repoNames[facts.repoPrefix]; !ok {
			repoNames[facts.repoPrefix] = make(map[string]nameRole)
		}
		collectFactNames(a.spec, facts, repoNames[facts.repoPrefix])
		if depth := factCallChainDepth(facts); depth+extendsWalkDepth+3 > maxRounds {
			maxRounds = depth + extendsWalkDepth + 3
		}
		if facts.file != "" {
			if _, duplicate := fileSet[facts.file]; !duplicate {
				fileSet[facts.file] = struct{}{}
				files = append(files, facts.file)
			}
		}
	}
	sort.Strings(files)
	a.loadPageFileNodes(files, fileSet, repoNames)
	for _, file := range files {
		a.fileLoaded[file] = true
	}

	// The frontier walk is seeded from the page's OWN symbols plus the
	// cross-file candidates that can carry a member or inheritance frontier —
	// never from every node that merely shares a name with something the page
	// mentions. Only a receiver/supertype-kind node is ever walked: methodOn,
	// typeHasRealMember and resolveAlias all start from an already-resolved
	// type node, and a method candidate reached by name is never one. Seeding
	// the walk from a popular name's other candidates re-expanded the whole
	// repository on every 32-file page.
	seeds := a.pageFileNodeIDs(files)
	for repoPrefix, names := range repoNames {
		seeds = append(seeds, a.preloadNames(repoPrefix, names)...)
	}
	// Each round seeds the frontier walk with only the seeds added since the
	// previous round: adjacency and node loads are gated by their loaded-sets,
	// so re-walking an old seed can never discover anything its first walk
	// did not, and re-passing the full accumulated set each round was pure
	// re-sort/re-scan churn.
	seededFrontier := make(map[string]struct{}, len(seeds))
	scanned := 0
	for round := 0; round < maxRounds; round++ {
		ids := make([]string, 0, len(seeds))
		for _, id := range seeds {
			if _, done := seededFrontier[id]; done {
				continue
			}
			seededFrontier[id] = struct{}{}
			ids = append(ids, id)
		}
		seeds = seeds[:0]
		a.preloadApplicationFrontier(ids)

		// Only nodes hydrated since the previous round can name a type the
		// previous round did not already ask for, and allNodes is append-only —
		// so its tail is exactly the new arrivals. Rescanning the head found
		// nothing and cost one full pass over the working set per round.
		addedName := false
		for _, node := range a.allNodes[scanned:] {
			if node == nil || node.Meta == nil {
				continue
			}
			for _, key := range []string{"return_type", "extension_receiver"} {
				value, _ := node.Meta[key].(string)
				value = a.spec.normalize(value)
				if value == "" {
					continue
				}
				names := repoNames[node.RepoPrefix]
				if names == nil {
					continue
				}
				// A name already collected in another role still needs its
				// TYPE candidates hydrated, so widen the role rather than
				// treating mere presence as loaded.
				if names[value]&roleType == 0 {
					names[value] |= roleType
					addedName = true
				}
			}
		}
		scanned = len(a.allNodes)
		if !addedName {
			break
		}
		for repoPrefix, names := range repoNames {
			seeds = append(seeds, a.preloadNames(repoPrefix, names)...)
		}
	}
}

// pageFileNodeIDs is the page's own symbol set: every node the file projection
// hydrated for the files this page applies. buildIndex reads adjacency for all
// of them and applySuper / applyCall start from the types among them, so they
// always seed the frontier walk.
func (a *applier) pageFileNodeIDs(files []string) []string {
	ids := make([]string, 0, len(files)*8)
	for _, file := range files {
		for _, node := range a.nodesByFile[file] {
			if node != nil {
				ids = append(ids, node.ID)
			}
		}
	}
	return ids
}

// loadPageFileNodes hydrates the page's per-file node groups through the
// pass hot cache. This projection was the one store read the cache's
// node/name/adjacency funnels never covered: every page applier of all four
// phases re-fetched near-identical file sets straight from the store
// (measured at 48–62% of whole-process CPU before the cache existed, and
// still one full store round-trip per page × phase after it). Groups are
// shared node pointers under the same safety model as the nodes funnel, and
// files the store yields nothing for are cached as empty groups.
func (a *applier) loadPageFileNodes(files []string, fileSet map[string]struct{}, repoNames map[string]map[string]nameRole) {
	missing := files
	if a.hot != nil {
		missing = make([]string, 0, len(files))
		for _, file := range files {
			if group, ok := a.hot.getFiles(file); ok {
				for _, node := range group {
					a.rememberNode(node)
				}
				continue
			}
			missing = append(missing, file)
		}
	}
	if len(missing) == 0 {
		return
	}
	if streamer, ok := a.g.(graph.NodesInFilesByKindStreamer); ok {
		yielded := make(map[string]struct{}, len(missing))
		for file, group := range streamer.NodesInFilesByKindSeq(missing, tstypesFileNodeKinds) {
			yielded[file] = struct{}{}
			a.hot.putFiles(file, group)
			for _, node := range group {
				a.rememberNode(node)
			}
		}
		for _, file := range missing {
			if _, ok := yielded[file]; !ok {
				a.hot.putFiles(file, []*graph.Node{})
			}
		}
		return
	}
	if finder, ok := a.g.(graph.NodesInFilesByKindFinder); ok {
		byFile := make(map[string][]*graph.Node, len(missing))
		for _, node := range finder.NodesInFilesByKind(missing, tstypesFileNodeKinds) {
			a.rememberNode(node)
			if node.FilePath != "" {
				byFile[node.FilePath] = append(byFile[node.FilePath], node)
			}
		}
		for _, file := range missing {
			group := byFile[file]
			if group == nil {
				group = []*graph.Node{}
			}
			a.hot.putFiles(file, group)
		}
		return
	}
	// Compatibility-only stores get one scoped projection. Production
	// Graph and SQLite stores implement NodesInFilesByKindFinder.
	for repoPrefix := range repoNames {
		for _, node := range a.g.GetRepoNodes(repoPrefix) {
			if _, wanted := fileSet[node.FilePath]; wanted {
				a.rememberNode(node)
			}
		}
	}
}

// preloadNames hydrates one repo's page-name candidates and returns the IDs of
// those a frontier walk can start from. Each name is filtered to the kinds the
// roles it was collected in can actually consume, so a name that only ever
// appeared as a call's method name (resolved structurally, never by name)
// contributes nothing — which is what keeps a page's working set proportional
// to the page rather than to the repository's most popular identifier.
func (a *applier) preloadNames(repoPrefix string, wanted map[string]nameRole) []string {
	missing := make([]string, 0, len(wanted))
	for name, roles := range wanted {
		if name == "" {
			continue
		}
		key := typeCandidateKey{repoPrefix: repoPrefix, name: name}
		if a.nameLoaded[key]&roles == roles {
			continue
		}
		// A role set that admits no node kind has nothing to fetch. Asking
		// anyway is neither free nor harmless: the batched store lookup still
		// runs for the name, and a.hot.putNames then retains its FULL raw
		// group for the rest of the pass — which for an Odoo-shaped `create`
		// or `write` is every same-named method in the repository, resident in
		// the pass-wide cache. That residency is exactly what the role split
		// exists to remove, so skip the name entirely and leave it UNLOADED;
		// namedNodes tops it up if some later role ever does need it.
		if len(a.candidateKinds(roles|a.nameLoaded[key])) == 0 {
			continue
		}
		missing = append(missing, name)
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)
	frontierKinds := a.candidateKinds(roleType)
	var seeds []string
	// Serve pass-cached name groups first (including cached NEGATIVE groups
	// — common inferred names that bind nothing are re-asked by almost every
	// page). The cache stores the raw store result; the residency filters
	// below run identically on both paths.
	remember := func(name string, group []*graph.Node) {
		key := typeCandidateKey{repoPrefix: repoPrefix, name: name}
		roles := wanted[name] | a.nameLoaded[key]
		kinds := a.candidateKinds(roles)
		for _, node := range group {
			if node == nil || node.RepoPrefix != repoPrefix || !kinds[node.Kind] ||
				!a.spec.allowsCandidateLanguage(node.Language) {
				continue
			}
			a.rememberNode(node)
			if frontierKinds[node.Kind] {
				seeds = append(seeds, node.ID)
			}
		}
		a.nameLoaded[key] = roles
	}
	residue := missing[:0]
	for _, name := range missing {
		if group, ok := a.hot.getNames(typeCandidateKey{repoPrefix: repoPrefix, name: name}); ok {
			remember(name, group)
			continue
		}
		residue = append(residue, name)
	}
	if len(residue) == 0 {
		return seeds
	}
	// Language-scoped candidate hydration: a type name inferred from this
	// spec's grammars can only bind nodes of the spec's languages (plus
	// language-neutral definitions). Repo-only scoping let a mixed repo's
	// host language flood every common name — the SQL path pushes the
	// language predicate into nodes_by_repo_language_name, and the
	// compatibility paths apply the same filter in memory.
	matches := graph.FindNodesByNamesInRepoLanguages(a.g, residue, repoPrefix, a.spec.candidateLanguages())
	for _, name := range residue {
		remember(name, matches[name])
		a.hot.putNames(typeCandidateKey{repoPrefix: repoPrefix, name: name}, matches[name])
	}
	return seeds
}

// collectFactNames records every name a page's facts can ask the graph about,
// tagged with the role it was asked in. The role decides which node kinds the
// candidate lookup materialises; see nameRole.
func collectFactNames(spec *LangSpec, facts *fileFacts, out map[string]nameRole) {
	add := func(name string, role nameRole) {
		if spec != nil {
			name = spec.normalize(name)
		}
		if name != "" {
			out[name] |= role
		}
	}
	for _, imp := range facts.imports {
		add(imp.Local, roleType)
	}
	for _, fact := range facts.supers {
		add(fact.typeName, roleType)
		add(fact.superName, roleType)
	}
	for _, fact := range facts.metas {
		// owner names a receiver type; name is a field, matched same-file by
		// findMember and never looked up across the repository.
		add(fact.owner, roleType)
		add(fact.name, roleMember)
		if fact.key == "return_type" || fact.key == "semantic_type" {
			add(fact.value, roleType)
		}
	}
	for _, fact := range facts.aliases {
		add(fact.typeName, roleType)
		add(fact.trait, roleType)
		// The alias and its target are resolved by methodOn against the using
		// type's own member set — structurally, not by name.
		add(fact.alias, roleMember)
		add(fact.method, roleMember)
	}
	for i := range facts.calls {
		collectCallNames(spec, &facts.calls[i], out)
	}
}

func collectCallNames(spec *LangSpec, fact *callFact, out map[string]nameRole) {
	if fact == nil {
		return
	}
	add := func(name string, role nameRole) {
		if spec != nil {
			name = spec.normalize(name)
		}
		if name != "" {
			out[name] |= role
		}
	}
	// The called member is resolved by walking the RESOLVED receiver type's
	// members (methodOn / resolveAlias / extensionMethod), never through the
	// name index — only a spec with extension functions needs its candidates.
	add(fact.method, roleMember)
	add(fact.recvType, roleType)
	// A bare callee in receiver position is grounded by callableReturnType,
	// which reads the declared return type of a repo-unique function/method.
	add(fact.recvPendingCallee, roleCallee)
	add(fact.recvCallTypeArg, roleType)
	add(fact.recvIdent, roleType)
	collectCallNames(spec, fact.recvChain, out)
}

func factCallChainDepth(facts *fileFacts) int {
	maxDepth := 0
	var depth func(*callFact) int
	depth = func(fact *callFact) int {
		if fact == nil {
			return 0
		}
		return 1 + depth(fact.recvChain)
	}
	for i := range facts.calls {
		if d := depth(&facts.calls[i]); d > maxDepth {
			maxDepth = d
		}
	}
	return maxDepth
}

// coverFiles hydrates the page's per-file node groups (through the pass hot
// cache — warming them for every later phase) and counts covered symbols.
// It replaces the coverage side effect the supers phase carried when every
// file appeared in its walk; the walk itself decodes no facts.
func (a *applier) coverFiles(page []*fileFacts) int {
	files := make([]string, 0, len(page))
	fileSet := make(map[string]struct{}, len(page))
	repoNames := make(map[string]map[string]nameRole)
	for _, facts := range page {
		if facts == nil || facts.file == "" {
			continue
		}
		if _, duplicate := fileSet[facts.file]; duplicate {
			continue
		}
		fileSet[facts.file] = struct{}{}
		files = append(files, facts.file)
		if _, ok := repoNames[facts.repoPrefix]; !ok {
			repoNames[facts.repoPrefix] = make(map[string]nameRole)
		}
	}
	sort.Strings(files)
	a.loadPageFileNodes(files, fileSet, repoNames)
	return a.coveredSymbols(page)
}

func (a *applier) preparePage(all []*fileFacts) []*fileIndex {
	sort.Slice(all, func(i, j int) bool { return all[i].file < all[j].file })
	a.preload(all)
	indexes := make([]*fileIndex, len(all))
	for i, facts := range all {
		indexes[i] = a.buildIndex(facts)
	}
	return indexes
}

func (a *applier) applySupersPage(ctx context.Context, all []*fileFacts, res *semantic.EnrichResult) error {
	indexes := a.preparePage(all)
	for i, facts := range all {
		for _, fact := range facts.supers {
			if err := ctx.Err(); err != nil {
				return err
			}
			a.applySuper(indexes[i], fact, res)
		}
	}
	return nil
}

func (a *applier) applyMetasPage(ctx context.Context, all []*fileFacts, res *semantic.EnrichResult) error {
	indexes := a.preparePage(all)
	for i, facts := range all {
		for _, fact := range facts.metas {
			if err := ctx.Err(); err != nil {
				return err
			}
			a.applyMeta(indexes[i], fact, res)
		}
	}
	return nil
}

func (a *applier) resolveAliasesPage(ctx context.Context, all []*fileFacts) ([]stagedResolvedAlias, error) {
	indexes := a.preparePage(all)
	var staged []stagedResolvedAlias
	for i, facts := range all {
		for _, fact := range facts.aliases {
			if err := ctx.Err(); err != nil {
				return staged, err
			}
			typeNode := indexes[i].types[fact.typeName]
			if typeNode == nil {
				continue
			}
			var traitID string
			if fact.trait != "" {
				trait := a.resolveSuperNode(indexes[i], fact.trait)
				if trait == nil {
					continue
				}
				traitID = trait.ID
			}
			staged = append(staged, stagedResolvedAlias{
				typeID: typeNode.ID, alias: fact.alias, traitID: traitID, method: fact.method,
			})
		}
	}
	return staged, nil
}

func (a *applier) typeNodeIDs() []string {
	ids := make([]string, 0)
	for id, node := range a.nodesByID {
		if node != nil && receiverTypeKinds[node.Kind] {
			ids = append(ids, id)
		}
	}
	return uniqueSortedIDs(ids)
}

func (a *applier) installAliases(records []stagedResolvedAlias) {
	traitIDs := make([]string, 0, len(records))
	for _, record := range records {
		if record.traitID != "" {
			traitIDs = append(traitIDs, record.traitID)
		}
	}
	traits := a.nodes(traitIDs)
	for _, record := range records {
		var trait *graph.Node
		if record.traitID != "" {
			trait = traits[record.traitID]
			if trait == nil {
				continue
			}
		}
		a.aliases[record.typeID] = append(a.aliases[record.typeID], resolvedAlias{
			alias: record.alias, trait: trait, method: record.method,
		})
	}
}

func (a *applier) applyCallsPage(ctx context.Context, all []*fileFacts, aliases []stagedResolvedAlias, res *semantic.EnrichResult) error {
	indexes := a.preparePage(all)
	a.installAliases(aliases)
	for i, facts := range all {
		for _, fact := range facts.calls {
			if err := ctx.Err(); err != nil {
				return err
			}
			a.applyCall(indexes[i], fact, res)
		}
	}
	return nil
}

func (a *applier) pageStats(base factPageStats) factPageStats {
	base.CacheNodes = len(a.nodesByID)
	base.CacheNames = len(a.nodesByName)
	for _, edges := range a.outByID {
		base.CacheEdges += len(edges)
	}
	for _, edges := range a.inByID {
		base.CacheEdges += len(edges)
	}
	return base
}

func (a *applier) coveredSymbols(all []*fileFacts) int {
	count := 0
	for _, facts := range all {
		for _, node := range a.nodesByFile[facts.file] {
			if node != nil && a.languages[node.Language] &&
				node.Kind != graph.KindFile && node.Kind != graph.KindImport {
				count++
			}
		}
	}
	return count
}
