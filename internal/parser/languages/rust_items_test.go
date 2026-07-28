package languages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func rustNodesByID(t *testing.T, src string) (map[string]*graph.Node, []*graph.Edge) {
	t.Helper()
	e := NewRustExtractor()
	result, err := e.Extract("probe.rs", []byte(src))
	require.NoError(t, err)
	byID := make(map[string]*graph.Node, len(result.Nodes))
	for _, n := range result.Nodes {
		byID[n.ID] = n
	}
	return byID, result.Edges
}

// A `mod` is a namespace, so it lands as KindPackage the way a C++ or C#
// namespace does — and the items inside it record which module they came
// from.
func TestRsExtractor_ModuleNode(t *testing.T) {
	byID, _ := rustNodesByID(t, `mod inner {
    pub fn hi() {}
}
mod declared_elsewhere;
`)
	mod := byID["probe.rs::inner"]
	require.NotNil(t, mod, "an inline mod must mint a node")
	assert.Equal(t, graph.KindPackage, mod.Kind)
	assert.Equal(t, "module", mod.Meta["type_flavor"])

	decl := byID["probe.rs::declared_elsewhere"]
	require.NotNil(t, decl, "a bodyless `mod foo;` must mint a node too")
	assert.Equal(t, true, decl.Meta["declaration_only"])

	require.NotNil(t, byID["probe.rs::hi"])
	assert.Equal(t, "inner", byID["probe.rs::hi"].Meta["scope_mod"])
}

// Two same-named types in two modules of one file both have to survive.
// Before this, the second was dropped and — worse — its fields were
// re-parented onto the first, so the surviving type reported members it
// does not have.
func TestRsExtractor_SameNameAcrossModulesBothSurvive(t *testing.T) {
	byID, edges := rustNodesByID(t, `mod alpha {
    pub struct Config { pub a: u8 }
}
mod beta {
    pub struct Config { pub b: u8 }
}
`)
	first := byID["probe.rs::Config"]
	second := byID["probe.rs::Config_L5"]
	require.NotNil(t, first, "first Config must keep the clean id")
	require.NotNil(t, second, "second Config must survive under a disambiguated id")
	assert.Equal(t, "alpha", first.Meta["scope_mod"])
	assert.Equal(t, "beta", second.Meta["scope_mod"])

	require.NotNil(t, byID["probe.rs::Config.a"])
	require.NotNil(t, byID["probe.rs::Config_L5.b"])

	// Each field must point at its own struct, not at whichever struct
	// happened to be emitted first.
	owners := map[string]string{}
	for _, e := range edges {
		if e.Kind == graph.EdgeMemberOf {
			owners[e.From] = e.To
		}
	}
	assert.Equal(t, "probe.rs::Config", owners["probe.rs::Config.a"])
	assert.Equal(t, "probe.rs::Config_L5", owners["probe.rs::Config_L5.b"])
}

func TestRsExtractor_TypeAlias(t *testing.T) {
	byID, _ := rustNodesByID(t, `pub type Registry<T> = HashMap<String, T>;`)
	n := byID["probe.rs::Registry"]
	require.NotNil(t, n, "a type alias must mint a node")
	assert.Equal(t, graph.KindType, n.Kind)
	assert.Equal(t, "alias", n.Meta["type_flavor"])
	assert.Equal(t, "HashMap<String, T>", n.Meta["aliases"])
}

func TestRsExtractor_UnionAndItsFields(t *testing.T) {
	byID, _ := rustNodesByID(t, `pub union Value { int: i64, float: f64 }`)
	u := byID["probe.rs::Value"]
	require.NotNil(t, u, "a union must mint a node")
	assert.Equal(t, "union", u.Meta["type_flavor"])
	require.NotNil(t, byID["probe.rs::Value.int"], "union fields were previously skipped outright")
	require.NotNil(t, byID["probe.rs::Value.float"])
}

