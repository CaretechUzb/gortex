package languages

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// Awaited receivers must be typed by unwrapping Task<T>: a local assigned
// `await LoadAsync()` and an inline `(await LoadAsync()).X()` both evaluate
// to the T inside the task, not to Task<T> — and previously to nothing at
// all (the tenv walk only knew `new`, and the chain walker collapsed a
// fully-parenthesized receiver to an empty string).
func TestCSharpExtractor_AwaitedReceivers(t *testing.T) {
	src := []byte(`using System.Threading.Tasks;
namespace App {
    public class DrumScale {
        public int Weigh(int id) { return id; }
    }
    public class ScaleLoader {
        public Task<DrumScale> LoadAsync(int id) { return Task.FromResult(new DrumScale()); }
        public async Task<int> WeighLoaded(int id) {
            var scale = await LoadAsync(id);
            return scale.Weigh(id);
        }
        public async Task<int> WeighInline(int id) {
            return (await LoadAsync(id)).Weigh(id);
        }
    }
}
`)
	e := NewCSharpExtractor()
	result, err := e.Extract("Scales.cs", src)
	require.NoError(t, err)

	var localCall, inlineCall *graph.Edge
	for _, ed := range result.Edges {
		if ed.Kind != graph.EdgeCalls || !strings.Contains(ed.To, "Weigh") {
			continue
		}
		switch ed.From {
		case "Scales.cs::ScaleLoader.WeighLoaded":
			localCall = ed
		case "Scales.cs::ScaleLoader.WeighInline":
			inlineCall = ed
		}
	}
	require.NotNil(t, localCall, "scale.Weigh() on an awaited local must emit a call edge")
	require.NotNil(t, localCall.Meta)
	assert.Equal(t, "DrumScale", localCall.Meta["receiver_type"],
		"awaited-local receiver unwraps Task<T>")

	require.NotNil(t, inlineCall, "(await ...).Weigh() must emit a call edge")
	require.NotNil(t, inlineCall.Meta)
	assert.Equal(t, "DrumScale", inlineCall.Meta["receiver_type"],
		"inline awaited receiver unwraps Task<T>")
}

// A local whose initializer merely CONTAINS an await is not the awaited
// value: `var w = (await LoadAsync(id)).Weigh(id)` holds Weigh's return,
// and stamping the awaited T on it would make the resolver confidently
// bind the wrong member — worse than the honest no-stamp.
func TestCSharpExtractor_AwaitedLocalRequiresBareAwaitInitializer(t *testing.T) {
	src := []byte(`using System.Threading.Tasks;
namespace App {
    public class Widget { public int Poke() { return 1; } }
    public class DrumScale { public Widget Weigh(int id) { return new Widget(); } }
    public class ScaleLoader {
        public Task<DrumScale> LoadAsync(int id) { return Task.FromResult(new DrumScale()); }
        public async Task<int> Use(int id) {
            var w = (await LoadAsync(id)).Weigh(id);
            return w.Poke();
        }
    }
}
`)
	e := NewCSharpExtractor()
	result, err := e.Extract("Scales.cs", src)
	require.NoError(t, err)

	for _, ed := range result.Edges {
		if ed.Kind != graph.EdgeCalls || ed.From != "Scales.cs::ScaleLoader.Use" {
			continue
		}
		if !strings.Contains(ed.To, "Poke") || ed.Meta == nil {
			continue
		}
		assert.NotEqual(t, "DrumScale", ed.Meta["receiver_type"],
			"w holds Weigh's return, never the awaited DrumScale")
	}
}

// An unqualified awaited call resolves against the ENCLOSING class (or a
// true free function) — never a same-named method picked off an unrelated
// sibling class in the file. this-qualified awaits name the enclosing
// class explicitly and must resolve the same way.
func TestCSharpExtractor_AwaitedUnqualifiedCallPrefersEnclosingClass(t *testing.T) {
	src := []byte(`using System.Threading.Tasks;
namespace App {
    public class Alpha { public int Foo() { return 1; } }
    public class Beta { public int Foo() { return 2; } }
    public class AService {
        public Task<Alpha> LoadAsync(int id) { return Task.FromResult(new Alpha()); }
    }
    public class BService {
        public Task<Beta> LoadAsync(int id) { return Task.FromResult(new Beta()); }
        public async Task<int> Use(int id) {
            var x = await LoadAsync(id);
            return x.Foo();
        }
        public async Task<int> UseSelf(int id) {
            var y = await this.LoadAsync(id);
            return y.Foo();
        }
    }
}
`)
	e := NewCSharpExtractor()
	result, err := e.Extract("Svc.cs", src)
	require.NoError(t, err)

	byFrom := map[string]*graph.Edge{}
	for _, ed := range result.Edges {
		if ed.Kind == graph.EdgeCalls && strings.Contains(ed.To, "Foo") {
			byFrom[ed.From] = ed
		}
	}

	use := byFrom["Svc.cs::BService.Use"]
	require.NotNil(t, use, "x.Foo() must emit a call edge")
	require.NotNil(t, use.Meta)
	assert.Equal(t, "Beta", use.Meta["receiver_type"],
		"unqualified LoadAsync is BService's, not the sibling AService's")

	useSelf := byFrom["Svc.cs::BService.UseSelf"]
	require.NotNil(t, useSelf, "y.Foo() must emit a call edge")
	require.NotNil(t, useSelf.Meta)
	assert.Equal(t, "Beta", useSelf.Meta["receiver_type"],
		"this.LoadAsync names the enclosing class explicitly")
}

// The unwrap is deliberately conservative: only Task<T>/ValueTask<T>
// produce a type. Everything else must yield no stamp — a guessed
// receiver_type is worse than a missing one.
func TestCSharpExtractor_AwaitedUnwrapRefusalMatrix(t *testing.T) {
	src := []byte(`using System.Threading.Tasks;
namespace App {
    public class DrumScale { public int Weigh(int id) { return id; } }
    public class MyTask<T> { }
    public class Loader {
        public Task PlainAsync(int id) { return Task.CompletedTask; }
        public ValueTask<DrumScale> ValueAsync(int id) { return default; }
        public MyTask<DrumScale> FakeAsync(int id) { return new MyTask<DrumScale>(); }

        public async Task<int> UsePlain(int id) {
            var a = await PlainAsync(id);
            return a.Weigh(id);
        }
        public async Task<int> UseValue(int id) {
            var b = await ValueAsync(id);
            return b.Weigh(id);
        }
        public async Task<int> UseFake(int id) {
            var c = await FakeAsync(id);
            return c.Weigh(id);
        }
    }
}
`)
	e := NewCSharpExtractor()
	result, err := e.Extract("Loader.cs", src)
	require.NoError(t, err)

	recvType := func(from string) any {
		for _, ed := range result.Edges {
			if ed.Kind == graph.EdgeCalls && ed.From == from && strings.Contains(ed.To, "Weigh") {
				if ed.Meta == nil {
					return nil
				}
				return ed.Meta["receiver_type"]
			}
		}
		return nil
	}

	assert.Nil(t, recvType("Loader.cs::Loader.UsePlain"),
		"bare Task has no T - must not stamp")
	assert.Equal(t, "DrumScale", recvType("Loader.cs::Loader.UseValue"),
		"ValueTask<T> unwraps like Task<T>")
	assert.Nil(t, recvType("Loader.cs::Loader.UseFake"),
		"MyTask<T> is not an awaitable wrapper we understand - must not stamp")
}
