package hooks

import (
	"strings"
	"testing"
)

func TestRulePreambleRoutesExplicitFileReadsDirectly(t *testing.T) {
	got := rulePreamble()
	direct := `read(operation:"file", target:{file:"<path>"})`
	localize := "`mcp__gortex__explore` (never a bare `explore`) with `operation:\"localize\"`"

	if !strings.Contains(got, "explicitly named file") || !strings.Contains(got, direct) {
		t.Fatalf("rule preamble must route explicit file reads directly; got %q", got)
	}
	if !strings.Contains(got, "do not start localization") || !strings.Contains(got, localize) {
		t.Fatalf("rule preamble must reserve localization for discovery; got %q", got)
	}
	if strings.Contains(got, "Call `explore` first for every code task") {
		t.Fatalf("rule preamble still contains the overbroad localization rule; got %q", got)
	}
}
