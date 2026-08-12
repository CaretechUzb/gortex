package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
)

func newLocalizationSourceWindowServer(t testing.TB, rel, content string) (*Server, string) {
	t.Helper()
	root := t.TempDir()
	abs := filepath.Join(root, filepath.FromSlash(rel))
	require.NoError(t, os.MkdirAll(filepath.Dir(abs), 0o755))
	require.NoError(t, os.WriteFile(abs, []byte(content), 0o644))
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	idx := indexer.New(store, parser.NewRegistry(), config.IndexConfig{}, nil)
	idx.SetRootPath(root)
	return &Server{graph: store, indexer: idx}, abs
}

func localizationSourceWindowFixture(lines, matchLine int, literal string) string {
	out := make([]string, lines)
	for index := range out {
		out[index] = fmt.Sprintf("line-%03d", index+1)
	}
	out[matchLine-1] = "register(" + literal + ")"
	return strings.Join(out, "\n") + "\n"
}

func TestLocalizationSourceWindowCentersAndCapsAt201Lines(t *testing.T) {
	const rel = "src/registry.go"
	server, _ := newLocalizationSourceWindowServer(t, rel, localizationSourceWindowFixture(400, 200, "NEEDLE"))
	window := server.localizationSourceWindowForHit(context.Background(), exploreSourceLiteralHit{
		nodeID: "src/registry.go::register", matchPath: rel, matchLine: 200, literal: "NEEDLE",
	}, "src/registry.go::register")

	require.NotNil(t, window)
	require.Equal(t, 100, window.StartLine)
	require.Equal(t, 300, window.EndLine)
	require.Equal(t, localizationSourceWindowMaxLines, window.Lines)
	require.LessOrEqual(t, window.Bytes, localizationSourceWindowMaxBytes)
	require.Contains(t, window.Content, "register(NEEDLE)")
}

func TestLocalizationSourceWindowClampsAtFileStart(t *testing.T) {
	const rel = "src/registry.go"
	server, _ := newLocalizationSourceWindowServer(t, rel, localizationSourceWindowFixture(40, 2, "NEEDLE"))
	window := server.localizationSourceWindowForHit(context.Background(), exploreSourceLiteralHit{
		nodeID: "src/registry.go::register", matchPath: rel, matchLine: 2, literal: "NEEDLE",
	}, "src/registry.go::register")

	require.NotNil(t, window)
	require.Equal(t, 1, window.StartLine)
	require.Equal(t, 40, window.EndLine)
	require.Equal(t, 40, window.Lines)
}

func TestLocalizationSourceWindowShrinksTo12KiBAndRetainsMatch(t *testing.T) {
	const rel = "src/registry.go"
	lines := make([]string, 260)
	for index := range lines {
		lines[index] = fmt.Sprintf("%03d %s", index+1, strings.Repeat("x", 180))
	}
	lines[129] = "register(NEEDLE)"
	server, _ := newLocalizationSourceWindowServer(t, rel, strings.Join(lines, "\n"))
	window := server.localizationSourceWindowForHit(context.Background(), exploreSourceLiteralHit{
		nodeID: "src/registry.go::register", matchPath: rel, matchLine: 130, literal: "NEEDLE",
	}, "src/registry.go::register")

	require.NotNil(t, window)
	require.LessOrEqual(t, window.Bytes, localizationSourceWindowMaxBytes)
	require.True(t, window.Truncated)
	require.Less(t, window.Lines, localizationSourceWindowMaxLines)
	require.LessOrEqual(t, window.StartLine, window.MatchLine)
	require.GreaterOrEqual(t, window.EndLine, window.MatchLine)
	require.Contains(t, window.Content, "register(NEEDLE)")
}

func TestLocalizationSourceWindowOmitsOversizedMatchLine(t *testing.T) {
	const rel = "src/registry.go"
	content := strings.Repeat("x", localizationSourceWindowMaxBytes) + " NEEDLE"
	server, _ := newLocalizationSourceWindowServer(t, rel, content)
	window := server.localizationSourceWindowForHit(context.Background(), exploreSourceLiteralHit{
		nodeID: "src/registry.go::register", matchPath: rel, matchLine: 1, literal: "NEEDLE",
	}, "src/registry.go::register")
	require.Nil(t, window)
}

