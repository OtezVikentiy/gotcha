package uptime

import (
	"context"
	"testing"
	"time"
)

// fakeChecker — чекер с заданной последовательностью результатов; считает вызовы.
type fakeChecker struct {
	results []Result
	calls   int
}

func (c *fakeChecker) Check(context.Context, Monitor) Result {
	r := c.results[min(c.calls, len(c.results)-1)]
	c.calls++
	return r
}

func TestCheckWithRetries(t *testing.T) {
	old := retryDelay
	retryDelay = time.Millisecond
	defer func() { retryDelay = old }()

	fail := Result{OK: false, Error: "boom"}
	ok := Result{OK: true, StatusCode: 200}

	cases := []struct {
		name      string
		retries   int
		results   []Result // по вызовам; последний повторяется
		wantOK    bool
		wantCalls int
	}{
		{"no retries, first ok", 0, []Result{ok}, true, 1},
		{"no retries, first fail — no retry", 0, []Result{fail}, false, 1},
		{"retry, succeeds on 2nd", 2, []Result{fail, ok}, true, 2},
		{"retry, succeeds on 3rd (last allowed)", 2, []Result{fail, fail, ok}, true, 3},
		{"retries exhausted — last failure returned", 2, []Result{fail}, false, 3},
		{"stops at first ok, no extra calls", 3, []Result{ok}, true, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &fakeChecker{results: tc.results}
			res := checkWithRetries(context.Background(), c, Monitor{Retries: tc.retries})
			if res.OK != tc.wantOK {
				t.Errorf("OK = %v, want %v", res.OK, tc.wantOK)
			}
			if c.calls != tc.wantCalls {
				t.Errorf("calls = %d, want %d", c.calls, tc.wantCalls)
			}
		})
	}
}

// Отмена контекста между повторами прекращает попытки и возвращает последний
// (неуспешный) результат — не висим на паузе при шатдауне.
func TestCheckWithRetriesContextCancel(t *testing.T) {
	old := retryDelay
	retryDelay = time.Hour // пауза заведомо длиннее — выходим только по ctx
	defer func() { retryDelay = old }()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := &fakeChecker{results: []Result{{OK: false, Error: "boom"}}}
	res := checkWithRetries(ctx, c, Monitor{Retries: 5})
	if res.OK {
		t.Fatalf("expected failure")
	}
	if c.calls != 1 {
		t.Errorf("calls = %d, want 1 (cancel before first retry)", c.calls)
	}
}
