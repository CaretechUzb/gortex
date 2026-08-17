package mcp

import (
	"strings"
	"testing"
)

func TestTaskNamedLeadingFileRowSeatsPrimaryOverRankFill(t *testing.T) {
	rows := []localizationDigestRow{
		{ID: "repo/errors.go::ErrorSink.register", Name: "register", QualName: "ErrorSink.register", Kind: "method", File: "repo/errors.go", Line: 12},
		{ID: "repo/socket.go::PacketSocket.open", Name: "open", QualName: "PacketSocket.open", Kind: "method", File: "repo/socket.go", Line: 28},
		{ID: "repo/publisher.go::DatagramPublisher.start", Name: "start", QualName: "DatagramPublisher.start", Kind: "method", File: "repo/publisher.go", Line: 62, supportingOnly: true, leadingFileDepth: true},
		{ID: "repo/socket.go::PacketSocket.close", Name: "close", QualName: "PacketSocket.close", Kind: "method", File: "repo/socket.go", Line: 52},
		{ID: "repo/socket.go::PacketSocket.send", Name: "send", QualName: "PacketSocket.send", Kind: "method", File: "repo/socket.go", Line: 60},
		{ID: "repo/cube.go::CubeSink.connect", Name: "connect", QualName: "CubeSink.connect", Kind: "method", File: "repo/cube.go", Line: 73},
		{ID: "repo/header.go::HeaderCodec.encode", Name: "encode", QualName: "HeaderCodec.encode", Kind: "method", File: "repo/header.go", Line: 91},
	}
	response := renderLocalizationFinalResponseForTask(
		"undefined constant while starting DatagramPublisher", nil, rows)
	want := "- PRIMARY — repo/publisher.go:62 — repo/publisher.go::DatagramPublisher.start"
	if !strings.Contains(response, want) {
		t.Fatalf("task-named leading-file row was not seated:\n%s", response)
	}
}

func TestTaskAlignedSeatsExtendWithoutDisplacingRankedRows(t *testing.T) {
	rows := []localizationDigestRow{
		{ID: "repo/pipeline.go::Pipeline.ingest", Name: "ingest", QualName: "Pipeline.ingest", Kind: "method", File: "repo/pipeline.go", Line: 10},
		{ID: "repo/journal.go::JournalWriter.append", Name: "append", QualName: "JournalWriter.append", Kind: "method", File: "repo/journal.go", Line: 20},
		{ID: "repo/journal.go::JournalWriter.seal", Name: "seal", QualName: "JournalWriter.seal", Kind: "method", File: "repo/journal.go", Line: 34},
		{ID: "repo/journal.go::JournalWriter.compact", Name: "compact", QualName: "JournalWriter.compact", Kind: "method", File: "repo/journal.go", Line: 48},
		{ID: "repo/journal.go::JournalWriter.replayTail", Name: "replayTail", QualName: "JournalWriter.replayTail", Kind: "method", File: "repo/journal.go", Line: 61},
		{ID: "repo/journal.go::JournalWriter.truncate", Name: "truncate", QualName: "JournalWriter.truncate", Kind: "method", File: "repo/journal.go", Line: 75},
	}
	response := renderLocalizationFinalResponseForTask(
		"journal overflow: append, seal, compact, replayTail and truncate misbehave under JournalWriter load", nil, rows)
	// The ranked lead keeps its seat even though it is the only row the task
	// does not name; the sixth row rides an extra seat instead.
	for _, want := range []string{
		"- PRIMARY — repo/pipeline.go:10 — repo/pipeline.go::Pipeline.ingest",
		"- PRIMARY — repo/journal.go:75 — repo/journal.go::JournalWriter.truncate",
	} {
		if !strings.Contains(response, want) {
			t.Fatalf("expected %q as primary:\n%s", want, response)
		}
	}
	if got := strings.Count(response, "- PRIMARY —"); got != localizationFinalResponsePrimaryLimit+1 {
		t.Fatalf("primary rows = %d, want %d:\n%s", got, localizationFinalResponsePrimaryLimit+1, response)
	}
}