func TestLocalizationSourceWindowRejectsStaleMatchLine(t *testing.T) {
	const rel = "src/registry.go"
	server, _ := newLocalizationSourceWindowServer(t, rel, localizationSourceWindowFixture(20, 10, "OTHER"))
	window := server.localizationSourceWindowForHit(context.Background(), exploreSourceLiteralHit{
		nodeID: "src/registry.go::register", matchPath: rel, matchLine: 10, literal: "NEEDLE",
	}, "src/registry.go::register")
	require.Nil(t, window)
}

func TestLocalizationSourceWindowValidatesOverlayAwareContent(t *testing.T) {
	const rel = "src/registry.go"
	server, _ := newLocalizationSourceWindowServer(t, rel, localizationSourceWindowFixture(20, 10, "DISK"))
	manager := daemon.NewOverlayManager(0)
	require.NoError(t, manager.RegisterWithID("session", ""))
	require.NoError(t, manager.Push("session", daemon.OverlayFile{
		Path: rel, Content: localizationSourceWindowFixture(20, 10, "OVERLAY"),
	}, nil))
	server.overlays = manager
	ctx := WithSessionID(context.Background(), "session")

	overlayWindow := server.localizationSourceWindowForHit(ctx, exploreSourceLiteralHit{
		nodeID: "src/registry.go::register", matchPath: rel, matchLine: 10, literal: "OVERLAY",
	}, "src/registry.go::register")
	require.NotNil(t, overlayWindow)
	require.Contains(t, overlayWindow.Content, "OVERLAY")

	staleDisk := server.localizationSourceWindowForHit(ctx, exploreSourceLiteralHit{
		nodeID: "src/registry.go::register", matchPath: rel, matchLine: 10, literal: "DISK",
	}, "src/registry.go::register")
	require.Nil(t, staleDisk)
}

func TestLocalizationSourceWindowRedactsSecrets(t *testing.T) {
	const rel = "src/registry.go"
	content := "func register() {\n" +
		"  token := \"ghp_123456789012345678901234567890123456\"\n" +
		"  register(NEEDLE)\n" +
		"}\n"
	server, _ := newLocalizationSourceWindowServer(t, rel, content)
	window := server.localizationSourceWindowForHit(context.Background(), exploreSourceLiteralHit{
		nodeID: "src/registry.go::register", matchPath: rel, matchLine: 3, literal: "NEEDLE",
	}, "src/registry.go::register")

	require.NotNil(t, window)
	require.True(t, window.SecretsRedacted)
	require.NotContains(t, window.Content, "ghp_123456789012345678901234567890123456")
	require.Contains(t, window.Content, "redacted")
}

func TestLocalizationSourceWindowOmitsRedactedMatchLiteral(t *testing.T) {
	const rel = "src/registry.go"
	const secret = "ghp_123456789012345678901234567890123456"
	server, _ := newLocalizationSourceWindowServer(t, rel, "register("+secret+")\n")
	window := server.localizationSourceWindowForHit(context.Background(), exploreSourceLiteralHit{
		nodeID: "src/registry.go::register", matchPath: rel, matchLine: 1, literal: secret,
	}, "src/registry.go::register")
	require.Nil(t, window, "automatic redaction must not leave a window whose visible match vanished")
}

func TestLocalizationSourceWindowCollectorElectsOnlySurvivingTargetDeterministically(t *testing.T) {
	collector := &localizationSourceWindowHitCollector{}
	collector.add(
		exploreSourceLiteralHit{nodeID: "dropped", matchPath: "d.go", matchLine: 2, literal: "x"},
		exploreSourceLiteralHit{nodeID: "kept", rank: 4, ambiguous: true, matchPath: "z.go", matchLine: 8, literal: "x"},
		exploreSourceLiteralHit{nodeID: "kept", rank: 2, matchPath: "a.go", matchLine: 4, literal: "x"},
	)
	targets := []exploreTarget{{node: &graph.Node{ID: "kept"}, sourceLiteral: true}}
	index, hit, ok := collector.elect(targets)
	require.True(t, ok)
	require.Zero(t, index)
	require.Equal(t, "a.go", hit.matchPath)
}

