package runner

import (
	"testing"

	"github.com/mauza/ai-flow/internal/flow"
)

func TestClassifyGatewayErrors(t *testing.T) {
	for _, c := range []struct {
		status int
		body   string
		want   string
	}{
		{500, `{"error":{"message":"litellm.APIConnectionError: OpenAIException - Connection error.","code":"500"}}`, flow.LimitRateLimited},
		{500, `{"error":{"message":"litellm.InternalServerError: [Errno 111] Connection refused"}}`, flow.LimitRateLimited},
		{500, `{"error":{"message":"litellm.ServiceUnavailableError: no healthy deployments"}}`, flow.LimitRateLimited},
		{500, `{"error":{"message":"litellm.BadRequestError: invalid tool schema"}}`, ""},
		{503, `upstream down`, flow.LimitRateLimited},
		{400, `{"error":{"message":"This model's maximum context length is 65536 tokens"}}`, flow.LimitContextExceeded},
	} {
		if got := classify(c.status, []byte(c.body), nil).kind; got != c.want {
			t.Errorf("%d %s: got %q, want %q", c.status, c.body, got, c.want)
		}
	}
}
