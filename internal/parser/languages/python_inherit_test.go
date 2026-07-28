package languages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestPyClass_EmitsInheritanceEdges(t *testing.T) {
	src := []byte(`import abc
from django.db import models
from typing import Generic, TypeVar

T = TypeVar("T")


class Repo(models.Model, abc.ABC, Generic[T], metaclass=abc.ABCMeta):
    pass


class Standalone:
    pass
`)
	e := NewPythonExtractor()
	result, err := e.Extract("repo.py", src)
	require.NoError(t, err)

	var bases []string
	byTarget := map[string]*graph.Edge{}
	for _, ed := range result.Edges {
		if ed.Kind != graph.EdgeExtends {
			continue
		}
		assert.Equal(t, "repo.py::Repo", ed.From)
		bases = append(bases, ed.To)
		byTarget[ed.To] = ed
	}
	assert.ElementsMatch(t, []string{
		"unresolved::Model", "unresolved::ABC", "unresolved::Generic",
	}, bases, "metaclass= is a keyword argument, not a base class")

	// The written-out path is kept so a resolver can disambiguate a bare
	// base name that several files define.
	assert.Equal(t, "models.Model", byTarget["unresolved::Model"].Meta["base_path"])
	assert.Equal(t, "generic", byTarget["unresolved::Generic"].Meta["via"])
	assert.Nil(t, byTarget["unresolved::ABC"].Meta["via"])
}

func TestPyClass_MultipleInheritanceKeepsEveryBase(t *testing.T) {
	src := []byte(`class Handler(LoggingMixin, RetryMixin, BaseHandler):
    pass
`)
	e := NewPythonExtractor()
	result, err := e.Extract("handler.py", src)
	require.NoError(t, err)

	var bases []string
	for _, ed := range result.Edges {
		if ed.Kind == graph.EdgeExtends {
			bases = append(bases, ed.To)
		}
	}
	assert.ElementsMatch(t, []string{
		"unresolved::LoggingMixin", "unresolved::RetryMixin", "unresolved::BaseHandler",
	}, bases)
}

func TestPyClass_NoBasesEmitsNoInheritanceEdges(t *testing.T) {
	src := []byte(`class Plain:
    pass


class AlsoPlain():
    pass
`)
	e := NewPythonExtractor()
	result, err := e.Extract("plain.py", src)
	require.NoError(t, err)
	assert.Empty(t, edgesOfKind(result.Edges, graph.EdgeExtends))
}

func TestPyClass_NestedClassStillGetsItsBase(t *testing.T) {
	src := []byte(`from django.db import models


class Article(models.Model):
    class Meta(BaseMeta):
        ordering = ["id"]
`)
	e := NewPythonExtractor()
	result, err := e.Extract("article.py", src)
	require.NoError(t, err)

	byFrom := map[string]string{}
	for _, ed := range result.Edges {
		if ed.Kind == graph.EdgeExtends {
			byFrom[ed.From] = ed.To
		}
	}
	assert.Equal(t, "unresolved::Model", byFrom["article.py::Article"])
	assert.Equal(t, "unresolved::BaseMeta", byFrom["article.py::Meta"])
}
