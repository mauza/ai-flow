package broker

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/mauza/ai-flow/internal/config"
	"github.com/mauza/ai-flow/internal/grant"
)

const zeroSHA = "0000000000000000000000000000000000000000"

// git proxies git smart-HTTP for /git/<host>/<owner>/<repo>.git/<service>.
// Pods authenticate with their grant; the proxy checks the repo and, for
// pushes, that every ref update targets the run branch and deletes nothing.
// The upstream token never leaves the control plane.
func (b *Broker) git(w http.ResponseWriter, r *http.Request) {
	claims, err := b.gitClaims(r)
	if err != nil {
		w.Header().Set("WWW-Authenticate", `Basic realm="ai-flow"`)
		http.Error(w, err.Error(), 401)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/git/")
	i := strings.Index(path, ".git/")
	if i < 0 {
		http.Error(w, "expected /git/<host>/<owner>/<repo>.git/...", 404)
		return
	}
	repo, rest := path[:i], path[i+len(".git/"):]
	if repo != claims.Repo {
		http.Error(w, fmt.Sprintf("repository %s is not granted to this node", repo), 403)
		return
	}
	host, _, _ := strings.Cut(repo, "/")
	hostCfg, ok := b.cfg.Env.Git.Hosts[host]
	if !ok {
		http.Error(w, "no credentials configured for "+host, 403)
		return
	}

	push := rest == "git-receive-pack" || (rest == "info/refs" && r.URL.Query().Get("service") == "git-receive-pack")
	switch rest {
	case "info/refs", "git-upload-pack", "git-receive-pack":
	default:
		http.Error(w, "unsupported git endpoint", 404)
		return
	}
	if push && !claims.Write {
		http.Error(w, "this node has read-only access to "+repo, 403)
		return
	}

	var body io.Reader = r.Body
	if rest == "git-receive-pack" {
		data, err := io.ReadAll(io.LimitReader(r.Body, 512<<20))
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if err := checkPush(data, r.Header.Get("Content-Encoding"), claims.Branch); err != nil {
			slog.Warn("push refused", "run", claims.Run, "node", claims.Node, "err", err)
			cmds, caps, perr := parsePush(data, r.Header.Get("Content-Encoding"))
			if perr != nil || len(cmds) == 0 {
				http.Error(w, "push refused: "+err.Error(), 403)
				return
			}
			// Answer in git's own protocol so the client prints
			// "! [remote rejected] <ref> (<reason>)" instead of a bare HTTP 403.
			w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
			w.Write(rejectReport(cmds, caps, err.Error()))
			return
		}
		body = bytes.NewReader(data)
	}

	scheme := hostCfg.Scheme
	if scheme == "" {
		scheme = "https"
	}
	target := fmt.Sprintf("%s://%s.git/%s", scheme, repo, rest)
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, target, body)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	for _, h := range []string{"Content-Type", "Accept", "Content-Encoding", "Git-Protocol", "User-Agent"} {
		if v := r.Header.Get(h); v != "" {
			req.Header.Set(h, v)
		}
	}
	user := hostCfg.Username
	if user == "" {
		user = "x-access-token"
	}
	if tok := config.Secret(hostCfg.TokenEnv); tok != "" {
		req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(user+":"+tok)))
	}
	resp, err := b.http.Do(req)
	if err != nil {
		http.Error(w, "upstream: "+err.Error(), 502)
		return
	}
	defer resp.Body.Close()
	for _, h := range []string{"Content-Type", "Cache-Control", "Expires", "Pragma"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(flushWriter{w}, resp.Body)
}

func (b *Broker) gitClaims(r *http.Request) (*grant.Claims, error) {
	auth := r.Header.Get("Authorization")
	var tok string
	switch {
	case strings.HasPrefix(auth, "Bearer "):
		tok = strings.TrimPrefix(auth, "Bearer ")
	case strings.HasPrefix(auth, "Basic "):
		raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(auth, "Basic "))
		if err != nil {
			return nil, fmt.Errorf("bad basic auth")
		}
		_, tok, _ = strings.Cut(string(raw), ":")
	default:
		return nil, fmt.Errorf("missing grant")
	}
	return b.signer.Verify(tok, "grant")
}

