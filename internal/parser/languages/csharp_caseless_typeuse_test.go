package languages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

// refTargets collects the reference-form edge targets of one kind, keyed by
// target id, with the ref_context that produced them.
func refTargets(edges []*graph.Edge, kind graph.EdgeKind) map[string]string {
	out := map[string]string{}
	for _, e := range edges {
		if e.Kind != kind {
			continue
		}
		ctx := ""
		if e.Meta != nil {
			if v, ok := e.Meta["ref_context"].(string); ok {
				ctx = v
			}
		}
		out[e.To] = ctx
	}
	return out
}

// A type named in a script without case — Chinese here, but Japanese, Korean,
// Hebrew, Arabic and Thai are the same shape — can never be "Capitalized".
// Gating the reference-form pass on capitalisation therefore did not merely
// lose precision in such a codebase, it dropped every instantiation, cast and
// static access in the repository.
func TestCSharpTypeUse_CaselessScriptTypeNames(t *testing.T) {
	src := []byte(`public class 消息工厂 {
    public object 创建(object 输入) {
        var 请求 = new 钓鱼点覆盖请求();
        var 类型 = 消息类型.钓鱼点覆盖请求;
        var 转换 = 输入 as 钓鱼点覆盖请求;
        if (输入 is 钓鱼点列表响应) { }
        return 请求;
    }
}`)
	e := NewCSharpExtractor()
	res, err := e.Extract("消息工厂.cs", src)
	require.NoError(t, err)

	inst := refTargets(res.Edges, graph.EdgeInstantiates)
	require.Contains(t, inst, "unresolved::钓鱼点覆盖请求",
		"`new 钓鱼点覆盖请求()` must mint an instantiates edge")

	refs := refTargets(res.Edges, graph.EdgeReferences)
	assert.Equal(t, "cast", refs["unresolved::钓鱼点覆盖请求"],
		"`输入 as 钓鱼点覆盖请求` must mint a cast reference")
	assert.Equal(t, "cast", refs["unresolved::钓鱼点列表响应"],
		"`输入 is 钓鱼点列表响应` must mint a cast reference")
	assert.Equal(t, "static_access", refs["unresolved::消息类型"],
		"`消息类型.X` must mint a static_access reference to the enum type")
}

// The capitalisation gate stood in for "is this receiver a value?". Where a
// script has no case the question is answered directly instead, so a local
// that shares a name with a type must still not be read as a static access.
func TestCSharpTypeUse_CaselessLocalReceiverIsNotStaticAccess(t *testing.T) {
	src := []byte(`public class 处理器 {
    public void 运行() {
        var 消息 = 取消息();
        var 值 = 消息.字段;
    }
    private object 取消息() { return null; }
}`)
	e := NewCSharpExtractor()
	res, err := e.Extract("处理器.cs", src)
	require.NoError(t, err)

	refs := refTargets(res.Edges, graph.EdgeReferences)
	assert.NotContains(t, refs, "unresolved::消息",
		"a receiver the file declares as a local must not become a type reference")
}

// Cased-script behaviour is unchanged: an uppercase receiver is a static
// access, a lowercase one is not, and primitives stay out.
func TestCSharpTypeUse_CasedScriptUnchanged(t *testing.T) {
	src := []byte(`public class Factory {
    public object Build(object input) {
        var local = Helper();
        var dead = local.Field;
        var color = Palette.Red;
        var made = new Widget();
        return made;
    }
    private object Helper() { return null; }
}`)
	e := NewCSharpExtractor()
	res, err := e.Extract("Factory.cs", src)
	require.NoError(t, err)

	refs := refTargets(res.Edges, graph.EdgeReferences)
	assert.Equal(t, "static_access", refs["unresolved::Palette"],
		"an uppercase receiver is still a static access")
	assert.NotContains(t, refs, "unresolved::local",
		"a lowercase receiver is still rejected")

	inst := refTargets(res.Edges, graph.EdgeInstantiates)
	assert.Contains(t, inst, "unresolved::Widget")
}
