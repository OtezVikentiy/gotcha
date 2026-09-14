package templates

import "testing"

// trace_id приходит из ingest без проверки на hex (internal/ingest/sentry.go normalizeID) —
// "/", "#", "?" в нём не должны ломать путь ссылки на флеймграф.
func TestTraceFlamePathEscapesTraceID(t *testing.T) {
	cases := []struct {
		name    string
		traceID string
		want    string
	}{
		{"plain hex", "abc123", "/traces/abc123/flame"},
		{"slash", "abc/def", "/traces/abc%2Fdef/flame"},
		{"hash", "abc#def", "/traces/abc%23def/flame"},
		{"question mark", "abc?def", "/traces/abc%3Fdef/flame"},
		{"percent", "abc%2fdef", "/traces/abc%252fdef/flame"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := traceFlamePath(c.traceID); got != c.want {
				t.Fatalf("traceFlamePath(%q) = %q, want %q", c.traceID, got, c.want)
			}
		})
	}
}
