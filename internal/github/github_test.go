package github

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func checksServer(t *testing.T, refuseCheckRuns bool) (*Client, *[]string) {
	t.Helper()
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.URL.Path)
		switch {
		case strings.HasSuffix(r.URL.Path, "/check-runs"):
			if refuseCheckRuns {
				w.WriteHeader(403)
				w.Write([]byte(`{"message":"Resource not accessible by personal access token"}`))
				return
			}
			w.Write([]byte(`{"check_runs":[{"name":"test","status":"completed","conclusion":"success"}]}`))
		case strings.HasSuffix(r.URL.Path, "/status"):
			w.Write([]byte(`{"statuses":[{"context":"deploy/preview","state":"pending"}]}`))
		case r.URL.Path == "/repos/o/app/commits/main":
			w.Write([]byte(`{"sha":"abc123"}`))
		case r.URL.Path == "/repos/o/app/actions/runs":
			if r.URL.Query().Get("head_sha") != "abc123" {
				t.Errorf("runs queried for %q, want the resolved sha", r.URL.Query().Get("head_sha"))
			}
			w.Write([]byte(`{"workflow_runs":[{"id":7},{"id":8}]}`))
		case r.URL.Path == "/repos/o/app/actions/runs/7/jobs":
			w.Write([]byte(`{"jobs":[{"name":"test","status":"completed","conclusion":"success"},{"name":"release","status":"completed","conclusion":"skipped"}]}`))
		case r.URL.Path == "/repos/o/app/actions/runs/8/jobs":
			w.Write([]byte(`{"jobs":[{"name":"web-test","status":"in_progress","conclusion":null}]}`))
		default:
			t.Errorf("unexpected %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	return New(srv.URL, "t"), &calls
}

func names(cs []Check) string {
	var out []string
	for _, c := range cs {
		state := "pending"
		if c.Done {
			state = map[bool]string{true: "ok", false: "failed"}[c.OK]
		}
		out = append(out, c.Name+"="+state)
	}
	return strings.Join(out, ",")
}

func TestChecksUsesCheckRunsWhenAllowed(t *testing.T) {
	c, calls := checksServer(t, false)
	got, err := c.Checks(t.Context(), Repo{Owner: "o", Name: "app"}, "main")
	if err != nil {
		t.Fatal(err)
	}
	if names(got) != "test=ok,deploy/preview=pending" {
		t.Fatalf("got %s", names(got))
	}
	for _, p := range *calls {
		if strings.Contains(p, "/actions/") {
			t.Fatal("no Actions fallback when check runs are readable")
		}
	}
}

// Fine-grained tokens cannot hold the Checks permission; the commit's
// GitHub Actions jobs stand in for its check runs.
func TestChecksFallsBackToActionsJobs(t *testing.T) {
	c, _ := checksServer(t, true)
	got, err := c.Checks(t.Context(), Repo{Owner: "o", Name: "app"}, "main")
	if err != nil {
		t.Fatal(err)
	}
	if names(got) != "test=ok,release=ok,web-test=pending,deploy/preview=pending" {
		t.Fatalf("got %s", names(got))
	}
}
