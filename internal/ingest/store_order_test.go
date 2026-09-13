package ingest

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestStoreQuotaExceededCountsDropAndReturns429(t *testing.T) {
	p, _, _ := newSatPipeline(0, 0)
	dc := newFakeDropCounter()
	h := NewHandler(overloadKeyCache(), denyingQuota{}, p, 1<<20)
	h.DropCounter = dc

	w := httptest.NewRecorder()
	h.store(w, honestStoreRequest())

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429, body=%s", w.Code, w.Body.String())
	}
	if got := dc.eventsFor(1); got != 1 {
		t.Errorf("IncDroppedEvents(org 1) = %d, want 1", got)
	}
}

// Битое или слишком большое тело не должно стоить клиенту квоты.
func TestStoreMalformedBodyDoesNotChargeQuota(t *testing.T) {
	p, _, _ := newSatPipeline(0, 0)
	q := &countingQuota{}
	h := NewHandler(overloadKeyCache(), q, p, 1<<20)

	req := httptest.NewRequest("POST", "/api/1/store/?sentry_key=pub", strings.NewReader("not-json-at-all"))
	req.SetPathValue("project", "1")

	w := httptest.NewRecorder()
	h.store(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body=%s", w.Code, w.Body.String())
	}
	if got := q.count(); got != 0 {
		t.Errorf("grant вызван %d раз на битое тело, want 0", got)
	}
}

func TestStoreTooLargeBodyDoesNotChargeQuota(t *testing.T) {
	p, _, _ := newSatPipeline(0, 0)
	q := &countingQuota{}
	h := NewHandler(overloadKeyCache(), q, p, 8) // предел меньше любого настоящего тела события

	req := httptest.NewRequest("POST", "/api/1/store/?sentry_key=pub",
		strings.NewReader(`{"message":"way too long for the configured limit"}`))
	req.SetPathValue("project", "1")

	w := httptest.NewRecorder()
	h.store(w, req)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413, body=%s", w.Code, w.Body.String())
	}
	if got := q.count(); got != 0 {
		t.Errorf("grant вызван %d раз на слишком большое тело, want 0", got)
	}
}

func TestStoreValidEventChargesQuotaAfterParsing(t *testing.T) {
	p, _, _ := newSatPipeline(0, 0)
	q := &countingQuota{}
	h := NewHandler(overloadKeyCache(), q, p, 1<<20)

	w := httptest.NewRecorder()
	h.store(w, honestStoreRequest())

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", w.Code, w.Body.String())
	}
	if got := q.count(); got != 1 {
		t.Errorf("grant вызван %d раз, want 1 — валидное событие обязано списать квоту", got)
	}
}

// Выключенный синк профилей не должен пройти 200-м без единого следа.
func TestEnvelopeProfileOnlyWithProfilesDisabledCountsDrop(t *testing.T) {
	p, ev, _ := newSatPipeline(0, 0)
	dc := newFakeDropCounter()
	h := NewHandler(overloadKeyCache(), nil, p, 1<<20)
	h.DropCounter = dc
	// h.Profiles остаётся nil — приём профилей выключен на этом узле.

	body := `{"event_id":"9ec79c33ec9942ab8353589fcb2e04dc"}
` + envelopeProfileItem

	w := httptest.NewRecorder()
	h.envelope(w, envelopeRequest(body))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (конверт принят, профиль выброшен по конфигурации), body=%s",
			w.Code, w.Body.String())
	}
	if dc.profilesCalls == 0 {
		t.Error("IncDroppedProfiles не вызван — выключенный синк профилей молча обошёл учёт дропов")
	}
	if ev.count() != 0 {
		t.Errorf("батчер увидел %d событий, want 0 — конверт был из одних профилей", ev.count())
	}
}
