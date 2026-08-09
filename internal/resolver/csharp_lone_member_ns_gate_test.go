package resolver

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// The BCL-shadowing decoy from the field report: every unresolvable LINQ
// `.Where(...)` in the repo latched onto the one indexed method named
// Where — a static helper in a namespace the calling file never imports.
// The locality fallback's lone-member lift binds it at 0.7 ast_inferred,
// and the cross-package guard's lone-definition keep waves it through:
// with no receiver evidence, "only definition in the repo" is the entire
// case for the bind. For C# the keep must also demand the candidate's
// namespace be visible from the calling file — the true target
// (System.Linq's Where) is external and can never out-argue an indexed
// homonym, so an out-of-scope candidate is noise, not signal.
func TestCSharpLoneMember_UnimportedNamespaceReverts(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Decoys/SpanQueries.cs": `namespace Probe.Decoys {
    public static class SpanQueries {
        public static int[] Where(int[] source, int min) {
            return source;
        }
    }
}`,
		"Services/ReportService.cs": `using System.Linq;

namespace App.Services {
    public class ReportService {
        public int CountBig() {
            var rows = Externals.FetchRows();
            var kept = rows.Where(v => v > 1);
            return 0;
        }
    }
}`,
	})
	New(g).ResolveAll()

	from := "Services/ReportService.cs::ReportService.CountBig"
	decoy := "Decoys/SpanQueries.cs::SpanQueries.Where"
	matched := 0
	for _, e := range g.GetOutEdges(from) {
		if e.Kind != graph.EdgeCalls || !strings.HasSuffix(e.To, "Where") {
			continue
		}
		matched++
		require.NotEqual(t, decoy, e.To,
			"an unresolvable .Where() must not bind the lone same-named method in a namespace the file cannot see (conf=%v origin=%q)",
			e.Confidence, e.Origin)
	}
	// Zero matches would pass vacuously on an ID-scheme drift — the
	// reverted placeholder (`unresolved::*.Where`) still counts.
	require.NotZero(t, matched, "no Where call edge found — fixture or ID scheme broke")
}

// Control: the lone-definition keep itself must survive. A file that DOES
// import the candidate's namespace has the corroboration the gate asks
// for, and the grounded 0.7 ast_inferred bind stays.
func TestCSharpLoneMember_ImportedNamespaceKeeps(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Decoys/SpanQueries.cs": `namespace Probe.Decoys {
    public static class SpanQueries {
        public static int[] Where(int[] source, int min) {
            return source;
        }
    }
}`,
		"Services/ReportService.cs": `using Probe.Decoys;

namespace App.Services {
    public class ReportService {
        public int CountBig() {
            var rows = Externals.FetchRows();
            var kept = rows.Where(v => v > 1);
            return 0;
        }
    }
}`,
	})
	New(g).ResolveAll()

	edge := findCallEdge(g, "Services/ReportService.cs::ReportService.CountBig", "Decoys/SpanQueries.cs::SpanQueries.Where")
	require.NotNil(t, edge, "with the namespace imported the lone-member bind must survive the guard")
	require.Equal(t, graph.OriginASTInferred, edge.Origin)
}
