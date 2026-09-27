package flow

import (
	"encoding/json"
	"testing"
	"time"
)

func TestDurationRoundTrip(t *testing.T) {
	for _, d := range []time.Duration{30 * time.Second, 90 * time.Second, 30 * time.Minute, 2 * time.Hour, 48 * time.Hour, 150 * time.Minute, 1500 * time.Millisecond} {
		b, err := json.Marshal(Duration{d})
		if err != nil {
			t.Fatal(err)
		}
		var back Duration
		if err := json.Unmarshal(b, &back); err != nil {
			t.Fatalf("%s: %v", b, err)
		}
		if back.Duration != d {
			t.Errorf("%v → %s → %v", d, b, back.Duration)
		}
	}
	var d Duration
	if err := json.Unmarshal([]byte(`"2d"`), &d); err != nil || d.Duration != 48*time.Hour {
		t.Errorf("2d → %v %v", d.Duration, err)
	}
}
