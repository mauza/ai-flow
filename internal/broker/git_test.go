package broker

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"strings"
	"testing"
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
