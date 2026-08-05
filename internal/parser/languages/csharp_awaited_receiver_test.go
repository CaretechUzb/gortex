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
