// Package mcpx connects the control plane to upstream MCP servers. Pods never
// talk to MCP servers directly: the broker lists and calls tools here, with
// the server credentials from the environment config.
package mcpx

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mauza/ai-flow/internal/config"
	"github.com/mauza/ai-flow/internal/protocol"
)

type Tool struct {
	Name        string
	Description string
	InputSchema map[string]any
}

type Pool struct {
	servers map[string]config.MCPServer

	mu    sync.Mutex
	tools map[string]cachedTools
}

type cachedTools struct {
	at    time.Time
	tools []Tool
}

func NewPool(servers map[string]config.MCPServer) *Pool {
	return &Pool{servers: servers, tools: map[string]cachedTools{}}
}

type headerTransport struct {
	headers map[string]string
	base    http.RoundTripper
}

func (h headerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	for k, v := range h.headers {
		r.Header.Set(k, os.ExpandEnv(v))
	}
	return h.base.RoundTrip(r)
}

func (p *Pool) connect(ctx context.Context, server string) (*mcp.ClientSession, error) {
	s, ok := p.servers[server]
	if !ok {
		return nil, fmt.Errorf("unknown MCP server %q", server)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "ai-flow", Version: "v2"}, nil)
	transport := &mcp.StreamableClientTransport{
		Endpoint:   s.URL,
		HTTPClient: &http.Client{Transport: headerTransport{s.Headers, http.DefaultTransport}, Timeout: 5 * time.Minute},
		MaxRetries: -1,
	}
	return client.Connect(ctx, transport, nil)
}

// Tools lists a server's tools (cached for five minutes).
func (p *Pool) Tools(ctx context.Context, server string) ([]Tool, error) {
	p.mu.Lock()
	if c, ok := p.tools[server]; ok && time.Since(c.at) < 5*time.Minute {
		p.mu.Unlock()
		return c.tools, nil
	}
	p.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	sess, err := p.connect(ctx, server)
	if err != nil {
		return nil, err
	}
	defer sess.Close()
	var out []Tool
	var cursor string
	for {
		res, err := sess.ListTools(ctx, &mcp.ListToolsParams{Cursor: cursor})
		if err != nil {
			return nil, err
		}
		for _, t := range res.Tools {
			schema := map[string]any{}
			if raw, err := json.Marshal(t.InputSchema); err == nil {
				json.Unmarshal(raw, &schema)
			}
			out = append(out, Tool{Name: t.Name, Description: t.Description, InputSchema: schema})
		}
		if res.NextCursor == "" {
			break
		}
		cursor = res.NextCursor
	}
	p.mu.Lock()
	p.tools[server] = cachedTools{at: time.Now(), tools: out}
	p.mu.Unlock()
	return out, nil
}

// Call invokes one tool and returns its content in wire form.
func (p *Pool) Call(ctx context.Context, server, tool string, args map[string]any) (*protocol.MCPCallResult, error) {
	sess, err := p.connect(ctx, server)
	if err != nil {
		return nil, err
	}
	defer sess.Close()
	res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		return nil, err
	}
	out := &protocol.MCPCallResult{IsError: res.IsError}
	for _, c := range res.Content {
		raw, err := json.Marshal(c)
		if err != nil {
			continue
		}
		var m map[string]any
		json.Unmarshal(raw, &m)
		out.Content = append(out.Content, m)
	}
	if len(out.Content) == 0 && res.StructuredContent != nil {
		raw, _ := json.Marshal(res.StructuredContent)
		out.Content = append(out.Content, map[string]any{"type": "text", "text": string(raw)})
	}
	return out, nil
}
