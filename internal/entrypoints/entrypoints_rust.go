package entrypoints

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

const rustAnnoPrefix = "annotation::rust::"

// rustEntryFnAnnos maps a function attribute to the entry-point kind it
// implies. Each names a symbol some runtime other than application code
// calls: an async runtime's generated main, a test harness, a benchmark
// harness, or a foreign runtime reaching in across an FFI boundary.
var rustEntryFnAnnos = map[string]string{
	"tokio::main":       "rust:async-main",
	"actix_web::main":   "rust:async-main",
	"actix_rt::main":    "rust:async-main",
	"async_std::main":   "rust:async-main",
	"rocket::main":      "rust:async-main",
	"test":              "rust:test",
	"tokio::test":       "rust:test",
	"actix_rt::test":    "rust:test",
	"async_std::test":   "rust:test",
	"bench":             "rust:bench",
	"no_mangle":         "rust:ffi-export",
	"wasm_bindgen":      "rust:ffi-export",
	"pyfunction":        "rust:ffi-export",
	"pymethods":         "rust:ffi-export",
	"napi":              "rust:ffi-export",
	"unsafe(no_mangle)": "rust:ffi-export",
}

// rustEntryTypeAnnos maps a type attribute to its entry-point kind. A
// clap derive makes the type the process's argument surface, and a pyo3
// module or class is instantiated by the Python interpreter.
var rustEntryTypeAnnos = map[string]string{
	"pyclass":      "rust:ffi-export",
	"pymodule":     "rust:ffi-export",
	"wasm_bindgen": "rust:ffi-export",
	"napi":         "rust:ffi-export",
}

// detectRust flags the symbols a Rust runtime reaches without any
// in-crate caller. Without it `fn main`, every #[test], and every
// #[no_mangle] FFI export look unreachable to a call-graph walk, so a
// binary crate's entire call tree reads as dead code.
func detectRust(relPath string, nodes []*graph.Node, edges []*graph.Edge) int {
	annos := map[string]map[string]bool{}
	for _, e := range edges {
		if e.Kind != graph.EdgeAnnotated {
			continue
		}
		name, ok := strings.CutPrefix(e.To, rustAnnoPrefix)
		if !ok {
			continue
		}
		if annos[e.From] == nil {
			annos[e.From] = map[string]bool{}
		}
		annos[e.From][name] = true
	}

	count := 0
	for _, n := range nodes {
		switch {
		case isFnOrMethod(n.Kind):
			kind := ""
			for a := range annos[n.ID] {
				if k, ok := rustEntryFnAnnos[a]; ok {
					kind = k
					break
				}
			}
			// Cargo runs `fn main` in a binary or example target; the
			// crate itself never calls it.
			if kind == "" && n.Name == "main" && rustBinaryTarget(relPath) {
				kind = "rust:main"
			}
			// An extern "C" declaration is the foreign half of an FFI
			// boundary — its body lives in another language, so a
			// missing definition is not a missing implementation.
			if kind == "" && n.Meta != nil && n.Meta["external"] == true &&
				n.Meta["is_extern"] == true {
				kind = "rust:ffi-import"
			}
			if kind != "" && stamp(n, kind) {
				count++
			}
		case n.Kind == graph.KindType || n.Kind == graph.KindInterface:
			for a := range annos[n.ID] {
				if k, ok := rustEntryTypeAnnos[a]; ok {
					if stamp(n, k) {
						count++
					}
					break
				}
			}
		}
	}
	return count
}

// rustBinaryTarget reports whether a path is one Cargo builds as an
// executable. Cargo's layout convention is what decides this: src/main.rs
// and src/bin/*.rs are binaries, and examples/ and benches/ are run
// directly too. A `fn main` anywhere else is an ordinary function.
func rustBinaryTarget(relPath string) bool {
	switch {
	case relPath == "main.rs" || strings.HasSuffix(relPath, "/main.rs"):
		return true
	case strings.Contains(relPath, "src/bin/"):
		return true
	case strings.HasPrefix(relPath, "examples/") || strings.Contains(relPath, "/examples/"):
		return true
	case strings.HasPrefix(relPath, "benches/") || strings.Contains(relPath, "/benches/"):
		return true
	case strings.HasPrefix(relPath, "tests/") || strings.Contains(relPath, "/tests/"):
		return true
	}
	return false
}
