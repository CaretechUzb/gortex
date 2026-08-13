package profiles

import (
	"strings"
	"testing"
)

func TestPolicyBodiesRouteExplicitFileReadsDirectly(t *testing.T) {
	bodies := map[string]string{
		"explore opener":   sectionExploreOpener,
		"compact workflow": sectionCompactWorkflow,
		"full rule table":  sectionFullRuleTable,
	}
	direct := `read(operation:"file", target:{file:"<path>"}, options:{new_user_task:true})`
	localize := `explore(operation:"localize")`

	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			directAt := strings.Index(body, direct)
			localizeAt := strings.Index(body, localize)
			if directAt < 0 || localizeAt < 0 {
				t.Fatalf("policy must describe direct file reads and discovery localization; got %q", body)
			}
			if directAt > localizeAt {
				t.Fatalf("direct explicit-file routing must precede localization guidance; got %q", body)
			}
		})
	}
}
