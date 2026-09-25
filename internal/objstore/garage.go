package objstore

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// garageAdmin talks to Garage's admin API (v2).
type garageAdmin struct {
	base  string
	token string
	http  *http.Client
}

type garageErr struct {
	status int
	body   string
}

func (e *garageErr) Error() string { return fmt.Sprintf("garage admin: HTTP %d: %s", e.status, strings.TrimSpace(e.body)) }

func (g *garageAdmin) call(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, g.base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+g.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := g.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return &garageErr{resp.StatusCode, string(raw)}
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

// bootstrapGarage makes a fresh single-node Garage usable: assigns the layout,
// creates the access key and bucket, and returns the key. Idempotent.
func bootstrapGarage(ctx context.Context, adminURL, token, keyName, bucket string) (string, string, error) {
	g := &garageAdmin{base: strings.TrimSuffix(adminURL, "/"), token: token, http: &http.Client{Timeout: 20 * time.Second}}

	var status struct {
		LayoutVersion int64 `json:"layoutVersion"`
		Nodes         []struct {
			ID   string          `json:"id"`
			Role json.RawMessage `json:"role"`
		} `json:"nodes"`
	}
	deadline := time.Now().Add(3 * time.Minute)
	for {
		err := g.call(ctx, "GET", "/v2/GetClusterStatus", nil, &status)
		if err == nil && len(status.Nodes) > 0 {
			break
		}
		if time.Now().After(deadline) {
			return "", "", fmt.Errorf("garage not reachable at %s: %v", adminURL, err)
		}
		slog.Info("waiting for garage", "err", err)
		select {
		case <-ctx.Done():
			return "", "", ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}

	unassigned := false
	for _, n := range status.Nodes {
		if len(n.Role) == 0 || string(n.Role) == "null" {
			unassigned = true
		}
	}
	if unassigned {
		var roles []map[string]any
		for _, n := range status.Nodes {
			if len(n.Role) == 0 || string(n.Role) == "null" {
				roles = append(roles, map[string]any{"id": n.ID, "zone": "dc1", "capacity": int64(100) << 30, "tags": []string{}})
			}
		}
		var layout struct {
			Version int64 `json:"version"`
		}
		if err := g.call(ctx, "POST", "/v2/UpdateClusterLayout", map[string]any{"roles": roles}, &layout); err != nil {
			return "", "", err
		}
		if err := g.call(ctx, "POST", "/v2/ApplyClusterLayout", map[string]any{"version": layout.Version + 1}, nil); err != nil {
			return "", "", err
		}
		slog.Info("garage layout applied", "nodes", len(roles))
	}

	var key struct {
		AccessKeyID     string `json:"accessKeyId"`
		SecretAccessKey string `json:"secretAccessKey"`
	}
	err := g.call(ctx, "GET", "/v2/GetKeyInfo?showSecretKey=true&search="+url.QueryEscape(keyName), nil, &key)
	if ge, ok := err.(*garageErr); ok && (ge.status == 404 || ge.status == 400) {
		err = g.call(ctx, "POST", "/v2/CreateKey", map[string]any{"name": keyName}, &key)
	}
	if err != nil {
		return "", "", err
	}

	var b struct {
		ID string `json:"id"`
	}
	err = g.call(ctx, "GET", "/v2/GetBucketInfo?globalAlias="+url.QueryEscape(bucket), nil, &b)
	if ge, ok := err.(*garageErr); ok && (ge.status == 404 || ge.status == 400) {
		err = g.call(ctx, "POST", "/v2/CreateBucket", map[string]any{"globalAlias": bucket}, &b)
	}
	if err != nil {
		return "", "", err
	}
	if err := g.call(ctx, "POST", "/v2/AllowBucketKey", map[string]any{
		"bucketId": b.ID, "accessKeyId": key.AccessKeyID,
		"permissions": map[string]bool{"read": true, "write": true, "owner": true},
	}, nil); err != nil {
		return "", "", err
	}
	return key.AccessKeyID, key.SecretAccessKey, nil
}
