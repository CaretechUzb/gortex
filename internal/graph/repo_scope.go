package graph

// SharedAcrossRepos reports whether n is a rendezvous point that every
// repository referencing it shares, rather than a symbol one repository
// owns. A repo-narrow (or project-narrow) predicate must admit such a
// node regardless of the RepoPrefix stamped on it.
//
// A contract node's ID is global by construction — `ws::pointerup`,
// `http::GET::/orders`, `di::Clock` — because matching a provider in one
// repository to a consumer in another is the entire point of it. Node
// identity is the ID, so the first repo to mint the node also lends it
// its RepoPrefix; that column records mint order, not ownership. A
// narrow that honours it therefore hides the node from every OTHER repo
// whose symbols point at it, and those symbols keep their `consumes` /
// `provides` edges into a node the caller can no longer see.
//
// Two checkouts of one repository make the arbitrariness plain: the code
// is identical, so mint order alone decides which side can see its own
// contracts. Measured on one Odoo submodule tracked at two branches,
// 1,805 edges ran from the losing checkout's symbols into contract nodes
// stamped with its sibling's prefix.
//
// Contract BRIDGE nodes are deliberately NOT shared: their IDs carry the
// scope (`bridge::<workspace>::<project>::…`), so they are per-scope by
// construction and honouring their prefix is correct. The same holds for
// every ordinary symbol node.
func SharedAcrossRepos(n *Node) bool {
	return n != nil && n.Kind == KindContract
}
