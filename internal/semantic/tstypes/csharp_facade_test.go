package tstypes

import (
	"testing"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
)

// The facade shape from the field report: a service wraps a repository
// method under the SAME name, calling through an interface-typed field.
// The resolver's caller-receiver fallback used to steal that call for
// the calling method itself — a 0.9 self-loop with no Origin stamp —
// and claimable() then graded the guess at the AST ceiling (the
// DefaultOriginFor backfill maps conf>=0.9 to ast_resolved), so the
// engine computed the right target and threw it away. Field-typed
// evidence must be able to reclaim the site.
func TestCSharp_FacadeSelfLoopIsClaimable(t *testing.T) {
	g, dir := buildFixture(t, map[string]string{
		"A/IFolioArchive.cs": `using System.Collections.Generic;

namespace App.Contracts {
    public interface IFolioArchive {
        IReadOnlyList<int> FetchSealedFolios(int year);
    }
}
`,
		"A/FolioArchive.cs": `using System.Collections.Generic;
using App.Contracts;

namespace App.Data {
    public class FolioArchive : IFolioArchive {
        public IReadOnlyList<int> FetchSealedFolios(int year) {
            return new List<int> { year };
        }
    }
}
`,
		"B/FolioService.cs": `using System.Collections.Generic;
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
}
`,
	})

	callerID := "B/FolioService.cs::FolioService.FetchSealedFolios"
	edges := callEdgesNamed(g, callerID, "FetchSealedFolios")
	if len(edges) != 1 {
		t.Fatalf("expected the one extracted member-call stub, got %v", edges)
	}
	// Simulate the resolver damage exactly as minted by the
	// caller-receiver fallback: bound to the calling method itself,
	// confidence 0.9, Origin never stamped.
	self := edges[0]
	oldTo := self.To
	self.To = callerID
	self.Confidence = 0.9
	self.Origin = ""
	g.ReindexEdge(self, oldTo)

	p := NewProvider(CSharpSpec(), zap.NewNop())
	if _, err := p.Enrich(g, dir); err != nil {
		t.Fatal(err)
	}

	ifaceID := "A/IFolioArchive.cs::IFolioArchive.FetchSealedFolios"
	e := callEdgeTo(g, callerID, ifaceID)
	if e == nil {
		t.Fatalf("field-typed evidence must reclaim the self-stolen site; edges: %v", g.GetOutEdges(callerID))
	}
	assertASTProvenance(t, e, "csharp-types")
	if callEdgeTo(g, callerID, callerID) != nil {
		t.Errorf("the self-loop must be retargeted, not kept alongside the fix")
	}
}

// Guard rail: a bind that carries an explicit ast_resolved stamp — real
// structural evidence, not a backfilled guess — must stay unclaimable
// even when it disagrees with the engine's pick.
func TestCSharp_ExplicitASTResolvedBindStaysUnclaimed(t *testing.T) {
	g, dir := buildFixture(t, map[string]string{
		"A/IFolioArchive.cs": `using System.Collections.Generic;

namespace App.Contracts {
    public interface IFolioArchive {
        IReadOnlyList<int> FetchSealedFolios(int year);
    }
}
`,
		"B/FolioService.cs": `using System.Collections.Generic;
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
}
`,
	})

	callerID := "B/FolioService.cs::FolioService.FetchSealedFolios"
	edges := callEdgesNamed(g, callerID, "FetchSealedFolios")
	if len(edges) != 1 {
		t.Fatalf("expected the one extracted member-call stub, got %v", edges)
	}
	self := edges[0]
	oldTo := self.To
	self.To = callerID
	self.Confidence = 0.9
	self.Origin = graph.OriginASTResolved
	g.ReindexEdge(self, oldTo)

	p := NewProvider(CSharpSpec(), zap.NewNop())
	if _, err := p.Enrich(g, dir); err != nil {
		t.Fatal(err)
	}

	if e := callEdgeTo(g, callerID, callerID); e == nil {
		t.Fatalf("an explicitly ast_resolved bind must not be retargeted; edges: %v", g.GetOutEdges(callerID))
	}
}
