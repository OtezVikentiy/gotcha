package ingest

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type refundCountingQuota struct {
	mu           sync.Mutex
	granted      int64
	refunded     int64
	refundCalls  int
	refundErr    error
	chargeAt     time.Time
	gotChargedAt time.Time
}

func (q *refundCountingQuota) CheckAndCount(_ context.Context, _ int64, want int64) (int64, time.Time, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.granted += want
	return want, q.chargeAt, nil
}

func (q *refundCountingQuota) Refund(_ context.Context, _ int64, n int64, chargedAt time.Time) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.refundCalls++
	q.gotChargedAt = chargedAt
	if q.refundErr != nil {
		return q.refundErr
	}
	q.refunded += n
	return nil
}

func (q *refundCountingQuota) net() int64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.granted - q.refunded
}

func (q *refundCountingQuota) lastChargedAt() time.Time {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.gotChargedAt
}

type refundLogBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *refundLogBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *refundLogBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestRefundAllCapacityDroppedOn503(t *testing.T) {
	body := `{"event_id":"9ec79c33ec9942ab8353589fcb2e04dc"}
{"type":"event"}
{"message":"e"}
`
	p, _, _ := newSatPipeline(0, 0)
	p.SetMaxQueueBytes(1)
	q := &refundCountingQuota{}
	h := NewHandler(overloadKeyCache(), q, p, 1<<20)

	w := httptest.NewRecorder()
	h.envelope(w, envelopeRequest(body))

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503, body=%s", w.Code, w.Body.String())
	}
	if q.granted != 1 {
		t.Fatalf("granted = %d, want 1", q.granted)
	}
	if q.refunded != 1 {
		t.Fatalf("refunded = %d, want 1 — списанное по 503 обязано вернуться целиком", q.refunded)
	}
	if net := q.net(); net != 0 {
		t.Fatalf("net потребление = %d, want 0", net)
	}
}

func TestRefundUsesChargedMonthNotNow(t *testing.T) {
	body := `{"event_id":"9ec79c33ec9942ab8353589fcb2e04dc"}
{"type":"event"}
{"message":"e"}
`
	p, _, _ := newSatPipeline(0, 0)
	p.SetMaxQueueBytes(1)
	charged := time.Date(2026, 1, 31, 23, 59, 59, 0, time.UTC)
	q := &refundCountingQuota{chargeAt: charged}
	h := NewHandler(overloadKeyCache(), q, p, 1<<20)

	w := httptest.NewRecorder()
	h.envelope(w, envelopeRequest(body))

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503, body=%s", w.Code, w.Body.String())
	}
	if q.refundCalls != 1 {
		t.Fatalf("Refund вызван %d раз, want 1", q.refundCalls)
	}
	if got := q.lastChargedAt(); !got.Equal(charged) {
		t.Fatalf("Refund получил месяц %v, want %v (месяц списания из CheckAndCount) — "+
			"возврат не должен пересчитывать текущее время заново", got, charged)
	}
}

func TestRefundPartialCapacityDrop(t *testing.T) {
	const item = "{\"type\":\"event\"}\n{\"message\":\"e\"}\n"
	body := `{"event_id":"9ec79c33ec9942ab8353589fcb2e04dc"}` + "\n" + strings.Repeat(item, 10)

	p, ev, _ := newSatPipeline(0, 0)
	p.SetMaxQueueBytes(4*257 + 100)
	q := &refundCountingQuota{}
	h := NewHandler(overloadKeyCache(), q, p, 1<<20)

	w := httptest.NewRecorder()
	h.envelope(w, envelopeRequest(body))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (частичная постановка — не отказ), body=%s", w.Code, w.Body.String())
	}
	if ev.count() != 0 {
		t.Errorf("батчер увидел %d событий, want 0 — воркер не запускался", ev.count())
	}
	if q.granted != 10 {
		t.Fatalf("granted = %d, want 10", q.granted)
	}
	if q.refunded != 6 {
		t.Fatalf("refunded = %d, want 6 (списано 10, поставлено 4)", q.refunded)
	}
	if net := q.net(); net != 4 {
		t.Fatalf("net потребление = %d, want 4 — ровно поставленное в очередь", net)
	}
}

func TestRefundBadItemsNotRefunded(t *testing.T) {
	body := `{"event_id":"9ec79c33ec9942ab8353589fcb2e04dc"}
{"type":"event"}
not-json-at-all
`
	p, ev, _ := newSatPipeline(0, 0)
	p.SetMaxQueueBytes(1)
	q := &refundCountingQuota{}
	h := NewHandler(overloadKeyCache(), q, p, 1<<20)

	w := httptest.NewRecorder()
	h.envelope(w, envelopeRequest(body))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (битый item — не причина для 503), body=%s", w.Code, w.Body.String())
	}
	if ev.count() != 0 {
		t.Errorf("батчер увидел %d событий, want 0", ev.count())
	}
	if q.refundCalls != 0 {
		t.Fatalf("Refund вызван %d раз, want 0 — брак клиента не возвращается", q.refundCalls)
	}
	if net := q.net(); net != 1 {
		t.Fatalf("net потребление = %d, want 1 — списанное за битый item остаётся списанным", net)
	}
}

func TestRefundQuotaDisabledNoop(t *testing.T) {
	p, ev, _ := newSatPipeline(0, 0)
	p.SetMaxQueueBytes(1)
	h := NewHandler(overloadKeyCache(), nil, p, 1<<20)

	w := httptest.NewRecorder()
	h.store(w, honestStoreRequest())

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503, body=%s", w.Code, w.Body.String())
	}
	if ev.count() != 0 {
		t.Errorf("батчер увидел %d событий, want 0", ev.count())
	}
}

func TestRefundErrorDoesNotChangeResponse(t *testing.T) {
	var logs refundLogBuf
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	defer slog.SetDefault(prev)

	body := `{"event_id":"9ec79c33ec9942ab8353589fcb2e04dc"}
{"type":"event"}
{"message":"e"}
`
	p, _, _ := newSatPipeline(0, 0)
	p.SetMaxQueueBytes(1)
	q := &refundCountingQuota{refundErr: errors.New("refund boom")}
	h := NewHandler(overloadKeyCache(), q, p, 1<<20)

	w := httptest.NewRecorder()
	h.envelope(w, envelopeRequest(body))

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 несмотря на ошибку возврата, body=%s", w.Code, w.Body.String())
	}
	if q.refundCalls != 1 {
		t.Fatalf("Refund вызван %d раз, want 1", q.refundCalls)
	}
	if out := logs.String(); !strings.Contains(out, "quota refund failed") {
		t.Errorf("лог не содержит предупреждение о неудавшемся возврате:\n%s", out)
	}
}
