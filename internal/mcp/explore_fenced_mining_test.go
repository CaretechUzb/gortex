package mcp

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
)

const exploreFencedReportTask = "Chunk upload never retries after a sealed manifest\n\n" +
	"The client gives up on the second attempt instead of retrying the upload, " +
	"so a large transfer has to start again from the beginning.\n\n" +
	"Steps to reproduce are in the attached script; the daemon prints this before it exits:\n\n" +
	"```\n" +
	"fatal: unable to seal chunk manifest\n" +
	"  at ChunkUploader.sealManifest (uploader.go:212)\n" +
	"```\n"

func TestExploreSyntacticAnchorsMineIdentifiersOnlyInsideFence(t *testing.T) {
	// The motivating condition: shaping deletes the block before retrieval, so
	// the identifier reaches ranking only if mining reads it first.
	require.NotContains(t, shapeExploreQuery(exploreFencedReportTask), "sealManifest")

	anchors := exploreSyntacticAnchors(exploreFencedReportTask)
	found := false
	for _, anchor := range anchors {
		if anchor.qualifiedName == "ChunkUploader.sealManifest" {
			found = true
		}
	}
	require.True(t, found, "fenced qualified mention must anchor: %#v", anchors)
}

func TestExploreTaskAnchorsResolveExactNameOnlyInsideFence(t *testing.T) {
	sealed := &graph.Node{
		ID: "src/chunk.c::sealchunk", Name: "sealchunk",
		Kind: graph.KindFunction, FilePath: "src/chunk.c", StartLine: 40, EndLine: 90,
	}
	server := exactNameAnchorServer(t, sealed)
	task := "Uploads stall on retry\n\n```\ntrace: sealchunk returned -1\n```\n"

	anchors := server.exploreTaskAnchors(context.Background(), task, nil, query.QueryOptions{})
	require.Len(t, anchors, 1, "anchors = %#v", anchors)
	require.Equal(t, "sealchunk", anchors[0].compact)
	require.Len(t, anchors[0].exactNodes, 1)

	got := server.exploreExactAnchorCandidate(
		context.Background(), anchors[0], nil, query.QueryOptions{},
		map[string]struct{}{}, map[string]struct{}{},
	)
	require.NotNil(t, got)
	require.Equal(t, sealed.ID, got.Node.ID)
}

func TestExploreFencedMiningRanksMinedTokensAfterProse(t *testing.T) {
	task := "buildContent and flushBuffer both truncate the payload\n\n" +
		"The two writers disagree about the payload limit, so the tail is lost.\n\n" +
		"```\nat RetryQueue.drainBatch (queue.php:44)\n```\n"

	anchors := exploreSyntacticAnchors(task)
	require.Len(t, anchors, exploreSyntacticAnchorMaxTerms)
	require.Equal(t, "buildcontent", anchors[0].compact)
	require.Equal(t, "flushbuffer", anchors[1].compact)
	require.Equal(t, "retryqueue.drainbatch", anchors[2].source,
		"mined tokens may only fill the slots prose left unclaimed: %#v", anchors)
}

func TestExploreFencedMiningRespectsRuneBudget(t *testing.T) {
	filler := strings.Repeat("  status ok for the current batch of pending records\n", 60)
	task := "Batch drain stalls once the queue saturates\n\n" +
		"The drain loop stops making progress while the producer keeps enqueuing.\n\n" +
		"```\n" + filler + "at TailOnlyOwner.tailOnlyMember (tail.go:9)\n```\n"

	lines := exploreFencedCodeLines(task)
	runes := 0
	for _, line := range lines {
		runes += utf8.RuneCountInString(line)
	}
	require.LessOrEqual(t, runes, exploreFencedMiningMaxRunes)
	require.NotContains(t, strings.Join(lines, "\n"), "tailOnlyMember",
		"content past the mining budget must stay unread")
	for _, anchor := range exploreSyntacticAnchors(task) {
		require.NotEqual(t, "tailonlyowner.tailonlymember", anchor.source)
	}
}

func TestExploreQuotedRecallTermsMineFencedLiterals(t *testing.T) {
	terms := exploreQuotedRecallTerms(exploreFencedReportTask)
	require.Contains(t, terms, "unable to seal chunk manifest",
		"an error literal that exists only inside a fence must reach content recall: %#v", terms)
	require.LessOrEqual(t, len(terms), exploreQuotedRecallMaxMinedTerms)
}

