package languages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// Calls inside property accessor bodies used to vanish outright: the
// call-owner lookup admitted only methods and constructors, so a call
// whose enclosing member was a property found no owner and was dropped
// at the funcRanges gate — not misattributed, gone (round-23 catch AC1,
// 3 of 4 sites in the probe cell). Properties now record byte extents
// and join the owner lookup, so every accessor form owns its calls.
//
// Each row is one accessor shape; the assertion is the call edge FROM
// the property node. The expression-bodied METHOD control at the bottom
// pins the one shape that always worked, so a regression that silenced
// calls wholesale cannot pass this test.
func TestCSharpExtractor_AccessorBodyCallsAttributeToTheProperty(t *testing.T) {
	src := []byte(`namespace App {
    public class Crank {
        public int Turn() { return 1; }
        public void Push(int v) { }
        public int Get() { return 2; }
        public void Prime(int v) { }
        public static int Spin() { return 3; }
        public int Feed(System.Func<int> f) { return f(); }
    }

    public class GaugeFace {
        private readonly Crank _crank = new Crank();
        private int _stored;

        public int Reading {
            get { return _crank.Turn(); }
            set { _stored = value + _crank.Turn(); }
        }

        public int Snapshot => _crank.Turn();

        public int Flow {
            get => _crank.Get();
            set => _crank.Push(value);
        }

        public int Sealed {
            get { return _crank.Get(); }
            init { _crank.Prime(value); }
        }

        public static int Global {
            get { return Crank.Spin(); }
        }

        public int Wrapped {
            get { return _crank.Feed(() => _crank.Turn()); }
        }

        public int Plain { get; set; }

        public int Tally() => _crank.Turn();
    }

    public class Box<T> {
        private readonly Crank _crank = new Crank();
        public int Item => _crank.Turn();
    }
}
`)
	e := NewCSharpExtractor()
	result, err := e.Extract("App.cs", src)
	require.NoError(t, err)

	for _, row := range []struct {
		shape, owner, method string
		count                int
	}{
		{"block get", "App.cs::GaugeFace.Reading", "Turn", 2}, // get + set both call Turn
		{"expression-bodied property", "App.cs::GaugeFace.Snapshot", "Turn", 1},
		{"expression-bodied get", "App.cs::GaugeFace.Flow", "Get", 1},
		{"expression-bodied set", "App.cs::GaugeFace.Flow", "Push", 1},
		{"init accessor", "App.cs::GaugeFace.Sealed", "Prime", 1},
		{"static property accessor", "App.cs::GaugeFace.Global", "Spin", 1},
		{"lambda inside accessor", "App.cs::GaugeFace.Wrapped", "Turn", 1},
		{"generic container property", "App.cs::Box.Item", "Turn", 1},
	} {
		t.Run(row.shape, func(t *testing.T) {
			edges := callEdgesFrom(result.Edges, row.owner, row.method)
			assert.Len(t, edges, row.count,
				"accessor-body call must attribute to the property node")
		})
	}

	t.Run("expression-bodied method control", func(t *testing.T) {
		require.Len(t, callEdgesFrom(result.Edges, "App.cs::GaugeFace.Tally", "Turn"), 1,
			"the one site AC1 left alive must stay alive")
	})

	t.Run("auto-property emits no calls", func(t *testing.T) {
		var fromPlain int
		for _, ed := range result.Edges {
			if ed.From == "App.cs::GaugeFace.Plain" && ed.Kind == graph.EdgeCalls {
				fromPlain++
			}
		}
		assert.Zero(t, fromPlain, "an auto-property has no body to own calls")
	})
}

// A property with an initializer spans its `= Call()` bytes, so the
// initializer call belongs to the property node the same way an
// accessor-body call does.
func TestCSharpExtractor_PropertyInitializerCallAttributesToTheProperty(t *testing.T) {
	src := []byte(`namespace App {
    public class Seeder {
        public static int SeedValue() { return 7; }
    }
    public class Config {
        public int Seeded { get; set; } = Seeder.SeedValue();
    }
}
`)
	e := NewCSharpExtractor()
	result, err := e.Extract("App.cs", src)
	require.NoError(t, err)

	assert.Len(t, callEdgesFrom(result.Edges, "App.cs::Config.Seeded", "SeedValue"), 1,
		"property-initializer call must attribute to the property node")
}

