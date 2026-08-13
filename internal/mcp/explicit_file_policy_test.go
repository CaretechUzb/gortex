package mcp

import (
	"strings"
	"testing"
)

func TestCodingAgentInstructionsRouteExplicitFileReadsDirectly(t *testing.T) {
	direct := `read(operation:"file", target:{file:"<path>"}, options:{new_user_task:true})`
	localize := `explore(operation:"localize")`
	directAt := strings.Index(codingAgentInstructions, direct)
	localizeAt := strings.Index(codingAgentInstructions, localize)

	if directAt < 0 || localizeAt < 0 {
		t.Fatalf("coding-agent instructions must describe direct file reads and discovery localization; got %q", codingAgentInstructions)
	}
	if directAt > localizeAt {
		t.Fatalf("direct explicit-file routing must precede localization guidance; got %q", codingAgentInstructions)
	}
	if !strings.Contains(codingAgentInstructions, "do not localize") {
		t.Fatalf("coding-agent instructions must prevent localization for explicit file reads; got %q", codingAgentInstructions)
	}
	if strings.Contains(codingAgentInstructions, "For files/symbols/evidence/where") {
		t.Fatalf("coding-agent instructions still contain the overbroad localization rule; got %q", codingAgentInstructions)
	}
}
