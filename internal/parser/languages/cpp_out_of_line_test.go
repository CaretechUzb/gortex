package languages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

// cppNodeByID indexes an extraction result so a test can assert on one node.
func cppNodeByID(res *parser.ExtractionResult) map[string]*graph.Node {
	byID := map[string]*graph.Node{}
	for _, n := range res.Nodes {
		byID[n.ID] = n
	}
	return byID
}

// The declaring class lives in a header, so the translation unit holds nothing
// but out-of-line definitions — the shape of an idiomatic .cpp. Every one of
// them used to extract as no node at all, which also silently cost every call
// in the bodies: a deferred call with no enclosing function range is dropped.
func TestCppExtractor_OutOfLineMemberEmitsNodeAndCalls(t *testing.T) {
	src := []byte(`#include "cls.h"

void Cls::depth2() {
  boost::asio::post(ex);
}

Widget *ns::Cls::depth3() {
  std::chrono::duration_cast<int>(value);
  return nullptr;
}

void a::b::Cls::depth4() {
  helper();
}
`)
	res, err := NewCppExtractor().Extract("demo.cpp", src)
	require.NoError(t, err)
	byID := cppNodeByID(res)
	byLine := genericCallTargets(res)

	depth2 := byID["demo.cpp::Cls.depth2"]
	require.NotNil(t, depth2, "an out-of-line member must emit a node")
	assert.Equal(t, graph.KindMethod, depth2.Kind)
	assert.Equal(t, "depth2", depth2.Name, "the trailing segment names the member")
	assert.Equal(t, "Cls", depth2.Meta["receiver"])
	assert.Contains(t, byLine[4], "unresolved::post",
		"a call in an out-of-line body needs the body's own function range")

	depth3 := byID["demo.cpp::Cls.depth3"]
	require.NotNil(t, depth3, "a pointer return must not hide the qualified declarator")
	assert.Equal(t, "depth3", depth3.Name)
	assert.Equal(t, "ns", depth3.Meta["scope_ns"],
		"the segments ahead of the owner are its namespace")
	assert.Contains(t, byLine[8], "unresolved::duration_cast")

	depth4 := byID["demo.cpp::Cls.depth4"]
	require.NotNil(t, depth4, "qualification depth must not cap definition extraction")
	assert.Equal(t, "a::b", depth4.Meta["scope_ns"])
	assert.Contains(t, byLine[13], "unresolved::helper")
}

// Two classes defining a same-named member in one translation unit is ordinary
// C++; keying the node on the owner is what keeps the second from colliding
// with the first and being dropped.
func TestCppExtractor_OutOfLineMembersDoNotCollide(t *testing.T) {
	src := []byte(`#include "cls.h"

void Alpha::run() {
  alphaWork();
}

void Beta::run() {
  betaWork();
}
`)
	res, err := NewCppExtractor().Extract("demo.cpp", src)
	require.NoError(t, err)
	byID := cppNodeByID(res)

	require.NotNil(t, byID["demo.cpp::Alpha.run"])
	require.NotNil(t, byID["demo.cpp::Beta.run"])
	byLine := genericCallTargets(res)
	assert.Contains(t, byLine[4], "unresolved::alphaWork")
	assert.Contains(t, byLine[8], "unresolved::betaWork")
}

// A qualifier naming a namespace declares a free function, not a member. The
// class/struct declared in the same file is the evidence; a name this file
// never declared falls back to the Capitalized-type convention.
func TestCppExtractor_OutOfLineNamespaceFunctionStaysFree(t *testing.T) {
	src := []byte(`namespace ns { void helper(); }

void ns::helper() {
  work();
}
`)
	res, err := NewCppExtractor().Extract("demo.cpp", src)
	require.NoError(t, err)
	byID := cppNodeByID(res)

	fn := byID["demo.cpp::helper"]
	require.NotNil(t, fn, "a namespace-qualified definition is a free function")
	assert.Equal(t, graph.KindFunction, fn.Kind)
	assert.Equal(t, "ns", fn.Meta["scope_ns"])
	assert.Nil(t, fn.Meta["receiver"], "a namespace is not a receiver")
	assert.Contains(t, genericCallTargets(res)[4], "unresolved::work")
}

