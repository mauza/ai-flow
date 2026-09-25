// Package mcpdemo is a tiny MCP server for trying MCP grants locally
// (`ai-flow mcp-demo`). It has two tools so a grant can allow one and not the other.
package mcpdemo

import (
	"context"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type WordStatsIn struct {
	Text string `json:"text" jsonschema:"the text to analyse"`
}

type WordStatsOut struct {
	Words       int      `json:"words"`
	UniqueWords int      `json:"unique_words"`
	TopWords    []string `json:"top_words"`
}

type TimeIn struct{}

type TimeOut struct {
	UTC string `json:"utc"`
}

var wordRe = regexp.MustCompile(`[A-Za-z0-9']+`)

func wordStats(_ context.Context, _ *mcp.CallToolRequest, in WordStatsIn) (*mcp.CallToolResult, WordStatsOut, error) {
	counts := map[string]int{}
	words := wordRe.FindAllString(strings.ToLower(in.Text), -1)
	for _, w := range words {
		counts[w]++
	}
	uniq := make([]string, 0, len(counts))
	for w := range counts {
		uniq = append(uniq, w)
	}
	sort.Slice(uniq, func(i, j int) bool {
		if counts[uniq[i]] != counts[uniq[j]] {
			return counts[uniq[i]] > counts[uniq[j]]
		}
		return uniq[i] < uniq[j]
	})
	if len(uniq) > 5 {
		uniq = uniq[:5]
	}
	return nil, WordStatsOut{Words: len(words), UniqueWords: len(counts), TopWords: uniq}, nil
}

func now(_ context.Context, _ *mcp.CallToolRequest, _ TimeIn) (*mcp.CallToolResult, TimeOut, error) {
	return nil, TimeOut{UTC: time.Now().UTC().Format(time.RFC3339)}, nil
}

// Server builds the demo server.
func Server() *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "ai-flow-demo", Version: "v1"}, nil)
	mcp.AddTool(s, &mcp.Tool{Name: "word_stats", Description: "Count words in a text and list the most common ones."}, wordStats)
	mcp.AddTool(s, &mcp.Tool{Name: "time_now", Description: "The current time in UTC."}, now)
	return s
}

// Handler serves the demo over streamable HTTP.
func Handler() http.Handler {
	s := Server()
	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, nil)
}
