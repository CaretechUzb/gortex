package languages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// callEdgesFrom returns the EdgeCalls out of fromID whose unresolved
// target names method (member form `unresolved::*.Name` or plain
// `unresolved::Name`).
func callEdgesFrom(edges []*graph.Edge, fromID, method string) []*graph.Edge {
	var out []*graph.Edge
	for _, ed := range edges {
		if ed.From != fromID || ed.Kind != graph.EdgeCalls {
			continue
		}
		if ed.To == "unresolved::*."+method || ed.To == "unresolved::"+method {
			out = append(out, ed)
		}
	}
	return out
}

// A call that spells explicit type arguments parses its invoked name as a
// `generic_name`, not an `identifier`. The call-site query used to require
// an identifier in every position, so EVERY generic invocation — the
// dominant shape in .NET (`GetRequiredService<T>()`,
// `AddSingleton<TI, TImpl>()`, `Deserialize<T>()`) — matched no pattern and
// produced no call edge at all. Not an unresolved stub: nothing. That made
// a heavily-called generic method indistinguishable from dead code.
//
// Each row pairs a call shape written WITHOUT type arguments against the
// same shape written with them. The pair — not an absolute expectation —
// is the assertion: whatever a shape earns plainly, it must still earn
// when the invoked name is generic. That keeps the test honest about
// shapes the extractor types weakly for unrelated reasons, while still
// failing loudly if type arguments cost an edge or a stamp.
func TestCSharpExtractor_GenericInvocationEmitsCallEdge(t *testing.T) {
	src := []byte(`namespace App {
    public interface IBox { }

    public class Registry {
        public T Resolve<T>() { return default(T); }
        public void Resolve() { }
    }

    public class Base {
        public virtual T Make<T>() { return default(T); }
        public virtual void Make() { }
    }

    public class Runner : Base {
        public void LocalPlain() { Registry r = new Registry(); r.Resolve(); }
        public void LocalGeneric() { Registry r = new Registry(); r.Resolve<IBox>(); }

        public void CondPlain(Registry r) { r?.Resolve(); }
        public void CondGeneric(Registry r) { r?.Resolve<IBox>(); }

        public void SelfPlain() { this.Helper(); }
        public void SelfGeneric() { this.Helper<IBox>(); }

        public void BasePlain() { base.Make(); }
        public void BaseGeneric() { base.Make<IBox>(); }

        public void BarePlain() { Helper(); }
        public void BareGeneric() { Helper<IBox>(); }

        public void Helper() { }
        public T Helper<T>() { return default(T); }
    }
}
`)
	e := NewCSharpExtractor()
	result, err := e.Extract("App.cs", src)
	require.NoError(t, err)

	for _, row := range []struct{ shape, plain, generic, method string }{
		{"typed local receiver", "LocalPlain", "LocalGeneric", "Resolve"},
		{"null-conditional receiver", "CondPlain", "CondGeneric", "Resolve"},
		{"this-qualified", "SelfPlain", "SelfGeneric", "Helper"},
		{"base-qualified", "BasePlain", "BaseGeneric", "Make"},
		{"receiverless", "BarePlain", "BareGeneric", "Helper"},
	} {
		t.Run(row.shape, func(t *testing.T) {
			plain := callEdgesFrom(result.Edges, "App.cs::Runner."+row.plain, row.method)
			generic := callEdgesFrom(result.Edges, "App.cs::Runner."+row.generic, row.method)
			require.Len(t, plain, 1, "control: the non-generic call emits one edge")
			require.Len(t, generic, 1,
				"explicit type arguments must not cost the call edge")
			assert.Equal(t, plain[0].To, generic[0].To,
				"both spellings name the same callee")
			assert.Equal(t, plain[0].Meta["receiver_type"], generic[0].Meta["receiver_type"],
				"explicit type arguments must not cost the receiver stamp")
		})
	}
}
