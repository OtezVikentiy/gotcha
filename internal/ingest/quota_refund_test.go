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

// Этот файл — T8: организация обязана платить за то, что встало в очередь, а
// не за то, что приёмник попытался принять. Сценарии зеркалят T5
// (honest_reject_test.go: те же способы вызвать ёмкостный дроп через
// SetMaxQueueBytes без запуска воркера), но квота здесь — двойник, видящий
// не только списания, а и возвраты, поэтому итоговое потребление организации
// проверяется явно, а не выводится из побочных эффектов.

// refundCountingQuota — QuotaChecker: CheckAndCount выдаёт всё запрошенное
// (безлимит) и накапливает сумму; Refund считает вызовы и (если refundErr не
// задан) накапливает возвращённое. net() — granted-refunded, то есть реальное
// потребление организации, что и есть предмет проверки T8.
//
// chargeAt — если задан, CheckAndCount возвращает его как chargedAt вместо
// нулевого времени: так тесты границы месяца могут подсунуть конкретный
// «момент списания» и проверить, что Handler.refund донёс именно его до
// Refund, а не подставил time.Now() заново (T8, фокус «граница месяца»).
// gotChargedAt запоминает, что Refund увидел последним.
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

// refundLogBuf — буфер логов, безопасный при параллельной записи (по образцу
// syncBuf из handler_test.go — тот живёт в ingest_test, внешнем пакете, и
// отсюда недоступен).
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

// TestRefundAllCapacityDroppedOn503 — тот же приём, что у
// TestHonestEnvelopeEventsCapacityDropReturns503 (T5): преflight пропускает
// (заполненность низкая), а байтовый бюджет очереди меньше цены любой задачи
// — Enqueue гарантированно возвращает false. Списанное квотой единственное
// событие обязано вернуться целиком: итоговое потребление организации — 0.
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

// TestRefundUsesChargedMonthNotNow — граница месяца (T8, самый опасный
// сценарий): списание в envelope и возврат разнесены по времени внутри одного
// запроса. Если бы Handler.refund/OrgQuota.Refund пересчитывали месяц
// собственным time.Now() в момент возврата, а не переносили значение,
// вернувшееся из CheckAndCount, запрос, пришедший вплотную к границе месяца,
// списался бы в одном месяце и вернулся в другой — счёт прошлого месяца был
// бы завышен навсегда. Здесь quota.chargeAt подставлен заведомо ДРУГИМ
// месяцем, чем «сейчас», и тест требует, чтобы именно ОН дошёл до Refund.
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

// TestRefundPartialCapacityDrop — 10 однотипных минимальных событий
// (overhead 256 + len("e")=1 → 257 байт задачи каждое, см. taskBytes) в одном
// конверте; бюджет очереди пропускает ровно 4 (4*257=1028 ≤ бюджет < 1285 —
// цена пятого). Списано 10, поставлено 4 → возвращено обязано быть ровно 6.
func TestRefundPartialCapacityDrop(t *testing.T) {
	const item = "{\"type\":\"event\"}\n{\"message\":\"e\"}\n"
	body := `{"event_id":"9ec79c33ec9942ab8353589fcb2e04dc"}` + "\n" + strings.Repeat(item, 10)

	p, ev, _ := newSatPipeline(0, 0)
	p.SetMaxQueueBytes(4*257 + 100) // 4 влезают (1028), 5-е — нет (1285)
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

// TestRefundBadItemsNotRefunded — сторож границы «наша вина / вина клиента»:
// битый item (не проходит ParseEvent) списан квотой на уровне envelope'а
// (грант считается по числу распознанных item'ов верхнего уровня, до разбора
// содержимого), но до Enqueue не доходит вовсе — не ёмкостная причина.
// Возврата быть не должно, иначе поток мусора стал бы бесплатным по квоте:
// квота в продукте работает и как ограничитель злоупотребления.
func TestRefundBadItemsNotRefunded(t *testing.T) {
	body := `{"event_id":"9ec79c33ec9942ab8353589fcb2e04dc"}
{"type":"event"}
not-json-at-all
`
	p, ev, _ := newSatPipeline(0, 0)
	p.SetMaxQueueBytes(1) // будь причина ёмкость, дроп случился бы гарантированно
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

// TestRefundQuotaDisabledNoop — h.quota == nil (квотирование выключено):
// Handler.refund обязан быть no-op при nil-квоте (как и h.grant), а не
// пытаться вызвать Refund через nil-интерфейс. Сценарий — тот же ёмкостный
// дроп на легаси-эндпоинте /api/1/store/, что у TestHonestStoreCapacityDropReturns503.
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

// TestRefundErrorDoesNotChangeResponse — возврат квоты падает с ошибкой:
// ответ клиенту не должен измениться (best-effort, та же дисциплина, что у
// countDrop), но ошибка обязана попасть в лог — молча потерянный возврат
// означает молча завышенный счёт (см. докблок Handler.refund).
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
