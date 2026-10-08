package eval

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/mauza/ai-flow/internal/config"
	"github.com/mauza/ai-flow/internal/flow"
	"github.com/mauza/ai-flow/internal/resolve"
)

// Exercise the actual parser/resolver/validator, without launching nodes or
// connecting to endpoints. Fixtures intentionally use non-runnable resources.
func TestTemplatesValidate(t *testing.T) {
	const root = "../../examples/templates"
	cfg, err := config.Load(filepath.Join(root, "config"))
	if err != nil {
		t.Fatal(err)
	}
	paths, err := filepath.Glob(filepath.Join(root, "*.yaml"))
	if err != nil || len(paths) != 4 {
		t.Fatalf("want four templates: %v %v", paths, err)
	}
	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			f, err := flow.Parse(data)
			if err != nil {
				t.Fatal(err)
			}
			for _, issue := range resolve.Validate(resolve.Resolve(f, cfg), cfg) {
				t.Error(issue)
			}
		})
	}
}