func TestTaskMentionSeatsWhileScanLanesStaySupporting(t *testing.T) {
	rows := []localizationDigestRow{
		{ID: "repo/loop.go::EventLoop.tick", Name: "tick", QualName: "EventLoop.tick", Kind: "method", File: "repo/loop.go", Line: 9},
		{ID: "repo/queue.go::WorkQueue.drain", Name: "drain", QualName: "WorkQueue.drain", Kind: "method", File: "repo/queue.go", Line: 21},
		{ID: "repo/state.go::StateCache.load", Name: "load", QualName: "StateCache.load", Kind: "method", File: "repo/state.go", Line: 33},
		{ID: "repo/codec.go::RingBufferCodec.flushSegment", Name: "flushSegment", QualName: "RingBufferCodec.flushSegment", Kind: "method", File: "repo/codec.go", Line: 47,
			Provenance: localizationProvenanceTaskMention},
		{ID: "repo/codec.go::RingBufferCodec.reset", Name: "reset", QualName: "RingBufferCodec.reset", Kind: "method", File: "repo/codec.go", Line: 68,
			Provenance: localizationProvenanceDirectAdjacency},
		{ID: "repo/codec.go::RingBufferCodec.scanTail", Name: "scanTail", QualName: "RingBufferCodec.scanTail", Kind: "method", File: "repo/codec.go", Line: 84,
			Provenance: localizationProvenanceBodyMention, supportingOnly: true},
	}
	response := renderLocalizationFinalResponseForTask(
		"overflow inside RingBufferCodec during flush", nil, rows)
	seated := "- PRIMARY — repo/codec.go:47 — repo/codec.go::RingBufferCodec.flushSegment"
	if !strings.Contains(response, seated) {
		t.Fatalf("task-mention row was not seated:\n%s", response)
	}
	for _, blocked := range []string{
		"- PRIMARY — repo/codec.go:68 — repo/codec.go::RingBufferCodec.reset",
		"- PRIMARY — repo/codec.go:84 — repo/codec.go::RingBufferCodec.scanTail",
	} {
		if strings.Contains(response, blocked) {
			t.Fatalf("scan-lane row claimed a primary seat:\n%s", response)
		}
	}
}

func TestLocalizationTaskNamesIdentifierWholeWord(t *testing.T) {
	cases := []struct {
		task, identifier string
		minRunes         int
		want             bool
	}{
		{"w.Written() is true after headers", "Written", 5, true},
		{"constructing SyslogUdpHandler fails", "SyslogUdpHandler", 6, true},
		{"a task about starting the daemon", "start", 5, false},
		{"the writer is written to disk", "Written", 5, false},
		{"getStorage returns the persist storage", "Storage", 5, false},
		{"Flush()", "Flush", 5, true},
	}
	for _, tc := range cases {
		if got := localizationTaskNamesIdentifier(tc.task, tc.identifier, tc.minRunes); got != tc.want {
			t.Fatalf("localizationTaskNamesIdentifier(%q, %q) = %v, want %v",
				tc.task, tc.identifier, got, tc.want)
		}
	}
}

func TestTaskNamesRowIgnoresNamespaceAndRepoSegments(t *testing.T) {
	task := "Localize bug: in the pre-fix Humanizer checkout, Taxes is singularized wrongly"
	namespaceRow := localizationDigestRow{
		ID:       "humanizer-1499/src/A.cs::src.Humanizer.TimeSpanHumanizeExtensions.GetNormalCaseTimeAsInteger",
		Name:     "GetNormalCaseTimeAsInteger",
		QualName: "src.Humanizer.TimeSpanHumanizeExtensions.GetNormalCaseTimeAsInteger",
	}
	if localizationTaskNamesRow(task, namespaceRow) {
		t.Fatalf("a namespace segment named the row")
	}
	repoOwnedType := localizationDigestRow{
		ID:       "humanizer-1499/src/B.cs::src.Humanizer.NumberToWordsExtension",
		Name:     "NumberToWordsExtension",
		QualName: "src.Humanizer.NumberToWordsExtension",
	}
	if localizationTaskNamesRow(task, repoOwnedType) {
		t.Fatalf("the repository namespace named a top-level type")
	}
	declaredOwner := localizationDigestRow{
		ID:       "monolog-1569/src/H.php::SyslogUdpHandler.__construct",
		Name:     "__construct",
		QualName: "SyslogUdpHandler.__construct",
	}
	if !localizationTaskNamesRow("constructing SyslogUdpHandler fails", declaredOwner) {
		t.Fatalf("the declaring owner did not name the row")
	}
}

func TestMergeLocalizationDigestRowEvidenceKeepsStrongestProvenance(t *testing.T) {
	base := localizationDigestRow{ID: "repo/a.go::Parser.scan", File: "repo/a.go"}
	adjacency := base
	adjacency.Provenance = localizationProvenanceDirectAdjacency
	literal := base
	literal.Provenance = localizationProvenanceContentLiteral
	structural := base
	structural.Provenance = localizationProvenanceImplementationTarget

	if got := mergeLocalizationDigestRowEvidence(adjacency, literal).Provenance; got != localizationProvenanceContentLiteral {
		t.Fatalf("literal re-observation lost to adjacency first-write: %q", got)
	}
	if got := mergeLocalizationDigestRowEvidence(literal, adjacency).Provenance; got != localizationProvenanceContentLiteral {
		t.Fatalf("adjacency re-observation erased a literal mark: %q", got)
	}
	if got := mergeLocalizationDigestRowEvidence(literal, structural).Provenance; got != localizationProvenanceContentLiteral {
		t.Fatalf("structural re-observation outranked a literal mark: %q", got)
	}
	if got := mergeLocalizationDigestRowEvidence(adjacency, structural).Provenance; got != localizationProvenanceImplementationTarget {
		t.Fatalf("structural re-observation lost to adjacency first-write: %q", got)
	}
}
