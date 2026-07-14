package main

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type fakePinger struct {
	err   error
	delay time.Duration
}

func (f fakePinger) Ping(ctx context.Context) error {
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return f.err
}

func TestHealthzOK(t *testing.T) {
	h := healthHandler(fakePinger{}, fakePinger{})
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"postgres":"ok"`) {
		t.Errorf("body = %s", rec.Body.String())
	}
}

func TestHealthzClickHouseDown(t *testing.T) {
	h := healthHandler(fakePinger{}, fakePinger{err: errors.New("dial tcp 10.0.0.5:9000: refused")})
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != 503 {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"clickhouse":"unavailable"`) {
		t.Errorf("want sanitized status, body = %s", body)
	}
	if strings.Contains(body, "10.0.0.5") {
		t.Errorf("internal error details leaked to body: %s", body)
	}
}

func TestHealthzSlowPostgresDoesNotStarveClickHouse(t *testing.T) {
	// PG висит дольше своего таймаута; CH отвечает за 1.5s — последовательный
	// хендлер занял бы ~3.5s, конкурентный — ~2s.
	h := healthHandler(fakePinger{delay: 3 * time.Second}, fakePinger{delay: 1500 * time.Millisecond})
	rec := httptest.NewRecorder()
	start := time.Now()
	h(rec, httptest.NewRequest("GET", "/healthz", nil))
	if elapsed := time.Since(start); elapsed > 2900*time.Millisecond {
		t.Fatalf("handler took %v, pings are not concurrent", elapsed)
	}
	if rec.Code != 503 {
		t.Fatalf("status = %d, want 503 (pg down)", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"clickhouse":"ok"`) || !strings.Contains(body, `"postgres":"unavailable"`) {
		t.Errorf("body = %s", body)
	}
}
