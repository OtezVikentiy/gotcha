package ingest

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Этот файл — T5: постановка в очередь (Enqueue/EnqueueTransaction) честно
// сообщает результат, и хендлер обязан превратить «ничего не встало по
// причине ёмкости» в 503 вместо прежнего молчаливого 200. Окно, которое
// закрывает T5, не воспроизводится через saturation-моки (newSatPipeline из
// overload_internal_test.go): там заполненность подставляется тестом и сама
// решает исход preflight'а. Здесь наоборот — preflight обязан ПРОПУСТИТЬ
// запрос (заполненность низкая), а реальная постановка в очередь всё равно
// обязана провалиться, потому что байтовый бюджет очереди (SetMaxQueueBytes)
// исчерпан или занижен ниже цены задачи. Воркеры нигде не запускаются
// (p.Start() не зовётся): без него постановленная задача остаётся в канале
// до конца теста, и бюджет не освобождается гонкой с обработкой — расход
// бюджета внутри одного запроса детерминирован.

// honestStoreRequest — тело для legacy-эндпоинта /api/1/store/: одно валидное
// событие, ключ и проект — как у overloadKeyCache().
func honestStoreRequest() *http.Request {
	req := httptest.NewRequest("POST", "/api/1/store/?sentry_key=pub", strings.NewReader(`{"message":"e"}`))
	req.SetPathValue("project", "1")
	return req
}

// TestHonestEnvelopeEventsCapacityDropReturns503 — конверт из одних событий:
// preflight видит низкую заполненность (пропускает), но байтовый бюджет
// очереди меньше цены любой задачи — Enqueue гарантированно вернёт false.
// Ничего не встало ни по одной причине, кроме ёмкости → 503 + Retry-After,
// а не прежний тихий 200.
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

// TestHonestStoreCapacityDropReturns503 — тот же сценарий на legacy-эндпоинте
// /api/1/store/: одно-единственное событие запроса — всё его содержимое, и
// если Enqueue вернёт false, запрос не внёс ничего.
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

// TestHonestOTLPTracesCapacityDropReturns503 — тот же сценарий на /v1/traces:
// экспорт с единственным спаном, EnqueueTransaction гарантированно возвращает
// false, signal в отказе — transaction, а не event.
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

// TestHonestEnvelopePartialCapacityDropStays200 — два события в одном
// конверте, бюджет очереди пропускает первое и не пропускает второе (тот же
// приём, что и TestQueueByteBudgetDropsOversizedFlood: бюджет — общий
// накопительный счётчик, второе Enqueue проверяется УЖЕ против занятого
// первым бюджета). Частичная потеря НЕ должна превращаться в отказ: повтор
// клиента продублировал бы уже принятое первое событие — дедупликации по
// event_id в продукте нет (см. запрет в брифе T5, п.3).
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

// TestHonestEnvelopeAllBadItemsStays503Free — сторож: ноль поставлено, но
// причина — НЕ ёмкость, а битый item (не проходит json.Unmarshal в
// ParseEvent). Такой item никогда не доходит до Enqueue, поэтому
// eventCapacityDropped не взводится, и ответ обязан остаться прежним 200.
// Без этого сторожа приёмник начал бы отвечать 503 на мусор клиента, и
// клиент бы ретраил этот же мусор бесконечно (см. запрет в брифе, п.2).
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

// TestHonestEnvelopeMixedCapacityDropStays200 — события и транзакции в одном
// конверте: событие раздуто так, что его цена превышает бюджет целиком (сам
// по себе, без накопления с чем-то ещё), а транзакция — нет. Общий
// накопительный счётчик бюджета (queueBytes) при этом не тратится неудачной
// попыткой (admit — CAS, при провале cur не меняется), поэтому проверка
// транзакции идёт против ПОЛНОГО бюджета, а не остатка. Событие дропнуто по
// ёмкости, транзакция встала — что-то реально принято → 200, а не 503:
// смешанный конверт, где отказ по ОДНОМУ классу не должен хоронить успех
// другого (та же дисциплина, что и у overload-preflight).
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

// TestHonestEnvelopeAllEnqueuedStaysQuiet — сторож обычного пути: щедрый
// бюджет, единственное событие ставится без проблем → 200, счётчик отказов
// по ёмкости не двигается. Ловит мутацию «весь ответ 503 независимо от
// результата постановки».
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
