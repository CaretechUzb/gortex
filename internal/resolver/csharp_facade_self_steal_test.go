package resolver

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// The facade shape: a service wraps a repository method under the SAME
// name, calling through an interface-typed field the extractor cannot
// type (fields never reach the tenv). The caller-receiver fallback used
// to answer "does the caller's own class declare this name?" — yes — and
// bind `_archive.FetchSealedFolios(...)` to the calling method ITSELF, a
// 0.9 self-loop with no Origin stamp. The PHP shield above that fallback
// documents the identical failure for `$this->handler->setFormatter()`;
// this pins the C# side.
func TestCSharpFacade_MemberCallDoesNotStealToSelf(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"IFolioArchive.cs": `using System.Collections.Generic;

namespace App.Contracts {
    public interface IFolioArchive {
        IReadOnlyList<int> FetchSealedFolios(int year);
    }
}`,
		"FolioArchive.cs": `using System.Collections.Generic;
using App.Contracts;

namespace App.Data {
    public class FolioArchive : IFolioArchive {
        public IReadOnlyList<int> FetchSealedFolios(int year) {
            return new List<int> { year };
        }
    }
}`,
		"FolioService.cs": `using System.Collections.Generic;
using App.Contracts;

namespace App.Services {
    public class FolioService {
        private readonly IFolioArchive _archive;

        public FolioService(IFolioArchive archive) {
            _archive = archive;
        }

        public IReadOnlyList<int> FetchSealedFolios(int year) {
            var folios = _archive.FetchSealedFolios(year);
            return folios;
        }
    }
}`,
	})
	New(g).ResolveAll()

	from := "FolioService.cs::FolioService.FetchSealedFolios"
	for _, e := range g.GetOutEdges(from) {
		if e.Kind != graph.EdgeCalls || !strings.HasSuffix(e.To, "FetchSealedFolios") {
			continue
		}
		require.NotEqual(t, from, e.To,
			"member call on the injected field must not bind to the calling method itself (got conf=%v origin=%q)",
			e.Confidence, e.Origin)
		// Whatever tier picked a target, the bind must stay below the AST
		// ceiling so semantic enrichment can still claim the site — the
		// old fallback's unstamped 0.9 backfills to ast_resolved and
		// blocks the retarget.
		if e.Origin == "" {
			require.Less(t, e.Confidence, 0.9,
				"an origin-unstamped name-tier bind at >=0.9 masquerades as AST-grade (to=%s)", e.To)
		}
	}
}

// Control: the gate must not disturb same-class binds that carry real
// receiver evidence — `this.Helper()` states the receiver type and binds
// at the exact-type tier.
func TestCSharpFacade_ThisCallStillBindsOwnClass(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Tower.cs": `namespace App {
    public class Tower {
        public int Height() {
            return this.Helper();
        }

        public int Helper() { return 3; }
    }
}`,
	})
	New(g).ResolveAll()

	edge := findCallEdge(g, "Tower.cs::Tower.Height", "Tower.cs::Tower.Helper")
	require.NotNil(t, edge, "this.Helper() must still bind to the own-class member")
	require.GreaterOrEqual(t, edge.Confidence, 0.9)
}
