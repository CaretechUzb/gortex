package languages

import (
	"sort"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/resolver"
)

// javaMethodRefGraph extracts every file and runs the fn-value gate, i.e. the
// same capture → resolve pair the indexer runs. A method reference only becomes
// a real edge once both halves have run, so the gate is part of what these
// tests pin.
func javaMethodRefGraph(t *testing.T, files map[string]string) *graph.Graph {
	t.Helper()
	g := graph.New()
	paths := make([]string, 0, len(files))
	for path := range files {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		nodes, edges := runJavaExtract(t, path, files[path])
		g.AddBatch(nodes, edges)
	}
	resolver.ResolveFnValueCallbacks(g)
	return g
}

// methodRefSources returns the IDs that reference target through a bound
// method-reference edge (the gate's callback_registration form).
func methodRefSources(g *graph.Graph, target string) []string {
	var out []string
	for _, e := range g.GetInEdges(target) {
		if e.Kind != graph.EdgeReferences || e.Meta == nil {
			continue
		}
		if via, _ := e.Meta["via"].(string); via == "callback_registration" {
			out = append(out, e.From)
		}
	}
	sort.Strings(out)
	return out
}

func hasMethodRef(g *graph.Graph, target string) bool {
	return len(methodRefSources(g, target)) > 0
}

// TestJavaMethodRef_Forms pins the four method-reference forms the Java grammar
// admits — `Type::staticMethod`, `Type::instanceMethod`, `instance::method`,
// and `Type::new` — as incoming reference edges on their target. Before this,
// `Type::new` was rejected outright: the constructor's `new` is an anonymous
// token, so the trailing-named-child rule read the *type* as the member name
// and the flood guard then dropped it.
func TestJavaMethodRef_Forms(t *testing.T) {
	g := javaMethodRefGraph(t, map[string]string{
		"app/Pipeline.java": `package app;

import java.util.List;

public class Pipeline {
	private Helper helper;

	public List<Out> run(List<In> items) {
		items.forEach(Statics::configure);
		items.forEach(ExampleType::transform);
		items.forEach(helper::assist);
		items.stream().map(Wrapper::new).toList();
		items.forEach(this::consume);
		return null;
	}

	void consume(In in) {}
}
`,
		"app/Statics.java": `package app;
public class Statics {
	public static void configure(In in) {}
}
`,
		"app/ExampleType.java": `package app;
public class ExampleType {
	public void transform(In in) {}
}
`,
		"app/Helper.java": `package app;
public class Helper {
	public void assist(In in) {}
}
`,
		"app/Wrapper.java": `package app;
public class Wrapper {
	public Wrapper(In in) {}
}
`,
	})

	for _, tc := range []struct {
		form   string
		target string
	}{
		{"Type::staticMethod", "app/Statics.java::Statics.configure"},
		{"Type::instanceMethod", "app/ExampleType.java::ExampleType.transform"},
		{"instance::method", "app/Helper.java::Helper.assist"},
		{"Type::new", "app/Wrapper.java::Wrapper.<init>"},
		{"this::method", "app/Pipeline.java::Pipeline.consume"},
	} {
		if !hasMethodRef(g, tc.target) {
			t.Errorf("%s: expected an incoming reference edge on %s", tc.form, tc.target)
		}
	}
}

// TestJavaMethodRef_QualifiedTypes pins that an import-qualified, nested, or
// generic qualifier still matches the receiver its methods are indexed under.
// The raw qualifier text (`app.Deep`, `Outer.Inner`, `Box<In>`) never equals a
// method's receiver, so these fell through to a repo-wide unique-or-drop
// lookup that silently dropped every non-unique method name.
func TestJavaMethodRef_QualifiedTypes(t *testing.T) {
	g := javaMethodRefGraph(t, map[string]string{
		"app/Uses.java": `package app;
import java.util.List;
public class Uses {
	public void go(List<String> xs) {
		xs.forEach(app.Deep::run);
		xs.forEach(Outer.Inner::run);
		xs.forEach(Box::run);
	}
}
`,
		"app/Deep.java": `package app;
public class Deep {
	public void run(String s) {}
}
`,
		"app/Outer.java": `package app;
public class Outer {
	public static class Inner {
		public void run(String s) {}
	}
}
`,
		"app/Box.java": `package app;
public class Box {
	public void run(String s) {}
}
`,
	})

	// `run` is declared by three unrelated types, so a repo-wide unique-or-drop
	// lookup resolves none of them — only receiver matching can.
	for _, target := range []string{
		"app/Deep.java::Deep.run",
		"app/Outer.java::Inner.run",
		"app/Box.java::Box.run",
	} {
		if !hasMethodRef(g, target) {
			t.Errorf("expected an incoming reference edge on %s", target)
		}
	}
}

// TestJavaMethodRef_NoFalsePositives pins the negatives: an array constructor
// reference has no declared constructor to bind, and an ordinary call must not
// be mistaken for a method reference.
func TestJavaMethodRef_NoFalsePositives(t *testing.T) {
	files := map[string]string{
		"app/Uses.java": `package app;
import java.util.List;
public class Uses {
	public void go(List<String> xs) {
		xs.toArray(String[]::new);
		Wrapper w = new Wrapper();
		helper();
	}
	void helper() {}
}
`,
		"app/Wrapper.java": `package app;
public class Wrapper {
	public Wrapper() {}
}
`,
	}
	_, edges := runJavaExtract(t, "app/Uses.java", files["app/Uses.java"])
	for _, e := range edges {
		if e.Meta == nil {
			continue
		}
		if via, _ := e.Meta["via"].(string); via != "callback_candidate" {
			continue
		}
		name, _ := e.Meta["fn_value_name"].(string)
		if name == "String.<init>" || name == "String[].<init>" {
			t.Errorf("`String[]::new` captured a constructor candidate %q — an array allocation has no declared constructor", name)
		}
	}

	g := javaMethodRefGraph(t, files)
	// `new Wrapper()` is an instantiation, not a method reference: it must not
	// produce a callback_registration edge on the constructor.
	for _, e := range g.GetInEdges("app/Wrapper.java::Wrapper.<init>") {
		if e.Meta == nil {
			continue
		}
		if via, _ := e.Meta["via"].(string); via == "callback_registration" {
			t.Errorf("`new Wrapper()` produced a method-reference edge on the constructor")
		}
	}
}
