package resolver

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// Two methods with UNEQUAL line spans can share a physical line - a
// zero-span `public int B(){return 0;}` followed on the same line by the
// opening of A, whose call is split across two lines. Line-keyed
// attribution picked the smaller span (B) and ambiguousAt saw no tie
// (it requires EQUAL spans), so B carried A's call, the shadow refusal
// consulted B's parameter set, and the field's closure filtered the
// parameter-correct implementor away (round-5 finding 4).
//
// Byte-interval attribution is the complete fix: the call's offset lies
// inside A's byte extent and outside B's.
func TestResolveCSharp_UnequalSpanMethodsSharingALine(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"UnequalSpanSameLine.cs": `namespace App {
    public interface SMBox<T> { T Get(int id); }
    public sealed class SMCrate { }
    public sealed class SMWidget { }
    public sealed class SMCrateBox : SMBox<SMCrate> { public SMCrate Get(int id) { return new SMCrate(); } }
    public sealed class SMWidgetBox : SMBox<SMWidget> { public SMWidget Get(int id) { return new SMWidget(); } }
    public sealed class SMFlow {
        private SMBox<SMCrate> _store = new SMCrateBox();
        public int B(){return 0;} public SMWidget A(SMBox<SMWidget> _store, int id) { return _store.Get(
            id); }
    }
}`,
	})
	New(g).ResolveAll()

	assert.Empty(t, callsFrom(g, "UnequalSpanSameLine.cs::SMFlow.B"),
		"B's body is `return 0;` - it owns no call, whatever shares its line")

	const callerID = "UnequalSpanSameLine.cs::SMFlow.A"
	bindMemberCallAtLine(t, g, callerID, "Get", "UnequalSpanSameLine.cs::SMBox.Get")
	ResolveCSharpInterfaceDispatch(g)

	targets := dispatchTargets(g, callerID)
	widgetSurvives := false
	for _, to := range targets {
		if to == "UnequalSpanSameLine.cs::SMWidgetBox.Get" {
			widgetSurvives = true
		}
	}
	assert.True(t, widgetSurvives,
		"the call belongs to A and its receiver is A's SMBox<SMWidget> parameter - at minimum SMWidgetBox.Get must survive in A's fan-out, got %v", targets)
}
