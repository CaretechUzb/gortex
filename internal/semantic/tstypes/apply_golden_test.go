package tstypes

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/semantic"
)

var updateGolden = flag.Bool("update", false, "rewrite golden snapshots")

type snapEdge struct {
	From, To, Kind, Origin string
	Line                   int
}

type snapMeta struct {
	NodeID, Key, Value string
}

type applySnapshot struct {
	Edges           []snapEdge
	MetaStamps      []snapMeta
	EdgesConfirmed  int
	EdgesAdded      int
	SymbolsCovered  int
	SymbolsTotal    int
	CoveragePercent float64
}

// goldenFixtureFiles is a miniature C# repo shaped like the cases the
// spool must not disturb: a partial type whose base clause lives in one
// part while the calls live in another (the Razor-codebehind shape), a
// local-variable receiver, and a parameter-typed receiver.
func goldenFixtureFiles() map[string]string {
	return map[string]string{
		"widgets/Base.cs": `namespace Widgets
{
    public class WidgetBase
    {
        public void Render() { }
    }
}
`,
		"widgets/Alpha.Part1.cs": `namespace Widgets
{
    public partial class Alpha : WidgetBase
    {
        public Helper Sidekick { get; set; }
    }
}
`,
		"widgets/Alpha.Part2.cs": `namespace Widgets
{
    public partial class Alpha
    {
        public void Draw()
        {
            this.Render();
            var helper = new Helper();
            helper.Assist(null);
        }
    }
}
`,
		"widgets/Helper.cs": `namespace Widgets
{
    public class Helper
    {
        public void Assist(WidgetBase w)
        {
            w.Render();
        }
    }
}
`,
	}
}

func snapshotFrom(g *graph.Graph, res *semantic.EnrichResult) applySnapshot {
	snap := applySnapshot{
		EdgesConfirmed: res.EdgesConfirmed, EdgesAdded: res.EdgesAdded,
		SymbolsCovered: res.SymbolsCovered, SymbolsTotal: res.SymbolsTotal,
		CoveragePercent: res.CoveragePercent,
	}
	for _, e := range g.AllEdges() {
		snap.Edges = append(snap.Edges, snapEdge{From: e.From, To: e.To, Kind: string(e.Kind), Origin: e.Origin, Line: e.Line})
	}
	for _, n := range g.AllNodes() {
		for key, val := range n.Meta {
			if s, ok := val.(string); ok {
				snap.MetaStamps = append(snap.MetaStamps, snapMeta{NodeID: n.ID, Key: key, Value: s})
			}
		}
	}
	sort.Slice(snap.Edges, func(i, j int) bool {
		a, b := snap.Edges[i], snap.Edges[j]
		if a.From != b.From {
			return a.From < b.From
		}
		if a.To != b.To {
			return a.To < b.To
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		return a.Line < b.Line
	})
	sort.Slice(snap.MetaStamps, func(i, j int) bool {
		a, b := snap.MetaStamps[i], snap.MetaStamps[j]
		if a.NodeID != b.NodeID {
			return a.NodeID < b.NodeID
		}
		if a.Key != b.Key {
			return a.Key < b.Key
		}
		return a.Value < b.Value
	})
	return snap
}

// TestApplyGoldenSnapshot pins the full streamed pipeline — parse
// fan-out, fact spool, four-phase apply, coverage accounting — against a
// committed snapshot. Any spool or paging change must reproduce this
// byte-for-byte; regenerate deliberately with -update.
func TestApplyGoldenSnapshot(t *testing.T) {
	g, dir := buildFixture(t, goldenFixtureFiles())
	p := NewProvider(CSharpSpec(), zap.NewNop())
	res, err := p.EnrichRepo(g, "", dir)
	if err != nil {
		t.Fatal(err)
	}
	got := snapshotFrom(g, res)

	goldenPath := filepath.Join("testdata", "golden", "apply_snapshot.json")
	if *updateGolden {
		blob, err := json.MarshalIndent(got, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(goldenPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(goldenPath, blob, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	blob, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden (run with -update first): %v", err)
	}
	var want applySnapshot
	if err := json.Unmarshal(blob, &want); err != nil {
		t.Fatal(err)
	}
	gotBlob, _ := json.Marshal(got)
	wantBlob, _ := json.Marshal(want)
	if string(gotBlob) != string(wantBlob) {
		t.Fatalf("apply snapshot drifted from golden\ngot:  %s\nwant: %s", gotBlob, wantBlob)
	}
}
