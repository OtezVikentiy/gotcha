package web

import (
	"net/http/httptest"
	"testing"
)

// TestTraceOrigin — трейс открывается из трёх мест (проблема
// производительности, эндпойнт, событие issue), и крошка должна возвращать
// туда, откуда пришли. Раньше она всегда вела в список транзакций, причём
// подписана была «Производительность» — то есть называла область, а вела в
// подраздел.
//
// Источник приходит из адреса, поэтому проверяется: неизвестное значение и
// битый идентификатор игнорируются.
func TestTraceOrigin(t *testing.T) {
	cases := []struct {
		url      string
		origin   string
		id       int64
		txn      string
	}{
		{"/traces/abc?from=perf-issue&from_id=218", "perf-issue", 218, ""},
		{"/traces/abc?from=issue&from_id=42", "issue", 42, ""},
		{"/traces/abc?from=endpoint&from_id=GET%20%2Fapi%2Fuser", "endpoint", 0, "GET /api/user"},
		// Прямой заход.
		{"/traces/abc", "", 0, ""},
		// Мусор из адреса не должен влиять на навигацию.
		{"/traces/abc?from=whatever&from_id=1", "", 0, ""},
		{"/traces/abc?from=perf-issue&from_id=not-a-number", "", 0, ""},
		{"/traces/abc?from=perf-issue&from_id=-5", "", 0, ""},
		{"/traces/abc?from=endpoint", "", 0, ""},
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", c.url, nil)
		origin, id, txn := traceOrigin(r)
		if origin != c.origin || id != c.id || txn != c.txn {
			t.Errorf("traceOrigin(%q) = (%q, %d, %q), want (%q, %d, %q)",
				c.url, origin, id, txn, c.origin, c.id, c.txn)
		}
	}
}
