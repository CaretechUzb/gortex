package languages

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// accessEdges returns the EdgeReads/EdgeWrites edges out of fromID whose
// unresolved target names member.
func accessEdges(result_edges []*graph.Edge, fromID, member string) []*graph.Edge {
	var out []*graph.Edge
	for _, ed := range result_edges {
		if ed.From != fromID || (ed.Kind != graph.EdgeReads && ed.Kind != graph.EdgeWrites) {
			continue
		}
		if strings.HasSuffix(ed.To, "."+member) {
			out = append(out, ed)
		}
	}
	return out
}

// C# property / field / const accesses must emit EdgeReads (EdgeWrites in
// assignment position) like PHP and Go already do — previously they
// produced no edge of any kind, so every C# property answered
// find_usages with its declaration and nothing else.
func TestCSharpExtractor_MemberAccessEdges(t *testing.T) {
	src := []byte(`namespace App {
    public class Plaque {
        public string Title { get; set; }
        public const int Cap = 5;
        public int Chime() { return 1; }
    }
    public class Reader {
        public string Note { get; set; }

        public string Get() { Plaque p = new Plaque(); return p.Title; }
        public void Set() { Plaque p = new Plaque(); p.Title = "x"; }
        public void Bump() { Plaque p = new Plaque(); p.Title += "!"; }
        public string Self() { return this.Note; }
        public int Fixed() { return Plaque.Cap; }
        public string Cond() { Plaque p = new Plaque(); return p?.Title; }
        public int Chained() { Plaque p = new Plaque(); return p.Title.Length; }
        public int Call() { Plaque p = new Plaque(); return p.Chime(); }
        public string FromParam(Plaque q) { return q.Title; }
        public int ParamChain(Plaque q) { return q.Title.Length; }
    }
}
`)
	e := NewCSharpExtractor()
	result, err := e.Extract("Plaque.cs", src)
	require.NoError(t, err)

	get := accessEdges(result.Edges, "Plaque.cs::Reader.Get", "Title")
	require.Len(t, get, 1, "p.Title in return position is one read")
	assert.Equal(t, graph.EdgeReads, get[0].Kind)
	require.NotNil(t, get[0].Meta)
	assert.Equal(t, "Plaque", get[0].Meta["receiver_type"])

	set := accessEdges(result.Edges, "Plaque.cs::Reader.Set", "Title")
	require.Len(t, set, 1, "assignment lhs is one write")
	assert.Equal(t, graph.EdgeWrites, set[0].Kind)
	assert.Equal(t, "Plaque", set[0].Meta["receiver_type"])

	bump := accessEdges(result.Edges, "Plaque.cs::Reader.Bump", "Title")
	require.Len(t, bump, 1, "compound assignment lhs is one write")
	assert.Equal(t, graph.EdgeWrites, bump[0].Kind)

	self := accessEdges(result.Edges, "Plaque.cs::Reader.Self", "Note")
	require.Len(t, self, 1, "this.Note is one read")
	assert.Equal(t, graph.EdgeReads, self[0].Kind)
	assert.Equal(t, "Reader", self[0].Meta["receiver_type"],
		"this-receiver is the enclosing type")

	fixed := accessEdges(result.Edges, "Plaque.cs::Reader.Fixed", "Cap")
	require.Len(t, fixed, 1, "Plaque.Cap static const access is one read")
	assert.Equal(t, graph.EdgeReads, fixed[0].Kind)
	assert.Equal(t, "Plaque", fixed[0].Meta["receiver_type"],
		"a known-type receiver types the static access")

	cond := accessEdges(result.Edges, "Plaque.cs::Reader.Cond", "Title")
	require.Len(t, cond, 1, "p?.Title conditional access is one read")
	assert.Equal(t, graph.EdgeReads, cond[0].Kind)
	assert.Equal(t, "Plaque", cond[0].Meta["receiver_type"])

	chained := accessEdges(result.Edges, "Plaque.cs::Reader.Chained", "Title")
	require.Len(t, chained, 1, "the typed inner link of p.Title.Length is a read of Title")
	assert.Equal(t, graph.EdgeReads, chained[0].Kind)

	call := accessEdges(result.Edges, "Plaque.cs::Reader.Call", "Chime")
	assert.Empty(t, call, "p.Chime() is a call - the call query owns it, no read edge")

	// Parameter receivers are not in the extraction tenv (same contract
	// as calls): the edge still emits, untyped, and receiver binding is
	// the resolver cascade's job.
	param := accessEdges(result.Edges, "Plaque.cs::Reader.FromParam", "Title")
	require.Len(t, param, 1, "a param-receiver access still emits its read edge")
	assert.Equal(t, graph.EdgeReads, param[0].Kind)

	// A chained access on a param receiver is a REAL read even though the
	// receiver is untyped: `q` names a declared parameter, which is
	// value-ness evidence the namespace-noise rule must respect —
	// `System` in `System.Threading.Tasks` names no param or local.
	pchain := accessEdges(result.Edges, "Plaque.cs::Reader.ParamChain", "Title")
	require.Len(t, pchain, 1, "q.Title inside q.Title.Length is a read of Title")
	assert.Equal(t, graph.EdgeReads, pchain[0].Kind)
}

// Dotted namespace qualification parses as nested member accesses in this
// grammar - `System.Threading.Tasks.Task.CompletedTask` must not shower
// the graph with reads of Threading/Tasks. The rule: an access with NO
// receiver evidence emits only at the outermost link of its chain.
func TestCSharpExtractor_MemberAccessNamespaceNoise(t *testing.T) {
	src := []byte(`using System.Threading.Tasks;
namespace App {
    public class Runner {
        public Task Idle() { return System.Threading.Tasks.Task.CompletedTask; }
    }
}
`)
	e := NewCSharpExtractor()
	result, err := e.Extract("Runner.cs", src)
	require.NoError(t, err)

	for _, member := range []string{"Threading", "Tasks", "Task"} {
		assert.Empty(t, accessEdges(result.Edges, "Runner.cs::Runner.Idle", member),
			"namespace-prefix link %q must not emit an access edge", member)
	}
	// The outermost link (CompletedTask) may emit an untyped read - that
	// is the same degraded-but-useful shape PHP emits for unknown
	// receivers, and the resolver's locality cascade handles it.
}
