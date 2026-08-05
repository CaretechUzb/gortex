package indexer

// watcherExcludeFilter adapts the indexer's exclude matcher to the file
// watcher's PathFilter interface, so events under ignored trees are
// dropped inside the backend rather than crossing the event channel, the
// aggregator and the storm counter first.
//
// It is an optimisation, not a security barrier: handleEvent still runs
// the full isExcluded check, which additionally refuses symlinks that
// escape the repo root. This filter deliberately does no I/O so it stays
// cheap enough to run on every raw event.
type watcherExcludeFilter struct{ w *Watcher }

// ShouldInclude reports whether the backend should deliver an event for
// path. A watcher with no matcher includes everything.
func (f *watcherExcludeFilter) ShouldInclude(path string) bool {
	if f == nil || f.w == nil || f.w.excludes == nil {
		return true
	}
	root := f.w.indexer.rootPath
	if root == "" {
		return !f.w.excludes.MatchRel(path)
	}
	return !f.w.excludes.MatchAbs(path, root)
}