// pushCmd is one ref update: old sha, new sha, ref name.
type pushCmd struct{ old, new, ref string }

// parsePush reads the ref-update commands at the start of a receive-pack
// request: "<old> <new> <ref>\x00<caps>" pkt-lines until a flush packet.
func parsePush(data []byte, encoding string) ([]pushCmd, string, error) {
	raw := data
	if strings.EqualFold(encoding, "gzip") {
		zr, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, "", err
		}
		raw, err = io.ReadAll(io.LimitReader(zr, 1<<20)) // commands are at the front
		if err != nil && err != io.ErrUnexpectedEOF {
			return nil, "", err
		}
	}
	var cmds []pushCmd
	caps := ""
	for len(raw) >= 4 {
		n, err := strconv.ParseUint(string(raw[:4]), 16, 16)
		if err != nil {
			return nil, "", fmt.Errorf("bad pkt-line")
		}
		if n == 0 {
			break // flush: end of commands
		}
		if int(n) > len(raw) || n < 4 {
			return nil, "", fmt.Errorf("truncated pkt-line")
		}
		line := string(raw[4:n])
		raw = raw[n:]
		line, c, hasCaps := strings.Cut(line, "\x00")
		if hasCaps && caps == "" {
			caps = strings.TrimSpace(c)
		}
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "shallow ") || strings.HasPrefix(line, "push-cert") {
			continue
		}
		f := strings.Fields(line)
		if len(f) != 3 {
			return nil, "", fmt.Errorf("unexpected command %q", line)
		}
		cmds = append(cmds, pushCmd{f[0], f[1], f[2]})
	}
	return cmds, caps, nil
}

// checkPush allows only non-deleting updates of the run branch.
func checkPush(data []byte, encoding, branch string) error {
	if branch == "" {
		return fmt.Errorf("no run branch in grant")
	}
	cmds, _, err := parsePush(data, encoding)
	if err != nil {
		return err
	}
	if len(cmds) == 0 {
		return fmt.Errorf("no ref updates found")
	}
	want := "refs/heads/" + branch
	for _, c := range cmds {
		if c.ref != want {
			return fmt.Errorf("only %s may be pushed (got %s)", want, c.ref)
		}
		if c.new == zeroSHA {
			return fmt.Errorf("deleting %s is not allowed", c.ref)
		}
	}
	return nil
}

func pktLine(s string) []byte { return []byte(fmt.Sprintf("%04x%s", len(s)+4, s)) }

// rejectReport builds a receive-pack report-status that rejects every ref,
// wrapped in side-band 1 when the client asked for it.
func rejectReport(cmds []pushCmd, caps, reason string) []byte {
	reason = strings.NewReplacer("\n", " ", "\x00", " ").Replace(reason)
	var report bytes.Buffer
	report.Write(pktLine("unpack ok\n"))
	for _, c := range cmds {
		report.Write(pktLine(fmt.Sprintf("ng %s %s\n", c.ref, reason)))
	}
	report.WriteString("0000")
	if !strings.Contains(caps, "side-band") {
		return report.Bytes()
	}
	var out bytes.Buffer
	data := report.Bytes()
	for len(data) > 0 {
		n := min(len(data), 65515)
		out.Write(pktLine("\x01" + string(data[:n])))
		data = data[n:]
	}
	out.WriteString("0000")
	return out.Bytes()
}

type flushWriter struct{ w http.ResponseWriter }

func (f flushWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if fl, ok := f.w.(http.Flusher); ok {
		fl.Flush()
	}
	return n, err
}

func monthStart() int64 {
	now := time.Now()
	return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location()).UnixMilli()
}
