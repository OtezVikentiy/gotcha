package web

import "testing"

// TestCanonicalEndpointSort: пустой/незнакомый sort канонизируется в
// throughput — фактический дефолт sortEndpointStats, — чтобы таблица честно
// показывала дефолтную сортировку aria-sort'ом и стрелкой (QA MINOR-5).
func TestCanonicalEndpointSort(t *testing.T) {
	cases := map[string]string{
		"":           "throughput",
		"bogus":      "throughput",
		"throughput": "throughput",
		"name":       "name",
		"p50":        "p50",
		"p75":        "p75",
		"p95":        "p95",
		"p99":        "p99",
		"failure":    "failure",
		"apdex":      "apdex",
	}
	for in, want := range cases {
		if got := canonicalEndpointSort(in); got != want {
			t.Errorf("canonicalEndpointSort(%q) = %q, want %q", in, got, want)
		}
	}
}
