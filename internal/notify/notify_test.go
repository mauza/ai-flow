package notify

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mauza/ai-flow/internal/config"
)

func TestNtfyPublishesJSONWithToken(t *testing.T) {
	var got map[string]any
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		if r.Method != http.MethodPost || r.URL.Path != "/" {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		json.NewDecoder(r.Body).Decode(&got)
	}))
	defer srv.Close()
	t.Setenv("NTFY_TEST_TOKEN", "tk_secret")
	n := New(config.Notify{Ntfy: &config.Ntfy{URL: srv.URL + "/", Topic: "alerts", TokenEnv: "NTFY_TEST_TOKEN"}})
	err := n.Send(context.Background(), Message{Title: "MAU-22: approve needs a decision", Body: "Ship it?", Click: "http://ai-flow:8080/runs/r-1", Priority: 4, Tags: []string{"raised_hand"}})
	if err != nil {
		t.Fatal(err)
	}
	if auth != "Bearer tk_secret" {
		t.Errorf("auth %q", auth)
	}
	if got["topic"] != "alerts" || got["title"] != "MAU-22: approve needs a decision" || got["message"] != "Ship it?" || got["click"] != "http://ai-flow:8080/runs/r-1" || got["priority"] != 4.0 {
		t.Errorf("body %v", got)
	}
}

func TestNtfyReportsRejection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
	}))
	defer srv.Close()
	err := New(config.Notify{Ntfy: &config.Ntfy{URL: srv.URL, Topic: "x"}}).Send(context.Background(), Message{Title: "t"})
	if err == nil || !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), "forbidden") {
		t.Fatalf("err %v", err)
	}
}

func TestOffWithoutBackend(t *testing.T) {
	if New(config.Notify{}) != nil {
		t.Fatal("no backend should mean no sender")
	}
}
