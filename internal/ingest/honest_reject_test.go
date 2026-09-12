package ingest

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Воркеры нигде не запускаются (p.Start() не зовётся): без него задача
// остаётся в канале, и бюджет не освобождается гонкой с обработкой.
func honestStoreRequest() *http.Request {
	req := httptest.NewRequest("POST", "/api/1/store/?sentry_key=pub", strings.NewReader(`{"message":"e"}`))
	req.SetPathValue("project", "1")
	return req
}

func TestHonestEnvelopeEventsCapacityDropReturns503(t *testing.T) {
	body := `{"event_id":"9ec79c33ec9942ab8353589fcb2e04dc"}
{"type":"event"}
{"message":"e"}
`
	p, ev, _ := newSatPipeline(0, 0) // заполненность НИЗКАЯ — preflight пропускает
	p.SetMaxQueueBytes(1)            // бюджет меньше цены любой задачи — Enqueue не пройдёт
	h := NewHandler(overloadKeyCache(), nil, p, 1<<20)

	w := httptest.NewRecorder()
	h.envelope(w, envelopeRequest(body))

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503, body=%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Retry-After"); got != "5" {
		t.Errorf("Retry-After = %q, want 5", got)
	}
	if ev.count() != 0 {
		t.Errorf("батчер увидел %d событий, want 0 — ничего не должно было дойти", ev.count())
	}
	if got := h.RejectedBy(RejectOverloaded, SignalEvent); got != 1 {
		t.Errorf("RejectedBy(overloaded, event) = %d, want 1", got)
	}
	if got := p.DroppedBy(DropQueueBytes); got != 1 {
		t.Errorf("DroppedBy(queue_bytes) = %d, want 1 — событие обязано быть дропнуто ИМЕННО по ёмкости", got)
	}
}

func TestHonestStoreCapacityDropReturns503(t *testing.T) {
	p, ev, _ := newSatPipeline(0, 0)
	p.SetMaxQueueBytes(1)
	h := NewHandler(overloadKeyCache(), nil, p, 1<<20)

	w := httptest.NewRecorder()
	h.store(w, honestStoreRequest())

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503, body=%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Retry-After"); got != "5" {
		t.Errorf("Retry-After = %q, want 5", got)
	}
	if ev.count() != 0 {
		t.Errorf("батчер увидел %d событий, want 0", ev.count())
	}
	if got := h.RejectedBy(RejectOverloaded, SignalEvent); got != 1 {
		t.Errorf("RejectedBy(overloaded, event) = %d, want 1", got)
	}
}

func TestHonestOTLPTracesCapacityDropReturns503(t *testing.T) {
	p, _, sp := newSatPipeline(0, 0) // TransactionSaturation тоже низкая
	p.SetMaxQueueBytes(1)
	h := NewHandler(overloadKeyCache(), nil, p, 1<<20)

	w := postTraces(t, h)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503, body=%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Retry-After"); got != "5" {
		t.Errorf("Retry-After = %q, want 5", got)
	}
	if sp.count() != 0 {
		t.Errorf("SpanWriter увидел %d транзакций, want 0", sp.count())
	}
	if got := h.RejectedBy(RejectOverloaded, SignalTransaction); got != 1 {
		t.Errorf("RejectedBy(overloaded, transaction) = %d, want 1", got)
	}
	if got := p.DroppedBy(DropQueueBytes); got != 1 {
		t.Errorf("DroppedBy(queue_bytes) = %d, want 1", got)
	}
}

func TestHonestEnvelopePartialCapacityDropStays200(t *testing.T) {
	body := `{"event_id":"9ec79c33ec9942ab8353589fcb2e04dc"}
{"type":"event"}
{"message":"e1"}
{"type":"event"}
{"message":"e2"}
`
	p, ev, _ := newSatPipeline(0, 0)
	p.SetMaxQueueBytes(300) // одно минимальное событие (~258 байт) влезает, два — нет

	h := NewHandler(overloadKeyCache(), nil, p, 1<<20)

	w := httptest.NewRecorder()
	h.envelope(w, envelopeRequest(body))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (частичная постановка — не отказ), body=%s", w.Code, w.Body.String())
	}
	if got := h.RejectedBy(RejectOverloaded, SignalEvent); got != 0 {
		t.Errorf("RejectedBy(overloaded, event) = %d, want 0 — частичная постановка не считается отказом", got)
	}
	if ev.count() != 0 {
		t.Errorf("батчер увидел %d событий, want 0 — воркер не запускался, задачи остаются в очереди", ev.count())
	}
	if got := p.DroppedBy(DropQueueBytes); got != 1 {
		t.Errorf("DroppedBy(queue_bytes) = %d, want 1 — ровно одно из двух событий обязано было не влезть "+
			"в бюджет, иначе тест не проверяет частичную постановку", got)
	}
}

