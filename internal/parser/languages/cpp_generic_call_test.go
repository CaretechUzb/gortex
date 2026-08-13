package languages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCppExtractor_TemplateCallEmitsEdge(t *testing.T) {
	src := []byte(`void run(Obj obj, Obj* ptr) {
  plain();
  generic<int>();
  obj.method();
  obj.methodT<int>();
  ptr->methodT<int>();
}
`)
	res, err := NewCppExtractor().Extract("demo.cpp", src)
	require.NoError(t, err)
	byLine := genericCallTargets(res)

	assert.Contains(t, byLine[2], "unresolved::plain", "control")
	assert.Contains(t, byLine[3], "unresolved::generic",
		"a template_function callee must not cost the edge")
	assert.Contains(t, byLine[4], "unresolved::*.method", "control")
	assert.Contains(t, byLine[5], "unresolved::*.methodT",
		"a template_method through . must not cost the edge")
	assert.Contains(t, byLine[6], "unresolved::*.methodT",
		"a template_method through -> must not cost the edge")
}

// C++ namespace-qualified calls are a second, non-generic blind spot in the
// same query: the callee is a qualified_identifier, which the bare-identifier
// pattern cannot match, so every std:: / ns:: free call was dropped.
func TestCppExtractor_QualifiedCallEmitsEdge(t *testing.T) {
	src := []byte(`void run() {
  ns::helper();
  ns::templated<int>();
  ns::Type::method();
  ns::Type::templated<int>();
  a::b::c::function();
  a::b::c::templated<int>();
  std::chrono::duration_cast<int>(value);
  boost::asio::post(ex);
}
`)
	res, err := NewCppExtractor().Extract("demo.cpp", src)
	require.NoError(t, err)
	byLine := genericCallTargets(res)

	assert.Contains(t, byLine[2], "unresolved::helper",
		"the trailing name is what a qualified call names")
	assert.Contains(t, byLine[3], "unresolved::templated",
		"a templated qualified call names the same trailing identifier")
	assert.Contains(t, byLine[4], "unresolved::method",
		"a three-segment qualified call names its trailing identifier")
	assert.Contains(t, byLine[5], "unresolved::templated",
		"a three-segment templated call names its trailing identifier")
	assert.Contains(t, byLine[6], "unresolved::function",
		"qualification depth must not cap call extraction")
	assert.Contains(t, byLine[7], "unresolved::templated",
		"template extraction must not be capped by qualification depth")
	assert.Contains(t, byLine[8], "unresolved::duration_cast",
		"standard-library nested calls must remain visible")
	assert.Contains(t, byLine[9], "unresolved::post",
		"library namespace depth must not hide free calls")
}
