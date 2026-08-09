package languages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestJSExtractor_Function(t *testing.T) {
	src := []byte(`function greet(name) {
  console.log("Hello " + name);
}

const add = (a, b) => a + b;
`)
	e := NewJavaScriptExtractor()
	result, err := e.Extract("app.js", src)
	require.NoError(t, err)

	funcs := nodesOfKind(result.Nodes, graph.KindFunction)
	assert.GreaterOrEqual(t, len(funcs), 1)
}

func TestJSExtractor_Class(t *testing.T) {
	src := []byte(`class UserService {
  constructor(db) {
    this.db = db;
  }

  findUser(id) {
    return this.db.query(id);
  }
}
`)
	e := NewJavaScriptExtractor()
	result, err := e.Extract("service.js", src)
	require.NoError(t, err)

	types := nodesOfKind(result.Nodes, graph.KindType)
	assert.Len(t, types, 1)

	methods := nodesOfKind(result.Nodes, graph.KindMethod)
	assert.GreaterOrEqual(t, len(methods), 1)
}

func TestJSExtractor_Imports(t *testing.T) {
	src := []byte(`import express from "express";
const path = require("path");
`)
	e := NewJavaScriptExtractor()
	result, err := e.Extract("app.js", src)
	require.NoError(t, err)

	imports := edgesOfKind(result.Edges, graph.EdgeImports)
	assert.GreaterOrEqual(t, len(imports), 1)
}

func TestJSExtractor_ArrowFunction(t *testing.T) {
	src := []byte(`const handler = () => {
  console.log("hello");
};
`)
	e := NewJavaScriptExtractor()
	result, err := e.Extract("app.js", src)
	require.NoError(t, err)

	funcs := nodesOfKind(result.Nodes, graph.KindFunction)
	require.Len(t, funcs, 1)
	assert.Equal(t, "handler", funcs[0].Name)
}

func TestJSExtractor_Variables(t *testing.T) {
	src := []byte(`const API_URL = "https://api.example.com";
let count = 0;
var legacy = true;
`)
	e := NewJavaScriptExtractor()
	result, err := e.Extract("config.js", src)
	require.NoError(t, err)

	vars := nodesOfKind(result.Nodes, graph.KindVariable)
	assert.GreaterOrEqual(t, len(vars), 2)
}

func TestJSExtractor_MethodMemberOf(t *testing.T) {
	src := []byte(`class Repo {
  save(item) {
    return item;
  }
}
`)
	e := NewJavaScriptExtractor()
	result, err := e.Extract("repo.js", src)
	require.NoError(t, err)

	memberEdges := edgesOfKind(result.Edges, graph.EdgeMemberOf)
	assert.GreaterOrEqual(t, len(memberEdges), 1)
}

func TestJSExtractor_NamedFunctionExpression(t *testing.T) {
	cases := []struct {
		name    string
		src     string
		want    string
		wantSig string
	}{
		{
			name:    "default export logical operand",
			src:     "export default isHttpAdapterSupported && function httpAdapter(config) {\n  return config;\n};\n",
			want:    "httpAdapter",
			wantSig: "function httpAdapter()",
		},
		{
			name:    "const initializer",
			src:     "const wrapped = function inner(a) {\n  return a;\n};\n",
			want:    "inner",
			wantSig: "function inner()",
		},
		{
			name:    "var initializer",
			src:     "var legacy = function legacyImpl() {\n  return 1;\n};\n",
			want:    "legacyImpl",
			wantSig: "function legacyImpl()",
		},
		{
			name:    "object property value",
			src:     "const api = {\n  run: function runTask() {\n    return 1;\n  },\n};\n",
			want:    "runTask",
			wantSig: "function runTask()",
		},
		{
			name:    "module.exports assignment",
			src:     "module.exports = function createServer(opts) {\n  return opts;\n};\n",
			want:    "createServer",
			wantSig: "function createServer()",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := NewJavaScriptExtractor()
			result, err := e.Extract("mod.js", []byte(tc.src))
			require.NoError(t, err)

			fn := nodeNamed(t, result.Nodes, graph.KindFunction, tc.want)
			assert.Equal(t, "mod.js", fn.FilePath)
			assert.Equal(t, "javascript", fn.Language)
			assert.Greater(t, fn.EndLine, fn.StartLine)
			assert.Equal(t, tc.wantSig, fn.Meta["signature"])

			var defined bool
			for _, ed := range edgesOfKind(result.Edges, graph.EdgeDefines) {
				if ed.From == "mod.js" && ed.To == fn.ID {
					defined = true
				}
			}
			assert.True(t, defined, "file must define %s", fn.ID)
		})
	}
}

func TestJSExtractor_AnonymousFunctionExpressionUnemitted(t *testing.T) {
	src := []byte(`const anon = function () {
  return 1;
};
module.exports = function () {
  return 2;
};
`)
	e := NewJavaScriptExtractor()
	result, err := e.Extract("anon.js", src)
	require.NoError(t, err)
	assert.Empty(t, nodesOfKind(result.Nodes, graph.KindFunction))
}

func TestJSExtractor_Extensions(t *testing.T) {
	e := NewJavaScriptExtractor()
	exts := e.Extensions()
	assert.Contains(t, exts, ".js")
	assert.Contains(t, exts, ".jsx")
	assert.Contains(t, exts, ".mjs")
	assert.Equal(t, "javascript", e.Language())
}
