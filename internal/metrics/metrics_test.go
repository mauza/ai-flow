package metrics_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mauza/ai-flow/internal/metrics"
)

func TestInstantAndRange(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/query":
			w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"value":[1,"2.5"]},{"value":[1,"NaN"]},{"value":[1,"-4"]}]}}`))
		case "/api/v1/query_range":
			w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[{"values":[[20,"1"],[10,"2"]]},{"values":[[10,"3"]]}]}}`))
		}
	}))
	defer srv.Close()
	values, err := metrics.Instant(t.Context(), nil, srv.URL, "q")
	if err != nil || len(values) != 2 || values[0] != 2.5 || values[1] != -4 {
		t.Fatalf("instant: %v %v", values, err)
	}
	series, err := metrics.Range(t.Context(), nil, srv.URL, "q", 3600, 10)
	if err != nil || len(series) != 2 || series[0] != [2]float64{10, 5} || series[1] != [2]float64{20, 1} {
		t.Fatalf("range (summed per timestamp, oldest first): %v %v", series, err)
	}
	if _, err := metrics.Instant(t.Context(), nil, "", "q"); err == nil {
		t.Fatal("an unconfigured URL must be an error")
	}
}