func TestRsExtractor_MacroRulesDefinition(t *testing.T) {
	byID, _ := rustNodesByID(t, `#[macro_export]
macro_rules! log_it {
    () => {};
    ($x:expr) => {};
}
`)
	m := byID["probe.rs::log_it!"]
	require.NotNil(t, m, "macro_rules! must mint a node")
	assert.Equal(t, graph.KindMacro, m.Kind)
	assert.Equal(t, VisibilityPublic, m.Meta["visibility"], "#[macro_export] makes a macro public")
	assert.Equal(t, 2, m.Meta["rules"])
}

// An `extern "C"` block declares the foreign half of an FFI boundary.
// These are the call targets of every unsafe call into C.
func TestRsExtractor_ExternBlockFunctions(t *testing.T) {
	byID, _ := rustNodesByID(t, `extern "C" {
    fn sqlite3_open(name: *const u8) -> i32;
}
`)
	fn := byID["probe.rs::sqlite3_open"]
	require.NotNil(t, fn, "an extern block fn must mint a node")
	assert.Equal(t, graph.KindFunction, fn.Kind)
	assert.Equal(t, "C", fn.Meta["abi"])
	assert.Equal(t, true, fn.Meta["is_extern"])
	assert.Equal(t, true, fn.Meta["external"])
}

func TestRsExtractor_ExternCrate(t *testing.T) {
	_, edges := rustNodesByID(t, `extern crate serde;`)
	var found bool
	for _, e := range edges {
		if e.Kind == graph.EdgeImports && e.To == "unresolved::import::serde" {
			found = true
			assert.Equal(t, true, e.Meta["extern_crate"])
		}
	}
	assert.True(t, found, "extern crate must record an import edge")
}

func TestRsExtractor_AssociatedType(t *testing.T) {
	byID, edges := rustNodesByID(t, `trait Source {
    type Item: Clone;
    fn get(&self) -> Self::Item;
}
`)
	at := byID["probe.rs::Source.Item"]
	require.NotNil(t, at, "a trait's associated type must mint a node")
	assert.Equal(t, "associated_type", at.Meta["type_flavor"])
	assert.Equal(t, "Clone", at.Meta["bound"])

	var member bool
	for _, e := range edges {
		if e.From == "probe.rs::Source.Item" && e.To == "probe.rs::Source" && e.Kind == graph.EdgeMemberOf {
			member = true
		}
	}
	assert.True(t, member, "an associated type belongs to its trait")
}

// The signature meta used to be the literal placeholder "fn name(...)"
// for every function in every Rust file.
func TestRsExtractor_SignatureAndQualifiers(t *testing.T) {
	byID, _ := rustNodesByID(t, `pub async unsafe fn fetch<T: Clone>(url: &str, n: usize) -> Option<T> where T: Send { todo!() }
pub const fn zero() -> i32 { 0 }
`)
	f := byID["probe.rs::fetch"]
	require.NotNil(t, f)
	assert.Equal(t,
		"async unsafe fn fetch<T: Clone>(url: &str, n: usize) -> Option<T> where T: Send",
		f.Meta["signature"])
	assert.Equal(t, true, f.Meta["is_async"])
	assert.Equal(t, true, f.Meta["is_unsafe"])

	z := byID["probe.rs::zero"]
	require.NotNil(t, z)
	assert.Equal(t, true, z.Meta["is_const"])
}

// A comment sitting between an attribute stack and its item is ordinary
// Rust style; it used to discard every attribute above it.
func TestRsExtractor_AttributesSurviveInterleavedComment(t *testing.T) {
	_, edges := rustNodesByID(t, `#[derive(Debug, Clone)]
// why this type exists
pub struct Dto { x: i32 }
`)
	got := map[string]bool{}
	for _, e := range edges {
		if e.Kind == graph.EdgeAnnotated && e.From == "probe.rs::Dto" {
			got[e.To] = true
		}
	}
	assert.True(t, got["annotation::rust::Debug"], "attributes above a comment must still attach")
	assert.True(t, got["annotation::rust::Clone"])
}

