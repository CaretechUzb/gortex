package tstypes

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func sampleFacts() *fileFacts {
	return &fileFacts{
		file: "a/b.cs", repoPrefix: "repo",
		imports: []Import{{Local: "Sys"}},
		supers:  []superFact{{typeName: "A", superName: "B", kind: graph.EdgeExtends, line: 1}},
		metas:   []metaFact{{key: "return_type", value: "C", owner: "A", name: "M", line: 2}},
		calls: []callFact{{line: 3, method: "M", recvType: "C",
			recvChain: &callFact{line: 3, method: "N"}, argCount: 2, argKnown: true}},
		// aliases deliberately empty — must be absent from the payload map
	}
}

func TestMarshalClassPayloadsRoundTrip(t *testing.T) {
	src := sampleFacts()
	payloads, err := marshalClassPayloads(src)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := payloads[classAliases]; ok {
		t.Fatal("empty aliases class must be omitted")
	}
	for _, class := range []factClass{classImports, classSupers, classMetas, classCalls} {
		if len(payloads[class]) == 0 {
			t.Fatalf("class %d missing payload", class)
		}
	}
	dst := &fileFacts{file: src.file, repoPrefix: src.repoPrefix}
	for class, payload := range payloads {
		if err := unmarshalClassPayload(dst, class, payload); err != nil {
			t.Fatalf("class %d: %v", class, err)
		}
	}
	if len(dst.imports) != 1 || dst.imports[0].Local != "Sys" {
		t.Fatalf("imports lost: %+v", dst.imports)
	}
	if len(dst.supers) != 1 || dst.supers[0].superName != "B" {
		t.Fatalf("supers lost: %+v", dst.supers)
	}
	if len(dst.metas) != 1 || dst.metas[0].value != "C" {
		t.Fatalf("metas lost: %+v", dst.metas)
	}
	if len(dst.calls) != 1 || dst.calls[0].recvChain == nil || dst.calls[0].recvChain.method != "N" {
		t.Fatalf("calls (incl. chain) lost: %+v", dst.calls)
	}
	if len(dst.aliases) != 0 {
		t.Fatalf("aliases should stay empty: %+v", dst.aliases)
	}
}
