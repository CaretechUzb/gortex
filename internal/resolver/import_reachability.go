package resolver

// importReachableDirs includes direct imports and every transitive re-export.
// File identities control traversal; directory identities control the result.
// A directory can contain several barrels with different outgoing edges.
// Previously completed closures can be reused; this walk never publishes
// partial results, including when it encounters a cycle.
func importReachableDirs(root string, targets map[string]map[string]struct{}, completed map[string][]string) []string {
	seen := make(map[string]struct{})
	dirs := make(map[string]struct{})
	var queue, reachable []string
	addDir := func(dir string) {
		if _, ok := dirs[dir]; !ok {
			dirs[dir] = struct{}{}
			reachable = append(reachable, dir)
		}
	}
	visit := func(file string) {
		if file == "" {
			return
		}
		if _, ok := seen[file]; ok {
			return
		}
		seen[file] = struct{}{}
		if cached, ok := completed[file]; ok {
			for _, dir := range cached {
				addDir(dir)
			}
			return
		}
		addDir(filePathDir(file))
		queue = append(queue, file)
	}
	visit(root)
	for i := 0; i < len(queue); i++ {
		for file := range targets[queue[i]] {
			visit(file)
		}
	}
	return reachable
}