// When the class is declared in this file, the member links to it — and the
// in-class declaration must not double as a second node for the definition.
func TestCppExtractor_OutOfLineMemberLinksLocalClass(t *testing.T) {
	src := []byte(`class Cls {
public:
  void run();
};

void Cls::run() {
  work();
}
`)
	res, err := NewCppExtractor().Extract("demo.cpp", src)
	require.NoError(t, err)

	var memberOf int
	for _, e := range res.Edges {
		if e.Kind == graph.EdgeMemberOf && e.From == "demo.cpp::Cls.run" && e.To == "demo.cpp::Cls" {
			memberOf++
		}
	}
	assert.Equal(t, 1, memberOf, "the member belongs to the class declared here")

	var runNodes int
	for _, n := range res.Nodes {
		if n.Name == "run" {
			runNodes++
		}
	}
	assert.Equal(t, 1, runNodes, "one definition is one node")
}

// A destructor, an operator, and a templated member all reach the same walk;
// none of them names itself with a bare identifier.
func TestCppExtractor_OutOfLineSpecialMembers(t *testing.T) {
	src := []byte(`#include "cls.h"

Cls::~Cls() {
  teardown();
}

Cls &Cls::operator=(const Cls &other) {
  copyFrom(other);
  return *this;
}

template <typename T>
void Holder<T>::put(T value) {
  store(value);
}
`)
	res, err := NewCppExtractor().Extract("demo.cpp", src)
	require.NoError(t, err)
	byID := cppNodeByID(res)
	byLine := genericCallTargets(res)

	dtor := byID["demo.cpp::Cls.~Cls"]
	require.NotNil(t, dtor, "an out-of-line destructor is a member")
	assert.Contains(t, byLine[4], "unresolved::teardown")

	op := byID["demo.cpp::Cls.operator="]
	require.NotNil(t, op, "an out-of-line operator is a member")
	assert.Contains(t, byLine[8], "unresolved::copyFrom")

	put := byID["demo.cpp::Holder.put"]
	require.NotNil(t, put, "a template member drops its type arguments to name its owner")
	assert.Equal(t, "Holder", put.Meta["receiver"])
	assert.Contains(t, byLine[14], "unresolved::store")

	// The template_declaration and the function_definition it wraps both reach
	// a dispatch that can now name a qualified declarator; only one may emit.
	var putNodes int
	for _, n := range res.Nodes {
		if n.Name == "put" {
			putNodes++
		}
	}
	assert.Equal(t, 1, putNodes, "a template member emits once, not once per dispatch")
}

// The declarator walk also decodes qualified callees, so a qualified operator
// call names itself the way an out-of-line operator definition does instead of
// being dropped for not ending in a bare identifier.
func TestCppExtractor_QualifiedOperatorCallEmitsEdge(t *testing.T) {
	src := []byte(`void run(Vec a, Vec b) {
  ns::operator+(a, b);
}
`)
	res, err := NewCppExtractor().Extract("demo.cpp", src)
	require.NoError(t, err)
	assert.Contains(t, genericCallTargets(res)[2], "unresolved::operator+")
}

// Free functions and inline methods are the paths that already worked; the
// broadened declarator pattern must leave both exactly as they were.
func TestCppExtractor_PlainDefinitionsUnchanged(t *testing.T) {
	src := []byte(`namespace ns {

class Cls {
public:
  void inlineM() { inlineWork(); }
};

void freeFn() {
  freeWork();
}

} // namespace ns
`)
	res, err := NewCppExtractor().Extract("demo.cpp", src)
	require.NoError(t, err)
	byID := cppNodeByID(res)
	byLine := genericCallTargets(res)

	inlineM := byID["demo.cpp::Cls.inlineM"]
	require.NotNil(t, inlineM)
	assert.Equal(t, graph.KindMethod, inlineM.Kind)
	assert.Contains(t, byLine[5], "unresolved::inlineWork")

	freeFn := byID["demo.cpp::freeFn"]
	require.NotNil(t, freeFn)
	assert.Equal(t, graph.KindFunction, freeFn.Kind)
	assert.Equal(t, "ns", freeFn.Meta["scope_ns"])
	assert.Contains(t, byLine[9], "unresolved::freeWork")
}