func TestExploreQuotedRecallTermsKeepProseTermsAheadOfMinedLiterals(t *testing.T) {
	task := "Registration drops the tenant on rollback\n\n" +
		"The daemon logs \"tenant registry cleared\" before the retry runs, so the entry is lost.\n\n" +
		"```\n" +
		"warn: tenant registry rollback discarded entry\n" +
		"error: retry scheduled after the registry was cleared\n" +
		"debug: reader ignored the pending manifest entry\n" +
		"trace: writer flushed the pending batch to disk\n" +
		"info: registry rebuilt from the durable snapshot\n" +
		"notice: registry verified after snapshot recovery\n" +
		"```\n"
	terms := exploreQuotedRecallTerms(task)
	require.Len(t, terms, exploreQuotedRecallMaxMinedTerms)
	require.Equal(t, "tenant registry cleared", terms[0], "prose literals keep the leading slots: %#v", terms)
	require.Contains(t, terms, "tenant registry rollback discarded entry")
	require.Contains(t, terms, "registry rebuilt from the durable snapshot")
	require.NotContains(t, terms, "registry verified after snapshot recovery",
		"mined literals stop at the total cap: %#v", terms)
}

func TestExploreSyntacticAnchorEvidenceReadyIgnoresMinedAnchors(t *testing.T) {
	targets := []exploreTarget{{
		node: &graph.Node{
			ID: "src/upload.go::retryUpload", Name: "retryUpload",
			Kind: graph.KindFunction, FilePath: "src/upload.go",
		},
		source: "func retryUpload() error { return nil }",
	}}
	anchors := exploreSyntacticAnchors(exploreFencedReportTask)
	require.NotEmpty(t, anchors)
	require.Zero(t, exploreAuthoredSyntacticAnchorCount(anchors))
	require.True(t, exploreSyntacticAnchorEvidenceReady(exploreFencedReportTask, targets),
		"a mined frame may name a third-party symbol, so it must not withhold an answer")

	authored := "ChunkUploader.sealManifest never retries the upload"
	require.Equal(t, 1, exploreAuthoredSyntacticAnchorCount(exploreSyntacticAnchors(authored)))
	require.False(t, exploreSyntacticAnchorEvidenceReady(authored, targets),
		"an anchor the requester spelled still needs covering evidence")
}

func TestExploreQuotedRecallClaimTermsIgnoreMinedLiterals(t *testing.T) {
	require.NotEmpty(t, exploreQuotedRecallTerms(exploreFencedReportTask))
	require.Empty(t, exploreQuotedRecallClaimTerms(exploreFencedReportTask),
		"a mined literal is our inference, so it must not arm the literal-claim terminality gate")

	// A task with no code block keeps one term list, so every gate that reads
	// claims behaves exactly as it did before mining existed.
	prose := `find where the locale "tenant-code" is registered`
	require.Equal(t, exploreQuotedRecallTerms(prose), exploreQuotedRecallClaimTerms(prose))
}

func TestExploreFencedMessageLineRejectsFramesAndFragments(t *testing.T) {
	for _, line := range []string{
		"at com.example.Worker.run(Worker.java:42)",
		"short",
		"++++",
		"|| && ;;",
	} {
		_, ok := exploreFencedMessageLine(line)
		require.False(t, ok, "line %q must not be mined as a literal", line)
	}
	got, ok := exploreFencedMessageLine("fatal: unable to seal chunk manifest")
	require.True(t, ok)
	require.Equal(t, "unable to seal chunk manifest", got)
}

func TestExploreFencedMiningReadsIndentedBlocks(t *testing.T) {
	task := "Manifest sealing fails on the retry path\n\n" +
		"The retry path reports a failure the first attempt never produced.\n\n" +
		"    fatal: unable to seal chunk manifest\n" +
		"    at ChunkUploader.sealManifest (uploader.go:212)\n\n" +
		"    - an indented bullet item that is prose rather than code\n"
	lines := exploreFencedCodeLines(task)
	require.NotEmpty(t, lines)
	require.NotContains(t, strings.Join(lines, "\n"), "indented bullet item")
	require.Contains(t, exploreQuotedRecallTerms(task), "unable to seal chunk manifest")
}