func TestHonestEnvelopeAllBadItemsStays503Free(t *testing.T) {
	body := `{"event_id":"9ec79c33ec9942ab8353589fcb2e04dc"}
{"type":"event"}
not-json-at-all
`
	p, ev, _ := newSatPipeline(0, 0)
	p.SetMaxQueueBytes(1) // будь причина ёмкость, дроп случился бы гарантированно
	h := NewHandler(overloadKeyCache(), nil, p, 1<<20)

	w := httptest.NewRecorder()
	h.envelope(w, envelopeRequest(body))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (битый item — не причина для 503), body=%s", w.Code, w.Body.String())
	}
	if ev.count() != 0 {
		t.Errorf("батчер увидел %d событий, want 0", ev.count())
	}
	if got := h.RejectedBy(RejectOverloaded, SignalEvent); got != 0 {
		t.Errorf("RejectedBy(overloaded, event) = %d, want 0 — битый item не вызывает Enqueue вовсе", got)
	}
	if got := p.DroppedBy(DropQueueBytes); got != 0 {
		t.Errorf("DroppedBy(queue_bytes) = %d, want 0 — до постановки дело не дошло", got)
	}
}

func TestHonestEnvelopeMixedCapacityDropStays200(t *testing.T) {
	bigMessage := strings.Repeat("m", 5000)
	body := `{"event_id":"9ec79c33ec9942ab8353589fcb2e04dc"}
{"type":"event"}
{"message":"` + bigMessage + `"}
` + envelopeTxItem

	p, ev, sp := newSatPipeline(0, 0)
	p.SetMaxQueueBytes(1000) // раздутое событие (~5256 байт) не влезает целиком, транзакция — влезает

	h := NewHandler(overloadKeyCache(), nil, p, 1<<20)

	w := httptest.NewRecorder()
	h.envelope(w, envelopeRequest(body))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (транзакция принята — что-то внесено), body=%s", w.Code, w.Body.String())
	}
	if got := h.RejectedBy(RejectOverloaded, SignalEvent); got != 0 {
		t.Errorf("RejectedBy(overloaded, event) = %d, want 0 — смешанный конверт не отказ", got)
	}
	if ev.count() != 0 || sp.count() != 0 {
		t.Errorf("до приёмников дошло: events=%d tx=%d, want 0/0 — воркер не запускался", ev.count(), sp.count())
	}
	if got := p.DroppedBy(DropQueueBytes); got != 1 {
		t.Errorf("DroppedBy(queue_bytes) = %d, want 1 — событие обязано было не влезть в бюджет одно, "+
			"иначе тест не проверяет искомый сценарий", got)
	}
	if got := p.QueuedBytes(); got == 0 {
		t.Error("QueuedBytes = 0 — транзакция обязана была занять бюджет, значит она встала в очередь")
	}
}

func TestHonestEnvelopeAllEnqueuedStaysQuiet(t *testing.T) {
	body := `{"event_id":"9ec79c33ec9942ab8353589fcb2e04dc"}
{"type":"event"}
{"message":"e"}
`
	p, _, _ := newSatPipeline(0, 0) // бюджет по умолчанию (64 МиБ) — щедрый
	h := NewHandler(overloadKeyCache(), nil, p, 1<<20)

	w := httptest.NewRecorder()
	h.envelope(w, envelopeRequest(body))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", w.Code, w.Body.String())
	}
	if got := h.RejectedBy(RejectOverloaded, SignalEvent); got != 0 {
		t.Errorf("RejectedBy(overloaded, event) = %d, want 0", got)
	}
	if got := p.DroppedBy(DropQueueBytes); got != 0 {
		t.Errorf("DroppedBy(queue_bytes) = %d, want 0 — щедрый бюджет не должен ронять единственное событие", got)
	}
}
