package resolver

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/zzet/gortex/internal/graph"
)

// Byte extents are recorded for methods, constructors, properties, and
// initialized field declarators. A member kind still WITHOUT them
// (indexer, event accessor) sharing a source line with one that has
// them matches no recorded extent - and answering "" there erased the
// call outright instead of falling back to the line answer (round-6
// finding B3). The extentless member's call must survive line-keyed;
// every extent-carrying member must own its call BYTE-precisely, not
// hand it to whoever shares the line (the pre-owner-widening behavior
// this test silently tolerated).
func TestResolveCSharp_ExtentlessMemberSharingALineKeepsItsCall(t *testing.T) {
	cases := map[string]struct {
		src   string
		owner string // "" = presence only: the owner stays line-keyed
	}{
		"property_shares_line": {src: `namespace App {
 public class BLBag { public int Take() { return 1; } }
 public class BLProp { private BLBag _b = new BLBag();
  public int Q => _b.Take(); public void M() { }
 } }`, owner: "X.cs::BLProp.Q"},
		"indexer_shares_line": {src: `namespace App {
 public class BLBag { public int Take() { return 1; } }
 public class BLIdx { private BLBag _b = new BLBag();
  public int this[int i] => _b.Take(); public void M() { }
 } }`},
		"field_init_shares_line": {src: `namespace App {
 public class BLBag { public int Take() { return 1; } }
 public class BLInit { private BLBag _b = new BLBag();
  private int _n = new BLBag().Take(); public void M() { }
 } }`, owner: "X.cs::BLInit._n"},
		"ctor_and_property_same_line": {src: `namespace App {
 public class BLBag { public int Take() { return 1; } }
 public class BLCtor { private BLBag _b = new BLBag();
  public BLCtor() { } public int Q => _b.Take();
 } }`, owner: "X.cs::BLCtor.Q"},
	}
	for name, tc := range cases {
		g := buildCSharpResolverGraph(t, map[string]string{"X.cs": tc.src})
		found := false
		owners := map[string]int{}
		for _, e := range g.AllEdges() {
			if e != nil && e.Kind == graph.EdgeCalls && strings.Contains(e.To, "Take") {
				found = true
				owners[e.From]++
			}
		}
		assert.True(t, found, "[%s] the member's call must not vanish", name)
		if tc.owner != "" {
			assert.NotZero(t, owners[tc.owner],
				"[%s] the extent-carrying member owns its call byte-precisely, got owners %v", name, owners)
			delete(owners, tc.owner)
			assert.Empty(t, owners,
				"[%s] no line-sharing member may carry a duplicate of the call", name)
		}
	}
}

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
