package hooks

import "testing"

func TestCodexLocalizationLifecycleRecognizesBothMCPNamespaces(t *testing.T) {
	for _, prefix := range []string{gortexMCPToolPrefix, gortexCodexMCPToolPrefix} {
		for _, operation := range []string{"explore", "search", "read", "relations", "trace", "analyze"} {
			tool := prefix + operation
			if !localizationNavigationTool(tool) {
				t.Errorf("localization navigation does not recognize %q", tool)
			}
			if !codexLocalizationPreToolUseTool(tool) {
				t.Errorf("Codex PreToolUse does not recognize %q", tool)
			}
		}
	}
}
