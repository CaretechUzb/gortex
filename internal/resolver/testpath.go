package resolver

import "github.com/zzet/gortex/internal/testpath"

// isTestFilePath reports whether a source path follows a recognised test
// convention.
//
// PURPOSE: let the Temporal orphan detector drop dispatches that
// originate in test fixtures (the dominant broken_dispatch false
// positive) without depending on Node.Meta test flags — which are not
// re-stamped on the incremental-reindex path.
//
// RATIONALE: the convention table lives in the stdlib-only leaf
// internal/testpath. The resolver cannot import the indexer package
// (indexer → resolver is the established import direction; the reverse
// would be a cycle), so the predicate used to be duplicated here — and the
// copies drifted. Both now delegate to the one definition.
//
// KEYWORDS: test-file, predicate, temporal, broken_dispatch, no-cycle
func isTestFilePath(path string) bool { return testpath.IsTestFile(path) }
