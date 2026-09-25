package planner

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/mauza/ai-flow/internal/flow"
)

func TestRenderRoundTrip(t *testing.T) {
	src, err := os.ReadFile("../resolve/testdata/good.yaml")
	if err != nil {
		t.Fatal(err)
	}
	f, err := flow.Parse(src)
	if err != nil {
		t.Fatal(err)
	}
	out, err := Render(f)
	if err != nil {
		t.Fatal(err)
	}
	back, err := flow.Parse([]byte(out))
	if err != nil {
		t.Fatalf("rendered YAML does not parse: %v\n%s", err, out)
	}
	a, _ := json.Marshal(f)
	b, _ := json.Marshal(back)
	if string(a) != string(b) {
		t.Errorf("round trip changed the flow\nbefore: %s\nafter:  %s\nyaml:\n%s", a, b, out)
	}
}

func TestSplitReply(t *testing.T) {
	exp, body := splitReply("Here is the flow.\n\n```yaml\nkind: Flow\n```\nDone.")
	if body != "kind: Flow\n" || exp != "Here is the flow.\n\nDone." {
		t.Errorf("got %q / %q", exp, body)
	}
}
