package resolve

import (
	"path/filepath"
	"testing"

	"github.com/mauza/ai-flow/internal/config"
)

// The example templates people copy must stay valid flows.
func TestExampleTemplatesValidate(t *testing.T) {
	cfg, err := config.Load("../../examples/templates/config")
	if err != nil {
		t.Fatal(err)
	}
	paths, _ := filepath.Glob("../../examples/templates/*.yaml")
	if len(paths) == 0 {
		t.Fatal("no templates found")
	}
	for _, p := range paths {
		r := Resolve(loadFlow(t, p), cfg)
		for _, is := range Validate(r, cfg) {
			if is.Severity == Error {
				t.Errorf("%s: %+v", filepath.Base(p), is)
			}
		}
	}
}