func TestLocalizationSourceWindowCollectorRejectsUnacceptedOwner(t *testing.T) {
	collector := &localizationSourceWindowHitCollector{}
	collector.add(exploreSourceLiteralHit{
		nodeID: "ordinary", matchPath: "ordinary.go", matchLine: 2, literal: "x",
	})
	_, _, ok := collector.elect([]exploreTarget{{node: &graph.Node{ID: "ordinary"}}})
	require.False(t, ok)
}

func TestLocalizationSourceWindowPackingUsesOnlyLeftoverAndPreservesEnvelope(t *testing.T) {
	base := localizationExploreEnvelope{
		Completion: newLocalizationCompletion(true, ""),
		Files:      []string{"src/registry.go"},
		Symbols:    []string{"src/registry.go::register"},
		Evidence: []localizationEvidence{{
			Rank: 1, ID: "src/registry.go::register", Name: "register",
			File: "src/registry.go", Line: 1,
		}},
	}
	window := &localizationSourceWindow{
		Path: "src/registry.go", StartLine: 1, EndLine: 3, MatchLine: 2,
		Content: "before\nregister(NEEDLE)\nafter", AnchorSymbol: "src/registry.go::register",
	}
	window.refreshSize()
	without, err := json.Marshal(base)
	require.NoError(t, err)
	packed := localizationEnvelopePackingSourceWindow(base, window, len(without))
	require.Nil(t, packed.SourceWindow, "a window must not displace ordinary envelope bytes")
	require.Equal(t, base.Evidence, packed.Evidence)
	require.Equal(t, base.Completion, packed.Completion)

	packed = localizationEnvelopePackingSourceWindow(base, window, len(without)+1024)
	require.NotNil(t, packed.SourceWindow)
	require.Equal(t, base.Evidence, packed.Evidence)
	require.Equal(t, base.Completion, packed.Completion)
}

func TestLocalizationSourceWindowShedsBeforeExistingPayload(t *testing.T) {
	envelope := localizationExploreEnvelope{
		Evidence: []localizationEvidence{{ID: "kept", Source: "important"}},
		Outline:  &localizationFileOutline{File: "src/registry.go"},
		SourceWindow: &localizationSourceWindow{
			Path: "src/registry.go", Content: "optional", AnchorSymbol: "kept",
		},
	}
	require.True(t, localizationShedSourceWindow(&envelope))
	require.Nil(t, envelope.SourceWindow)
	require.Equal(t, "important", envelope.Evidence[0].Source)
	require.NotNil(t, envelope.Outline)
}

func TestLocalizationBuilderOmitsWindowBeforeChangingEvidence(t *testing.T) {
	node := &graph.Node{
		ID: "src/registry.go::register", Name: "register", Kind: graph.KindFunction,
		FilePath: "src/registry.go", StartLine: 1, EndLine: 3,
	}
	window := &localizationSourceWindow{
		Path: "src/registry.go", StartLine: 1, EndLine: 1, MatchLine: 1,
		Content:      strings.Repeat("w", localizationSourceWindowMaxBytes),
		AnchorSymbol: node.ID,
	}
	window.refreshSize()
	completion := newLocalizationCompletion(true, "")
	baseline, _, baselineDigest, baselineCompletion := buildLocalizationExploreResultForTaskFinalized(
		completion, "find register", []exploreTarget{{node: node}}, exploreMinBudgetTokens,
	)
	withWindow, _, withDigest, withCompletion := buildLocalizationExploreResultForTaskFinalized(
		completion, "find register", []exploreTarget{{node: node, sourceWindow: window}}, exploreMinBudgetTokens,
	)
	baselineText, ok := singleTextContent(baseline)
	require.True(t, ok)
	withText, ok := singleTextContent(withWindow)
	require.True(t, ok)
	require.Equal(t, baselineText, withText)
	require.Equal(t, baselineDigest, withDigest)
	require.Equal(t, baselineCompletion, withCompletion)
	require.NotContains(t, withText, "source_window")
}
