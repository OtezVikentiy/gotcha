package web

import (
	"net/http/httptest"
	"testing"
)

// TestNavOrigin — источник перехода приходит из адреса, поэтому сверяется со
// списком известных подразделов: страницы, общие для нескольких разделов
// (эндпойнт, трейс), подсвечиваются по нему, и произвольная строка не должна
// на это влиять.
func TestNavOrigin(t *testing.T) {
	cases := []struct {
		url  string
		want string
	}{
		{"/projects/7/performance/GET%20%2F?from=web-vitals", "web-vitals"},
		{"/traces/abc?from=perf-issue&from_id=218", "perf-issue"},
		{"/traces/abc?from=issue&from_id=42", "issue"},
		{"/traces/abc?from=endpoint&from_id=GET+%2F", "endpoint"},
		// Прямой заход и мусор из адреса на навигацию не влияют.
		{"/traces/abc", ""},
		{"/traces/abc?from=whatever", ""},
		{"/traces/abc?from=%3Cscript%3E", ""},
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", c.url, nil)
		if got := navOrigin(r); got != c.want {
			t.Errorf("navOrigin(%q) = %q, want %q", c.url, got, c.want)
		}
	}
}

// TestEndpointOrigin — в шаблон попадает только известное значение.
func TestEndpointOrigin(t *testing.T) {
	if got := endpointOrigin("web-vitals"); got != "web-vitals" {
		t.Errorf("endpointOrigin(web-vitals) = %q", got)
	}
	for _, in := range []string{"", "transactions", "<script>"} {
		if got := endpointOrigin(in); got != "" {
			t.Errorf("endpointOrigin(%q) = %q, ожидалась пустая строка", in, got)
		}
	}
}
