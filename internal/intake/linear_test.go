package intake

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mauza/ai-flow/internal/config"
	"github.com/mauza/ai-flow/internal/linear"
)

func TestWebhookKicksPoll(t *testing.T) {
	t.Setenv("TEST_LINEAR_SECRET", "s3cret")
	l := &Linear{cfg: &config.Config{Env: config.Environment{Linear: config.Linear{WebhookSecretEnv: "TEST_LINEAR_SECRET"}}}, kick: make(chan struct{}, 1)}
	h, err := l.WebhookHandler()
	if err != nil {
		t.Fatal(err)
	}
	send := func(body, sig string) int {
		req := httptest.NewRequest("POST", "/webhooks/linear", strings.NewReader(body))
		req.Header.Set("Linear-Signature", sig)
		rec := httptest.NewRecorder()
		h(rec, req)
		return rec.Code
	}
	body := `{"action":"update","type":"Issue","data":{"id":"x"}}`
	mac := hmac.New(sha256.New, []byte("s3cret"))
	mac.Write([]byte(body))
	if code := send(body, "wrong"); code == http.StatusOK {
		t.Error("bad signature accepted")
	}
	select {
	case <-l.kick:
		t.Fatal("bad signature kicked a poll")
	default:
	}
	if code := send(body, hex.EncodeToString(mac.Sum(nil))); code != http.StatusOK {
		t.Fatalf("good signature: %d", code)
	}
	select {
	case <-l.kick:
	case <-time.After(2 * time.Second): // the handler dispatches asynchronously
		t.Fatal("valid Issue webhook did not kick a poll")
	}
}

func TestMatches(t *testing.T) {
	link := &config.LinearLink{}
	link.Trigger.Label = "ai-flow"
	var is linear.IssueDetails
	is.Labels.Nodes = append(is.Labels.Nodes, struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}{"1", "AI-Flow"})
	if !matches(is, link) {
		t.Error("label match should be case-insensitive")
	}
	link.Projects = []string{"Sandbox"}
	if matches(is, link) {
		t.Error("issue without a project matched a project filter")
	}
}
