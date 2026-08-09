package tstypes

import (
	"context"
	"fmt"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// benchCorpus stages a spool shaped like a large production C# repo:
// calls dominate every file, a third of files declare inheritance, aliases
// are absent entirely — the class-skew the partitioned walk exploits.
func benchCorpus(b *testing.B, files int) *factSpool {
	b.Helper()
	spool, err := newFactSpool()
	if err != nil {
		b.Fatal(err)
	}
	staged := make([]stagedFileFacts, 0, files)
	for i := 0; i < files; i++ {
		facts := &fileFacts{
			file: fmt.Sprintf("src/dir%02d/File%04d.cs", i%16, i), repoPrefix: "bench",
			imports: []Import{{Local: "Sys"}, {Local: "Col"}},
		}
		if i%3 == 0 {
			facts.supers = []superFact{{typeName: "T", superName: "Base", kind: graph.EdgeExtends, line: 1}}
		}
		for m := 0; m < 5; m++ {
			facts.metas = append(facts.metas, metaFact{key: "return_type", value: "T", owner: "T", name: fmt.Sprintf("M%d", m), line: m})
		}
		for c := 0; c < 40; c++ {
			facts.calls = append(facts.calls, callFact{
				line: c, method: fmt.Sprintf("Call%d", c), recvType: "T",
				recvChain: &callFact{line: c, method: "Inner", recvIdent: "x"},
			})
		}
		record, err := stageFileFacts(facts)
		if err != nil {
			b.Fatal(err)
		}
		staged = append(staged, record)
	}
	if err := spool.appendFiles(staged); err != nil {
		b.Fatal(err)
	}
	return spool
}

// legacyFourPhaseWalk models the pre-partition read pattern: four full
// walks, each decoding every class of every file. Query granularity is
// per file rather than the old per-32-file page scan, so treat the gap
// as decode volume (~4x) plus point-query overhead, not decode alone.
func legacyFourPhaseWalk(b *testing.B, spool *factSpool) int {
	total := 0
	for phase := 0; phase < 4; phase++ {
		after := ""
		for {
			page, last, err := spool.pageFiles(context.Background(), after)
			if err != nil {
				b.Fatal(err)
			}
			if len(page) == 0 {
				break
			}
			for _, stub := range page {
				rows, err := spool.db.Query(`SELECT class,payload FROM file_facts WHERE file_path = ?`, stub.file)
				if err != nil {
					b.Fatal(err)
				}
				for rows.Next() {
					var class int
					var payload []byte
					if err := rows.Scan(&class, &payload); err != nil {
						b.Fatal(err)
					}
					if err := unmarshalClassPayload(stub, factClass(class), payload); err != nil {
						b.Fatal(err)
					}
				}
				_ = rows.Close()
				total += len(stub.calls) + len(stub.supers) + len(stub.metas) + len(stub.aliases)
			}
			after = last
		}
	}
	return total
}

func partitionedFourPhaseWalk(b *testing.B, spool *factSpool) int {
	total := 0
	for _, class := range []factClass{classSupers, classMetas, classAliases, classCalls} {
		after := ""
		for {
			page, last, _, err := spool.pageClass(context.Background(), class, after)
			if err != nil {
				b.Fatal(err)
			}
			if len(page) == 0 {
				break
			}
			for _, facts := range page {
				total += len(facts.calls) + len(facts.supers) + len(facts.metas) + len(facts.aliases)
			}
			after = last
		}
	}
	return total
}

func BenchmarkFourPhaseWalkLegacyShape(b *testing.B) {
	spool := benchCorpus(b, 512)
	defer spool.close()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		legacyFourPhaseWalk(b, spool)
	}
}

func BenchmarkFourPhaseWalkPartitioned(b *testing.B) {
	spool := benchCorpus(b, 512)
	defer spool.close()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		partitionedFourPhaseWalk(b, spool)
	}
}
