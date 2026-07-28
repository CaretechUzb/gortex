package entrypoints

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser/languages"
)

// detectRustFor runs the real Rust extractor over src and then the
// detector, so the test exercises the annotation shape the extractor
// actually emits rather than a hand-built one.
func detectRustFor(t *testing.T, relPath, src string) map[string]*graph.Node {
	t.Helper()
	e := languages.NewRustExtractor()
	result, err := e.Extract(relPath, []byte(src))
	require.NoError(t, err)
	Detect(relPath, "rust", result.Nodes, result.Edges)
	byID := map[string]*graph.Node{}
	for _, n := range result.Nodes {
		byID[n.ID] = n
	}
	return byID
}

// Without a Rust arm, `fn main` was never a live root and a binary
// crate's entire call tree read as dead code.
func TestDetectRust_MainInBinaryTarget(t *testing.T) {
	byID := detectRustFor(t, "src/main.rs", `fn main() { run(); }
fn run() {}
`)
	m := byID["src/main.rs::main"]
	require.NotNil(t, m)
	assert.Equal(t, true, m.Meta[MetaEntryPoint])
	assert.Equal(t, "rust:main", m.Meta[MetaEntryKind])

	assert.NotEqual(t, true, byID["src/main.rs::run"].Meta[MetaEntryPoint],
		"an ordinary function is not an entry point")
}

// A `fn main` in a library module is an ordinary function — only
// Cargo's binary targets are executables.
func TestDetectRust_MainOutsideBinaryTargetIsNotAnEntryPoint(t *testing.T) {
	byID := detectRustFor(t, "src/helpers.rs", `fn main() {}`)
	assert.NotEqual(t, true, byID["src/helpers.rs::main"].Meta[MetaEntryPoint])
}

func TestDetectRust_AsyncMainAndTests(t *testing.T) {
	byID := detectRustFor(t, "src/lib.rs", `#[tokio::main]
async fn main() {}

#[test]
fn it_works() {}

#[tokio::test]
async fn async_works() {}
`)
	assert.Equal(t, "rust:async-main", byID["src/lib.rs::main"].Meta[MetaEntryKind],
		"#[tokio::main] marks an entry point even outside a binary target")
	assert.Equal(t, "rust:test", byID["src/lib.rs::it_works"].Meta[MetaEntryKind])
	assert.Equal(t, "rust:test", byID["src/lib.rs::async_works"].Meta[MetaEntryKind])
}

// An FFI export is called from another language entirely, so nothing in
// the crate refers to it.
func TestDetectRust_FFIExports(t *testing.T) {
	byID := detectRustFor(t, "src/lib.rs", `#[no_mangle]
pub extern "C" fn gx_open() -> i32 { 0 }

#[wasm_bindgen]
pub fn greet() {}
`)
	assert.Equal(t, "rust:ffi-export", byID["src/lib.rs::gx_open"].Meta[MetaEntryKind])
	assert.Equal(t, "rust:ffi-export", byID["src/lib.rs::greet"].Meta[MetaEntryKind])
}

// An extern block declares the foreign half of the boundary: its body
// lives in C, so a missing definition is not a missing implementation.
func TestDetectRust_ExternBlockDeclarationsAreNotDeadCode(t *testing.T) {
	byID := detectRustFor(t, "src/ffi.rs", `extern "C" {
    fn sqlite3_open(name: *const u8) -> i32;
}
`)
	n := byID["src/ffi.rs::sqlite3_open"]
	require.NotNil(t, n)
	assert.Equal(t, "rust:ffi-import", n.Meta[MetaEntryKind])
}
