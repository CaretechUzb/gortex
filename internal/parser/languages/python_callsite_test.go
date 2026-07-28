package languages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestPyCalls_ModuleLevelAndClassBodyAreAttributedToFile(t *testing.T) {
	// Module-level and class-body calls execute at import time. Dropping
	// them made the whole module-level configuration layer — routes, ORM
	// columns, settings, registrations — invisible to find_usages.
	src := []byte(`from flask import Flask
from db import Column, String

app = Flask(__name__)
register(app)


class User:
    name = Column(String)

    def save(self):
        persist(self)
`)
	e := NewPythonExtractor()
	result, err := e.Extract("app.py", src)
	require.NoError(t, err)

	fromFile := map[string]bool{}
	fromMethod := map[string]bool{}
	for _, ed := range result.Edges {
		if ed.Kind != graph.EdgeCalls {
			continue
		}
		switch ed.From {
		case "app.py":
			fromFile[ed.To] = true
		case "app.py::User.save":
			fromMethod[ed.To] = true
		}
	}

	assert.True(t, fromFile["unresolved::extern::flask.Flask::Flask"],
		"module-level `app = Flask(...)` must be attributed to the file node")
	assert.True(t, fromFile["unresolved::register"],
		"module-level `register(app)` must be attributed to the file node")
	assert.True(t, fromFile["unresolved::extern::db.Column::Column"],
		"a call in a class body must be attributed to the file node")
	assert.True(t, fromMethod["unresolved::persist"],
		"calls inside a method still belong to the method, not the file")
}

func TestPyCalls_DecoratorCallIsNotDropped(t *testing.T) {
	// A decorator sits above the `def` line, so it falls outside every
	// function range and used to be dropped along with module-level calls.
	src := []byte(`from app import app


@app.route("/")
def index():
    return "hi"
`)
	e := NewPythonExtractor()
	result, err := e.Extract("routes.py", src)
	require.NoError(t, err)

	var found bool
	for _, ed := range result.Edges {
		if ed.Kind == graph.EdgeCalls && ed.From == "routes.py" && ed.To == "unresolved::extern::app.app::route" {
			found = true
		}
	}
	assert.True(t, found, "the decorator's own call site belongs to the file")
}
