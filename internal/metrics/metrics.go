// Package metrics reads a Prometheus-compatible query API (VictoriaMetrics):
// the engine's health checks and the product metrics on story maps.
package metrics

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// DefaultClient is used when a caller passes a nil client.
var DefaultClient = &http.Client{Timeout: 20 * time.Second}

type response struct {
	Status string `json:"status"`
	Error  string `json:"error"`
	Data   struct {
		ResultType string          `json:"resultType"`
		Result     json.RawMessage `json:"result"`
	} `json:"data"`
}

func get(ctx context.Context, c *http.Client, base, path string, q url.Values) (*response, error) {
	if base == "" {
		return nil, fmt.Errorf("metrics.url is not configured")
	}
	if c == nil {
		c = DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, "GET", strings.TrimSuffix(base, "/")+path+"?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	var out response
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("metrics: HTTP %d", resp.StatusCode)
	}
	if out.Status != "success" {
		return nil, errors.New(out.Error) // the backend's own message
	}
	return &out, nil
}

func parse(v any) (float64, bool) {
	s, _ := v.(string)
	f, err := strconv.ParseFloat(s, 64)
	return f, err == nil && !math.IsNaN(f) && !math.IsInf(f, 0)
}

// Instant runs an instant query and returns one value per series (a scalar
// is one value; NaN and unparsable values are left out).
func Instant(ctx context.Context, c *http.Client, base, q string) ([]float64, error) {
	resp, err := get(ctx, c, base, "/api/v1/query", url.Values{"query": {q}})
	if err != nil {
		return nil, err
	}
	var raw [][2]any
	switch resp.Data.ResultType {
	case "vector":
		var series []struct {
			Value [2]any `json:"value"`
		}
		json.Unmarshal(resp.Data.Result, &series)
		for _, s := range series {
			raw = append(raw, s.Value)
		}
	case "scalar":
		var v [2]any
		json.Unmarshal(resp.Data.Result, &v)
		raw = append(raw, v)
	default:
		return nil, fmt.Errorf("metrics: unsupported result type %q", resp.Data.ResultType)
	}
	var out []float64
	for _, v := range raw {
		if f, ok := parse(v[1]); ok {
			out = append(out, f)
		}
	}
	return out, nil
}

// Range runs a range query over the last window seconds and returns
// [unix seconds, value] points summed across series.
func Range(ctx context.Context, c *http.Client, base, q string, window, step int64) ([][2]float64, error) {
	end := time.Now().Unix()
	resp, err := get(ctx, c, base, "/api/v1/query_range", url.Values{
		"query": {q}, "start": {strconv.FormatInt(end-window, 10)}, "end": {strconv.FormatInt(end, 10)}, "step": {strconv.FormatInt(step, 10)},
	})
	if err != nil {
		return nil, err
	}
	var series []struct {
		Values [][2]any `json:"values"`
	}
	if err := json.Unmarshal(resp.Data.Result, &series); err != nil {
		return nil, err
	}
	sums := map[float64]float64{}
	var times []float64
	for _, s := range series {
		for _, v := range s.Values {
			t, _ := v[0].(float64)
			f, ok := parse(v[1])
			if !ok {
				continue
			}
			if _, seen := sums[t]; !seen {
				times = append(times, t)
			}
			sums[t] += f
		}
	}
	sort.Float64s(times)
	out := make([][2]float64, 0, len(times))
	for _, t := range times {
		out = append(out, [2]float64{t, sums[t]})
	}
	return out, nil
}