// A newtype's wrapped type used to be invisible: no field node meant
// find_usages on Config could not see that Wrapper carries one.
func TestRsExtractor_TupleStructAndVariantPayloads(t *testing.T) {
	byID, edges := rustNodesByID(t, `struct Wrapper(Config, u8);
enum Msg { Started, Failed(Error), Rich { code: u16 } }
`)
	w0 := byID["probe.rs::Wrapper.0"]
	require.NotNil(t, w0, "a tuple struct's positional field must exist")
	assert.Equal(t, "Config", w0.Meta["field_type"])
	assert.Equal(t, true, w0.Meta["positional"])
	require.NotNil(t, byID["probe.rs::Wrapper.1"])

	// A payload nests under its variant so two payload-carrying
	// variants cannot collide on ".0".
	require.NotNil(t, byID["probe.rs::Msg.Failed.0"])
	assert.Equal(t, "Msg.Failed", byID["probe.rs::Msg.Failed.0"].Meta["receiver"])
	require.NotNil(t, byID["probe.rs::Msg.Rich.code"],
		"a struct-shaped variant's fields were dropped as well")

	var typed bool
	for _, e := range edges {
		if e.From == "probe.rs::Wrapper.0" && e.To == "unresolved::Config" && e.Kind == graph.EdgeTypedAs {
			typed = true
		}
	}
	assert.True(t, typed, "the wrapped type must be reachable from the newtype")
}

// An associated const belongs to its impl. Emitting every const at file
// scope meant two impls declaring the same const name fought over one id
// and the loser was dropped with no node and no edge.
func TestRsExtractor_AssociatedConstsKeepTheirOwner(t *testing.T) {
	byID, edges := rustNodesByID(t, `const MAX: u32 = 1;
struct A;
struct B;
impl A { pub const LIMIT: usize = 10; }
impl B { pub const LIMIT: usize = 20; }
`)
	require.NotNil(t, byID["probe.rs::A.LIMIT"], "impl A's const must survive")
	require.NotNil(t, byID["probe.rs::B.LIMIT"], "impl B's const must survive too")
	assert.Equal(t, "A", byID["probe.rs::A.LIMIT"].Meta["receiver"])
	assert.Equal(t, "B", byID["probe.rs::B.LIMIT"].Meta["receiver"])

	owners := map[string]string{}
	for _, e := range edges {
		if e.Kind == graph.EdgeMemberOf {
			owners[e.From] = e.To
		}
	}
	assert.Equal(t, "probe.rs::A", owners["probe.rs::A.LIMIT"])
	assert.Equal(t, "probe.rs::B", owners["probe.rs::B.LIMIT"])

	top := byID["probe.rs::MAX"]
	require.NotNil(t, top)
	assert.Equal(t, graph.KindConstant, top.Kind, "a const is a constant, not a variable")
	assert.Equal(t, "u32", top.Meta["declared_type"])
}

// `static mut` is the one form that is genuinely mutable shared state.
func TestRsExtractor_StaticMutIsMarked(t *testing.T) {
	byID, _ := rustNodesByID(t, `static mut COUNTER: u64 = 0;
static NAME: &str = "x";
`)
	require.NotNil(t, byID["probe.rs::COUNTER"])
	assert.Equal(t, graph.KindVariable, byID["probe.rs::COUNTER"].Kind)
	assert.Equal(t, true, byID["probe.rs::COUNTER"].Meta["mutable"])
	assert.Nil(t, byID["probe.rs::NAME"].Meta["mutable"])
}

// Result error extraction split on every comma and looked at the last
// "::" in the string, so a tuple payload produced the literal target
// "u16)" and a fully-qualified Result produced no edge at all.
func TestRustErrorTypeFromResult_TupleAndQualified(t *testing.T) {
	assert.Equal(t, "MyError", rustErrorTypeFromResult("Result<(u8, u16), MyError>"))
	assert.Equal(t, "Error", rustErrorTypeFromResult("std::result::Result<u8, io::Error>"))
	assert.Equal(t, "ParseError", rustErrorTypeFromResult("Result<Vec<[u8; 4]>, ParseError>"))
	assert.Equal(t, "", rustErrorTypeFromResult("anyhow::Result<u8>"))
	assert.Equal(t, "", rustErrorTypeFromResult("i32"))
}