// A field initializer is executable code with no method around it. The
// field declarator's own byte span owns the call — per DECLARATOR, so a
// multi-declarator line (`int a = F(), b = G();`) hands each call to
// the field it actually initializes.
func TestCSharpExtractor_FieldInitializerCallAttributesToItsField(t *testing.T) {
	src := []byte(`namespace App {
    public class Seeder {
        public static int A() { return 1; }
        public static int B() { return 2; }
    }
    public class Config {
        private int _lone = Seeder.A();
        private int _first = Seeder.A(), _second = Seeder.B();
        private static readonly int Shared = Seeder.B();
        private int _bare;
    }
}
`)
	e := NewCSharpExtractor()
	result, err := e.Extract("App.cs", src)
	require.NoError(t, err)

	for _, row := range []struct{ shape, owner, method string }{
		{"single declarator", "App.cs::Config._lone", "A"},
		{"first of two declarators", "App.cs::Config._first", "A"},
		{"second of two declarators", "App.cs::Config._second", "B"},
		{"static readonly", "App.cs::Config.Shared", "B"},
	} {
		t.Run(row.shape, func(t *testing.T) {
			assert.Len(t, callEdgesFrom(result.Edges, row.owner, row.method), 1,
				"initializer call must attribute to its own field node")
		})
	}
}

// findCallEdge returns the single EdgeCalls from fromID whose unresolved
// target names method, failing the test on any other cardinality.
func findCallEdge(t *testing.T, edges []*graph.Edge, fromID, method string) *graph.Edge {
	t.Helper()
	found := callEdgesFrom(edges, fromID, method)
	require.Len(t, found, 1, "expected exactly one %s call from %s", method, fromID)
	return found[0]
}

// Inside a set/init accessor `value` is the implicit parameter and its
// type is DECLARED - it is the property type. Seeding it as typed
// receiver evidence stamps receiver_type instead of the old
// receiver_name:value, which invited the resolver to go looking for a
// FIELD named value the accessor cannot mean. When the enclosing type
// really does declare a member named value, the seed is withheld and
// the field evidence stands - same refusal posture as the shadow index.
func TestCSharpExtractor_SetterValueCarriesThePropertyType(t *testing.T) {
	src := []byte(`namespace App {
    public class Crank {
        public int Turn() { return 1; }
        public void Prime(int v) { }
    }
    public class Sink {
        public Crank Feed {
            set { _ = value.Turn(); }
        }
        public Crank Boot {
            get { return null; }
            init { value.Prime(1); }
        }
    }
    public class Clash {
        private readonly Crank value = new Crank();
        public Crank Feed {
            set { _ = value.Turn(); }
        }
    }
}
`)
	e := NewCSharpExtractor()
	result, err := e.Extract("App.cs", src)
	require.NoError(t, err)

	t.Run("set accessor", func(t *testing.T) {
		ed := findCallEdge(t, result.Edges, "App.cs::Sink.Feed", "Turn")
		assert.Equal(t, "Crank", ed.Meta["receiver_type"], "value is declared Crank by the property")
		assert.Nil(t, ed.Meta["receiver_name"], "value is a parameter, not field evidence")
	})
	t.Run("init accessor", func(t *testing.T) {
		ed := findCallEdge(t, result.Edges, "App.cs::Sink.Boot", "Prime")
		assert.Equal(t, "Crank", ed.Meta["receiver_type"])
	})
	t.Run("member named value withholds the seed", func(t *testing.T) {
		ed := findCallEdge(t, result.Edges, "App.cs::Clash.Feed", "Turn")
		assert.Nil(t, ed.Meta["receiver_type"], "a declared value member makes the name ambiguous evidence")
		assert.Equal(t, "value", ed.Meta["receiver_name"], "the field evidence stands")
	})
}
