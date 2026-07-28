package graph

// IsContentNode reports whether n is a CONTENT section node — a KindDoc
// chunk tagged data_class="content" (text / pdf / pptx / xlsx section
// bodies). Content bodies are indexed in the dedicated content store
// (ContentSearcher), never the symbol search, and are excluded from the
// code-oriented analysis passes — so this predicate is the single place
// every package agrees on what "content" means. Markdown prose (KindDoc
// without data_class=content) and data assets (data_class="data") are NOT
// content and keep their existing treatment.
func IsContentNode(n *Node) bool {
	if n == nil || n.Kind != KindDoc || n.Meta == nil {
		return false
	}
	dc, _ := n.Meta["data_class"].(string)
	return dc == "content"
}

// NonContentNodeReader is an optional store capability: a cheap (SQL-level
// on the disk backend) enumeration of a repo's NON-content nodes, so the
// code-oriented passes (search-index build, embedding, language detection)
// never materialise a content-heavy repo's hundreds of thousands of content
// sections just to iterate past them.
type NonContentNodeReader interface {
	GetRepoNonContentNodes(repoPrefix string) []*Node
}

// ContentNodeReader is the inverse projection used by the content-link pass.
// The repository predicate is EXACT — an empty prefix selects only nodes whose
// RepoPrefix is itself empty — and implementations must push data_class=content
// into the backend.
//
// NOTE the asymmetry with NonContentNodeReader above: the two are siblings in
// this file and their empty-prefix argument means OPPOSITE things. "" is a
// WILDCARD for GetRepoNonContentNodes ("every repo") and an EXACT MATCH for
// GetRepoContentNodes ("the repo whose prefix is empty"). Neither is wrong —
// the code passes want every repo, the content-link pass wants one — but
// reading either signature as a guide to the other silently inverts the scope.
type ContentNodeReader interface {
	GetRepoContentNodes(repoPrefix string) []*Node
}

// GetRepoNonContentNodes implements NonContentNodeReader without allocating a
// graph-wide snapshot. Empty prefix is a WILDCARD — it keeps the historical
// "all repos" semantics used by global code/search passes — while non-empty
// prefixes use the compact bucket. Do not "tighten" the empty case to an exact
// RepoPrefix == "" match: that would silently empty every global pass built on
// it rather than failing.
func (g *Graph) GetRepoNonContentNodes(repoPrefix string) []*Node {
	var out []*Node
	for _, s := range g.shards {
		s.mu.RLock()
		if repoPrefix == "" {
			for _, n := range s.nodes {
				if n != nil && !IsContentNode(n) {
					out = append(out, n)
				}
			}
		} else {
			for _, n := range s.byRepo[repoPrefix] {
				if n != nil && !IsContentNode(n) {
					out = append(out, n)
				}
			}
		}
		s.mu.RUnlock()
	}
	return out
}

// GetRepoContentNodes implements ContentNodeReader with the EXACT repository
// predicate — unlike its GetRepoNonContentNodes sibling above, "" here selects
// only nodes whose own RepoPrefix is empty, not every repo. That branch is
// live: the standalone Indexer never sets a prefix (see GetRepoNodes).
// Empty-prefix nodes live outside byRepo, so the case reads shard maps
// directly and retains only CONTENT sections.
func (g *Graph) GetRepoContentNodes(repoPrefix string) []*Node {
	var out []*Node
	for _, s := range g.shards {
		s.mu.RLock()
		if repoPrefix == "" {
			for _, n := range s.nodes {
				if n != nil && n.RepoPrefix == "" && IsContentNode(n) {
					out = append(out, n)
				}
			}
		} else {
			for _, n := range s.byRepo[repoPrefix] {
				if n != nil && IsContentNode(n) {
					out = append(out, n)
				}
			}
		}
		s.mu.RUnlock()
	}
	return out
}

var (
	_ NonContentNodeReader = (*Graph)(nil)
	_ ContentNodeReader    = (*Graph)(nil)
)

// RepoCodeNodes returns repoPrefix's non-content projection. The disk backend
// filters in SQL so content-heavy repositories never ship their sections into
// memory; the in-memory backend filters directly while holding shard locks.
// Empty repoPrefix means "all repos".
func RepoCodeNodes(s Store, repoPrefix string) []*Node {
	return s.GetRepoNonContentNodes(repoPrefix)
}
