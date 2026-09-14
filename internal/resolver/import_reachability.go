package resolver

// importReachableDirs includes direct imports and every transitive re-export.
// File identities control traversal; directory identities control the result.
// A directory can contain several barrels with different outgoing edges.
func importReachableDirs(roots map[string]struct{}, targets map[string]map[string]struct{}) map[string]struct{} {
	seen := make(map[string]struct{})
	dirs := make(map[string]struct{})
	queue := make([]string, 0, len(roots))
	visit := func(file string) {
		if file == "" {
			return
		}
		if _, ok := seen[file]; ok {
			return
		}
		seen[file] = struct{}{}
		dirs[filePathDir(file)] = struct{}{}
		queue = append(queue, file)
	}
	for file := range roots {
		visit(file)
	}
	for i := 0; i < len(queue); i++ {
		for file := range targets[queue[i]] {
			visit(file)
		}
	}
	return dirs
}
