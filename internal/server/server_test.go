package server

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"testing/fstest"

	"github.com/mauza/ai-flow/internal/app"
	"github.com/mauza/ai-flow/internal/config"
	"github.com/mauza/ai-flow/internal/hub"
	"github.com/mauza/ai-flow/internal/store"
)

func newTestServer(t *testing.T, token string) http.Handler {
	t.Helper()
	cfg, err := config.Load("../../deploy/config")
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		t.Setenv("TEST_AI_FLOW_TOKEN", token)
		cfg.Env.Server.AuthTokenEnv = "TEST_AI_FLOW_TOKEN"
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	a := &app.App{Cfg: cfg, Store: st, Hub: hub.New()}
	ui := fstest.MapFS{"index.html": {Data: []byte("<html>ui</html>")}}
	return New(a, nil, ui).Handler()
}

func do(h http.Handler, method, path string, cookie *http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestNoTokenMeansOpen(t *testing.T) {
	h := newTestServer(t, "")
	if rec := do(h, "GET", "/api/tasks", nil); rec.Code != 200 {
		t.Fatalf("GET /api/tasks: %d", rec.Code)
	}
}

func TestTokenLoginFlow(t *testing.T) {
	h := newTestServer(t, "sekret")
	if rec := do(h, "GET", "/api/tasks", nil); rec.Code != 401 {
		t.Fatalf("unauthenticated API: %d", rec.Code)
	}
	if rec := do(h, "GET", "/api/tasks?token=sekret", nil); rec.Code != 401 {
		t.Fatal("tokens in API query strings must not authenticate")
	}
	// the UI page is served (it shows the "needs a token" screen itself)
	if rec := do(h, "GET", "/flows/x", nil); rec.Code != 200 {
		t.Fatalf("SPA page: %d", rec.Code)
	}
	bad := do(h, "GET", "/?token=wrong", nil)
	if len(bad.Result().Cookies()) != 0 {
		t.Fatal("wrong token set a cookie")
	}
	rec := do(h, "GET", "/runs/abc?token=sekret", nil)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/runs/abc" {
		t.Fatalf("login redirect: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].HttpOnly {
		t.Fatalf("cookie %+v", cookies)
	}
	if rec := do(h, "GET", "/api/tasks", cookies[0]); rec.Code != 200 {
		t.Fatalf("API with cookie: %d", rec.Code)
	}
}
