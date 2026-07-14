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
