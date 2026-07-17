package ingest_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"
)

// TestQuotaExceededReturns429 закрывает основной сценарий: квота 2, третье
// принятое событие получает 429 с Retry-After > 0.
func TestQuotaExceededReturns429(t *testing.T) {
	s := newStack(t)
	ctx := context.Background()
	if err := s.orgSvc.SetQuota(ctx, s.org.ID, 2); err != nil {
		t.Fatalf("SetQuota: %v", err)
	}
	path := fmt.Sprintf("/api/%d/envelope/", s.project.ID)

	for i := 0; i < 2; i++ {
		resp := s.post(t, path, envelopeBody(testEventJSON), false, s.key.PublicKey)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("event %d: status = %d, want 200", i, resp.StatusCode)
		}
	}

	resp := s.post(t, path, envelopeBody(testEventJSON), false, s.key.PublicKey)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("third event: status = %d, want 429", resp.StatusCode)
	}
	ra := resp.Header.Get("Retry-After")
	if ra == "" {
		t.Fatal("Retry-After header missing")
	}
	secs, err := time.ParseDuration(ra + "s")
	if err != nil || secs <= 0 {
		t.Fatalf("Retry-After = %q, want positive seconds", ra)
	}
}

// TestDroppedCounterOnQuotaExceeded: при 429 по квоте событий отклонённое
// событие учитывается в org_usage.dropped_events (PROD-P1) и не трогает
// принятый events_count.
func TestDroppedCounterOnQuotaExceeded(t *testing.T) {
	s := newStack(t)
	ctx := context.Background()
	if err := s.orgSvc.SetQuota(ctx, s.org.ID, 1); err != nil {
		t.Fatalf("SetQuota: %v", err)
	}
	path := fmt.Sprintf("/api/%d/envelope/", s.project.ID)

	// Первое событие принято (200), второе и третье — 429 (по одному дропу).
	if resp := s.post(t, path, envelopeBody(testEventJSON), false, s.key.PublicKey); resp.StatusCode != http.StatusOK {
		t.Fatalf("first event: status = %d, want 200", resp.StatusCode)
	}
	for i := 0; i < 2; i++ {
		if resp := s.post(t, path, envelopeBody(testEventJSON), false, s.key.PublicKey); resp.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("drop %d: status = %d, want 429", i, resp.StatusCode)
		}
	}

	d, err := s.orgSvc.DroppedUsage(ctx, s.org.ID, time.Now())
	if err != nil {
		t.Fatalf("DroppedUsage: %v", err)
	}
	if d.Events != 2 {
		t.Fatalf("dropped events = %d, want 2", d.Events)
	}
	// Прочие счётчики дропов не задеты дропом событий.
	if d.Transactions != 0 || d.Metrics != 0 || d.Profiles != 0 {
		t.Fatalf("cross-class drops = %+v, want only Events", d)
	}
}

// TestQuotaZeroIsUnlimited: EventQuota=0 никогда не блокирует, сколько бы
// событий ни пришло.
func TestQuotaZeroIsUnlimited(t *testing.T) {
	s := newStack(t)
	ctx := context.Background()
	if err := s.orgSvc.SetQuota(ctx, s.org.ID, 0); err != nil {
		t.Fatalf("SetQuota: %v", err)
	}
	path := fmt.Sprintf("/api/%d/envelope/", s.project.ID)

	for i := 0; i < 5; i++ {
		resp := s.post(t, path, envelopeBody(testEventJSON), false, s.key.PublicKey)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("event %d: status = %d, want 200", i, resp.StatusCode)
		}
	}
}

// TestQuotaCountsInOrgUsage: каждый принятый запрос увеличивает org_usage.
func TestQuotaCountsInOrgUsage(t *testing.T) {
	s := newStack(t)
	ctx := context.Background()
	path := fmt.Sprintf("/api/%d/envelope/", s.project.ID)

	for i := 0; i < 3; i++ {
		resp := s.post(t, path, envelopeBody(testEventJSON), false, s.key.PublicKey)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("event %d: status = %d, want 200", i, resp.StatusCode)
		}
	}

	n, err := s.orgSvc.Usage(ctx, s.org.ID, time.Now())
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}
	if n != 3 {
		t.Fatalf("Usage = %d, want 3", n)
	}
}
