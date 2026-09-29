package broker

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mauza/ai-flow/internal/config"
)

func pkt(s string) string { return fmt.Sprintf("%04x%s", len(s)+4, s) }

const (
	oldSHA = "1111111111111111111111111111111111111111"
	newSHA = "2222222222222222222222222222222222222222"
)

func TestCheckPush(t *testing.T) {
	branch := "ai-flow/fix-abc123"
	cases := []struct {
		name string
		body string
		ok   bool
	}{
		{"run branch", pkt(oldSHA+" "+newSHA+" refs/heads/"+branch+"\x00report-status side-band-64k\n") + "0000PACK", true},
		{"create run branch", pkt(zeroSHA+" "+newSHA+" refs/heads/"+branch+"\x00report-status\n") + "0000", true},
		{"main", pkt(oldSHA+" "+newSHA+" refs/heads/main\x00report-status\n") + "0000", false},
		{"sneaky second ref", pkt(oldSHA+" "+newSHA+" refs/heads/"+branch+"\x00caps\n") + pkt(oldSHA+" "+newSHA+" refs/heads/main\n") + "0000", false},
		{"tag", pkt(zeroSHA+" "+newSHA+" refs/tags/v1\x00caps\n") + "0000", false},
		{"delete run branch", pkt(oldSHA+" "+zeroSHA+" refs/heads/"+branch+"\x00caps\n") + "0000", false},
		{"prefix trick", pkt(oldSHA+" "+newSHA+" refs/heads/"+branch+"-evil\x00caps\n") + "0000", false},
		{"shallow first", pkt("shallow "+oldSHA+"\n") + pkt(oldSHA+" "+newSHA+" refs/heads/"+branch+"\x00caps\n") + "0000", true},
		{"no commands", "0000", false},
		{"garbage", "zzzz", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := checkPush([]byte(c.body), "", branch)
			if (err == nil) != c.ok {
				t.Errorf("ok=%v, err=%v", c.ok, err)
			}
		})
	}
}

func TestCheckPushGzip(t *testing.T) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zw.Write([]byte(pkt(oldSHA+" "+newSHA+" refs/heads/main\x00caps\n") + "0000"))
	zw.Close()
	if err := checkPush(buf.Bytes(), "gzip", "b"); err == nil || !strings.Contains(err.Error(), "only refs/heads/b") {
		t.Errorf("gzip body not inspected: %v", err)
	}
}

func TestPushRequiresCompleteCommandSection(t *testing.T) {
	valid := pkt(oldSHA + " " + newSHA + " refs/heads/b\x00report-status\n")
	for _, body := range []string{valid, valid + "0", valid + "000", valid + "0010short", valid + "0001", valid + "zzzz"} {
		for _, encoding := range []string{"", "gzip"} {
			data := []byte(body)
			if encoding == "gzip" {
				data = gzipPush(t, data)
			}
			if err := checkPush(data, encoding, "b"); err == nil {
				t.Fatalf("accepted unterminated section %q (%s)", body, encoding)
			}
		}
	}
	for _, encoding := range []string{"br", "deflate", "gzip, identity", "identity, gzip"} {
		if err := checkPush([]byte(valid+"0000"), encoding, "b"); err == nil {
			t.Errorf("accepted unsupported encoding %q", encoding)
		}
	}
	for _, encoding := range []string{"", "identity", "gzip"} {
		// End an allowed packet exactly at the old decompression limit, then
		// hide an unauthorized ref beyond it. A partial-packet fixture alone
		// would not reproduce the old successful-prefix authorization bug.
		command := pkt(oldSHA + " " + newSHA + " refs/heads/b\n")
		padding := (maxPushCommandBytes - len(valid)) % len(command)
		first := pkt(oldSHA + " " + newSHA + " refs/heads/b\x00report-status" + strings.Repeat(" ", padding) + "\n")
		body := first + strings.Repeat(command, (maxPushCommandBytes-len(first))/len(command))
		if len(body) != maxPushCommandBytes {
			t.Fatal("fixture does not align with decompression boundary")
		}
		body += pkt(oldSHA+" "+newSHA+" refs/heads/main\n") + "0000"
		data := []byte(body)
		if encoding == "gzip" {
			data = gzipPush(t, data)
		}
		if _, _, err := parsePush(data, encoding); err == nil {
			t.Errorf("accepted over-limit command section (%s)", encoding)
		}
	}
	// Pack bytes aren't part of the command budget.
	data := gzipPush(t, []byte(valid+"0000PACK"+strings.Repeat("x", 2*maxPushCommandBytes)))
	if err := checkPush(data, "gzip", "b"); err != nil {
		t.Fatalf("valid push with large pack: %v", err)
	}
	data = gzipPush(t, []byte(valid))
	if err := checkPush(data[:len(data)-6], "gzip", "b"); err == nil {
		t.Fatal("accepted incomplete gzip command stream")
	}
}

func gzipPush(t *testing.T, data []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	w := gzip.NewWriter(&out)
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

type gitTestTransport func(*http.Request) (*http.Response, error)

func (f gitTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestGitOnlyForwardsFullyAuthorizedPush(t *testing.T) {
	b, _, token := runtimeBroker(t)
	b.cfg.Env.Git.Hosts = map[string]config.GitHost{"example.invalid": {Scheme: "https"}}
	forwarded := 0
	b.http = &http.Client{Transport: gitTestTransport(func(*http.Request) (*http.Response, error) {
		forwarded++
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("upstream"))}, nil
	})}
	prefix := pkt(oldSHA + " " + newSHA + " refs/heads/b\x00report-status\n")
	for _, tc := range []struct {
		body, encoding string
		allow          bool
	}{
		{prefix + "0000PACK", "", true},
		{prefix, "", false},
		{prefix + "000", "", false},
		{prefix + "0000PACK", "br", false},
		{prefix + pkt(oldSHA+" "+newSHA+" refs/heads/main\n") + "0000PACK", "", false},
	} {
		before := forwarded
		r := httptest.NewRequest("POST", "/git/example.invalid/o/r.git/git-receive-pack", strings.NewReader(tc.body))
		r.SetBasicAuth("node", token)
		r.Header.Set("Content-Encoding", tc.encoding)
		w := httptest.NewRecorder()
		b.Handler().ServeHTTP(w, r)
		if (forwarded == before+1) != tc.allow {
			t.Fatalf("forwarded=%d status=%d body=%q", forwarded-before, w.Code, tc.body)
		}
	}
}
