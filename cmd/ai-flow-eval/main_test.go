package main

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestCLIExitCodesAndJSON(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		code int
		json bool
	}{
		{"passing", []string{"-suite", "../../examples/evals/baseline-suite.json"}, 0, true},
		{"scoring failure", []string{"-suite", "../../examples/evals/candidate-suite.json"}, 1, true},
		{"missing suite", nil, 2, false},
		{"bad file", []string{"-suite", "missing.json"}, 2, false},
		{"bad baseline", []string{"-suite", "../../examples/evals/baseline-suite.json", "-baseline", "missing.json"}, 2, false},
		{"extra argument", []string{"-suite", "../../examples/evals/baseline-suite.json", "ignored"}, 2, false},
		{"help", []string{"-h"}, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := run(tc.args, &stdout, &stderr); code != tc.code {
				t.Fatalf("exit %d; want %d: %s", code, tc.code, stderr.String())
			}
			if tc.json && !json.Valid(stdout.Bytes()) {
				t.Fatalf("stdout must contain only JSON: %s", stdout.String())
			}
			if !tc.json && stdout.Len() != 0 {
				t.Fatal("input errors/help should not emit a report")
			}
		})
	}
}
