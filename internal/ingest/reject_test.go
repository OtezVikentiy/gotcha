package ingest

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	"google.golang.org/protobuf/proto"

	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/profile"
)

// errReader — тело запроса, чтение которого обрывается ошибкой ввода-вывода
// (оборванное соединение), а не EOF и не превышением лимита. Единственный
// способ достать ветку «тело не дочитано» на входах, где ReadAll/ParseNDJSON
// обязаны отличать её от too_large: без него ветка недостижима из httptest,
// а именно она даёт reason=malformed.
type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("connection reset") }

// countingProfileSink — ProfileSink, считающий записанные профили: тестам
// отказов нужно убедиться, что отклонённый запрос НИЧЕГО не записал.
// Собственный, а не общий с handler_test.go: тот живёт в пакете ingest_test.
type countingProfileSink struct{ n int }

func (s *countingProfileSink) Add(_ int64, _ profile.Profile) { s.n++ }

// newRejectHandler — приёмник с валидным ключом (Bearer pub / sentry_key любой)
// и заданным потолком тела. Квоты/приёмники подставляют сами тесты.
func newRejectHandler(maxBytes int64) *Handler {
	return NewHandler(NewKeyCache(stubKeyResolver{key: org.Key{ProjectID: 1, OrgID: 1}}), nil, nil, maxBytes)
}

func pprofRequest(body io.Reader, contentEncoding string) *http.Request {
	req := httptest.NewRequest("POST", "/profiles/pprof", body)
	req.Header.Set("Authorization", "Bearer pub")
	if contentEncoding != "" {
		req.Header.Set("Content-Encoding", contentEncoding)
	}
	return req
}

func otlpLogsRequest(body io.Reader, contentType, contentEncoding string) *http.Request {
	req := httptest.NewRequest("POST", "/v1/logs", body)
	req.Header.Set("Authorization", "Bearer pub")
	req.Header.Set("Content-Type", contentType)
	if contentEncoding != "" {
		req.Header.Set("Content-Encoding", contentEncoding)
	}
	return req
}

// TestIngestRejectionPairsContract — контракт закрытого набора пар
// gotcha_ingest_rejected_total{reason,signal}: набор отдаётся КОПИЕЙ (чужая
// мутация не должна портить общий слайс, на котором main регистрирует
// self-метрики), key_revoked зарезервирован и в наборе отсутствовать обязан,
// (quota, deploy) невозможна (деплои не расходуют месячную квоту), у каждой
// пары набора есть живой счётчик, а пара ВНЕ набора молча игнорируется.
func TestIngestRejectionPairsContract(t *testing.T) {
	pairs := IngestRejectionPairs()
	if len(pairs) != 29 {
		t.Fatalf("пар в наборе = %d, want 29 (5 сигналов × 4 причины + 5 quota-сигналов, без deploy)", len(pairs))
	}

	// Копия, а не общий слайс: порча вернувшегося набора не должна доезжать
	// до ingestRejectionPairs.
	poison := IngestRejectionKey{RejectKeyRevoked, SignalDeploy}
	pairs[0] = poison
	fresh := IngestRejectionPairs()
	if fresh[0] == poison {
		t.Fatalf("IngestRejectionPairs отдаёт общий слайс: мутация копии видна в наборе")
	}

	for _, p := range fresh {
		if p.Reason == RejectKeyRevoked {
			t.Errorf("key_revoked в наборе (%v): причина зарезервирована, кодом не производится", p)
		}
		if p.Reason == RejectQuota && p.Signal == SignalDeploy {
			t.Errorf("(quota, deploy) в наборе: деплои не расходуют месячную квоту")
		}
	}

	// Каждая пара набора инкрементируется и читается.
	h := newRejectHandler(1 << 20)
	for _, p := range fresh {
		before := h.RejectedBy(p.Reason, p.Signal)
		h.countRejected(p.Reason, p.Signal)
		if got := h.RejectedBy(p.Reason, p.Signal); got != before+1 {
			t.Errorf("RejectedBy(%s, %s) = %d, want %d", p.Reason, p.Signal, got, before+1)
		}
	}

	// Пара вне набора: счётчика нет — RejectedBy отдаёт 0, countRejected не паникует.
	h.countRejected(RejectKeyRevoked, SignalEvent)
	if got := h.RejectedBy(RejectKeyRevoked, SignalEvent); got != 0 {
		t.Errorf("RejectedBy(key_revoked, event) = %d, want 0 (пары нет в наборе)", got)
	}
}

// TestPprofRejectCounters — каждый отказ /profiles/pprof виден
// gotcha_ingest_rejected_total с ПРАВИЛЬНОЙ причиной: дежурный обязан
// отличать «клиента троттлит» от «квота исчерпана» и «тело битое» от «тело
// слишком большое», а не видеть один общий рост 4xx.
func TestPprofRejectCounters(t *testing.T) {
	t.Run("profiles-disabled", func(t *testing.T) {
		// h.Profiles == nil → 202 без записи и БЕЗ отказа: выключенный приём
		// профилей — не отказ, счётчик расти не должен.
		h := newRejectHandler(1 << 20)
		before := h.RejectedBy(RejectMalformed, SignalProfile)
		w := httptest.NewRecorder()
		h.pprofIngest(w, pprofRequest(strings.NewReader("что угодно"), ""))
		if w.Code != http.StatusAccepted {
			t.Fatalf("status = %d, want 202", w.Code)
		}
		if got := h.RejectedBy(RejectMalformed, SignalProfile); got != before {
			t.Errorf("RejectedBy(malformed, profile) = %d, want %d (выключенный приём — не отказ)", got, before)
		}
	})

	t.Run("rate-limit", func(t *testing.T) {
		h := newRejectHandler(1 << 20)
		h.Profiles = &countingProfileSink{}
		now := time.Unix(0, 0)
		h.SetRateLimit(func() time.Time { return now }, 1, 1) // 1 ток/с, запас 1

		// Первый запрос съедает единственный токен (чем он кончится — неважно).
		h.pprofIngest(httptest.NewRecorder(), pprofRequest(strings.NewReader("x"), ""))

		before := h.RejectedBy(RejectRateLimit, SignalProfile)
		w := httptest.NewRecorder()
		h.pprofIngest(w, pprofRequest(strings.NewReader("x"), ""))
		if w.Code != http.StatusTooManyRequests {
			t.Fatalf("status = %d, want 429", w.Code)
		}
		if got := h.RejectedBy(RejectRateLimit, SignalProfile); got != before+1 {
			t.Errorf("RejectedBy(rate_limit, profile) = %d, want %d", got, before+1)
		}
	})

	t.Run("quota", func(t *testing.T) {
		h := newRejectHandler(1 << 20)
		h.Profiles = &countingProfileSink{}
		h.ProfileQuota = &fixedQuotaChecker{n: 0}

		before := h.RejectedBy(RejectQuota, SignalProfile)
		w := httptest.NewRecorder()
		h.pprofIngest(w, pprofRequest(strings.NewReader("x"), ""))
		if w.Code != http.StatusTooManyRequests {
			t.Fatalf("status = %d, want 429", w.Code)
		}
		if w.Header().Get("Retry-After") == "" {
			t.Error("Retry-After отсутствует на 429 по квоте")
		}
		if got := h.RejectedBy(RejectQuota, SignalProfile); got != before+1 {
			t.Errorf("RejectedBy(quota, profile) = %d, want %d", got, before+1)
		}
	})

	t.Run("bad-body-encoding", func(t *testing.T) {
		// Content-Encoding: gzip на не-gzip теле — h.body падает на gzip.NewReader.
		h := newRejectHandler(1 << 20)
		h.Profiles = &countingProfileSink{}

		before := h.RejectedBy(RejectMalformed, SignalProfile)
		w := httptest.NewRecorder()
		h.pprofIngest(w, pprofRequest(strings.NewReader("это не gzip"), "gzip"))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", w.Code)
		}
		if got := h.RejectedBy(RejectMalformed, SignalProfile); got != before+1 {
			t.Errorf("RejectedBy(malformed, profile) = %d, want %d", got, before+1)
		}
	})

	t.Run("body-too-large", func(t *testing.T) {
		// Тело сверх GOTCHA_MAX_EVENT_BYTES → 413 и reason=too_large, НЕ malformed.
		h := newRejectHandler(8)
		h.Profiles = &countingProfileSink{}

		beforeLarge := h.RejectedBy(RejectTooLarge, SignalProfile)
		beforeBad := h.RejectedBy(RejectMalformed, SignalProfile)
		w := httptest.NewRecorder()
		h.pprofIngest(w, pprofRequest(strings.NewReader(strings.Repeat("x", 500)), ""))
		if w.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, want 413", w.Code)
		}
		if got := h.RejectedBy(RejectTooLarge, SignalProfile); got != beforeLarge+1 {
			t.Errorf("RejectedBy(too_large, profile) = %d, want %d", got, beforeLarge+1)
		}
		if got := h.RejectedBy(RejectMalformed, SignalProfile); got != beforeBad {
			t.Errorf("RejectedBy(malformed, profile) = %d, want %d (превышение лимита — не битое тело)", got, beforeBad)
		}
	})

	t.Run("body-read-error", func(t *testing.T) {
		// Оборванное чтение тела — malformed, а не too_large.
		h := newRejectHandler(1 << 20)
		h.Profiles = &countingProfileSink{}

		beforeBad := h.RejectedBy(RejectMalformed, SignalProfile)
		beforeLarge := h.RejectedBy(RejectTooLarge, SignalProfile)
		w := httptest.NewRecorder()
		h.pprofIngest(w, pprofRequest(errReader{}, ""))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", w.Code)
		}
		if got := h.RejectedBy(RejectMalformed, SignalProfile); got != beforeBad+1 {
			t.Errorf("RejectedBy(malformed, profile) = %d, want %d", got, beforeBad+1)
		}
		if got := h.RejectedBy(RejectTooLarge, SignalProfile); got != beforeLarge {
			t.Errorf("RejectedBy(too_large, profile) = %d, want %d (оборванное чтение — не превышение)", got, beforeLarge)
		}
	})

	t.Run("corrupt-gzip-header", func(t *testing.T) {
		// Тело начинается gzip-магией (0x1f 0x8b), но заголовок оборван:
		// gunzipLimited отдаёт ошибку, НЕ равную ErrTooLarge → 400 malformed,
		// а не 413. pprof приходит gzip'ом ВНУТРИ тела (без Content-Encoding),
		// поэтому эта ветка отдельна от bad-body-encoding выше.
		h := newRejectHandler(1 << 20)
		sink := &countingProfileSink{}
		h.Profiles = sink

		beforeBad := h.RejectedBy(RejectMalformed, SignalProfile)
		beforeLarge := h.RejectedBy(RejectTooLarge, SignalProfile)
		w := httptest.NewRecorder()
		h.pprofIngest(w, pprofRequest(bytes.NewReader([]byte{0x1f, 0x8b, 0x08}), ""))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", w.Code)
		}
		if got := h.RejectedBy(RejectMalformed, SignalProfile); got != beforeBad+1 {
			t.Errorf("RejectedBy(malformed, profile) = %d, want %d", got, beforeBad+1)
		}
		if got := h.RejectedBy(RejectTooLarge, SignalProfile); got != beforeLarge {
			t.Errorf("RejectedBy(too_large, profile) = %d, want %d (битый gzip-заголовок — не бомба)", got, beforeLarge)
		}
		if sink.n != 0 {
			t.Errorf("профилей записано %d, want 0", sink.n)
		}
	})

	t.Run("malformed-pprof", func(t *testing.T) {
		// Тело не gzip вовсе (gunzipLimited пропускает его как есть) и не pprof:
		// отказ приходит от ParsePprof и тоже обязан быть malformed.
		h := newRejectHandler(1 << 20)
		sink := &countingProfileSink{}
		h.Profiles = sink

		before := h.RejectedBy(RejectMalformed, SignalProfile)
		w := httptest.NewRecorder()
		h.pprofIngest(w, pprofRequest(strings.NewReader("совсем не pprof"), ""))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", w.Code)
		}
		if got := h.RejectedBy(RejectMalformed, SignalProfile); got != before+1 {
			t.Errorf("RejectedBy(malformed, profile) = %d, want %d", got, before+1)
		}
		if sink.n != 0 {
			t.Errorf("профилей записано %d, want 0", sink.n)
		}
	})
}

// TestOTLPLogsRejectCounters — отказы /v1/logs с правильными причинами.
func TestOTLPLogsRejectCounters(t *testing.T) {
	t.Run("unsupported-content-type", func(t *testing.T) {
		h := newRejectHandler(1 << 20)
		h.Logs = &collectLogSink{}
		w := httptest.NewRecorder()
		h.otlpLogs(w, otlpLogsRequest(strings.NewReader(""), "text/plain", ""))
		if w.Code != http.StatusUnsupportedMediaType {
			t.Fatalf("status = %d, want 415", w.Code)
		}
	})

	t.Run("rate-limit", func(t *testing.T) {
		h := newRejectHandler(1 << 20)
		h.Logs = &collectLogSink{}
		now := time.Unix(0, 0)
		h.SetRateLimit(func() time.Time { return now }, 1, 1)

		h.otlpLogs(httptest.NewRecorder(), otlpLogsRequest(strings.NewReader(""), "application/x-protobuf", ""))

		before := h.RejectedBy(RejectRateLimit, SignalLog)
		w := httptest.NewRecorder()
		h.otlpLogs(w, otlpLogsRequest(strings.NewReader(""), "application/x-protobuf", ""))
		if w.Code != http.StatusTooManyRequests {
			t.Fatalf("status = %d, want 429", w.Code)
		}
		if got := h.RejectedBy(RejectRateLimit, SignalLog); got != before+1 {
			t.Errorf("RejectedBy(rate_limit, log) = %d, want %d", got, before+1)
		}
	})

	t.Run("bad-body-encoding", func(t *testing.T) {
		h := newRejectHandler(1 << 20)
		h.Logs = &collectLogSink{}

		before := h.RejectedBy(RejectMalformed, SignalLog)
		w := httptest.NewRecorder()
		h.otlpLogs(w, otlpLogsRequest(strings.NewReader("не gzip"), "application/x-protobuf", "gzip"))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", w.Code)
		}
		if got := h.RejectedBy(RejectMalformed, SignalLog); got != before+1 {
			t.Errorf("RejectedBy(malformed, log) = %d, want %d", got, before+1)
		}
	})

	t.Run("body-read-error", func(t *testing.T) {
		h := newRejectHandler(1 << 20)
		h.Logs = &collectLogSink{}

		beforeBad := h.RejectedBy(RejectMalformed, SignalLog)
		beforeLarge := h.RejectedBy(RejectTooLarge, SignalLog)
		w := httptest.NewRecorder()
		h.otlpLogs(w, otlpLogsRequest(errReader{}, "application/x-protobuf", ""))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", w.Code)
		}
		if got := h.RejectedBy(RejectMalformed, SignalLog); got != beforeBad+1 {
			t.Errorf("RejectedBy(malformed, log) = %d, want %d", got, beforeBad+1)
		}
		if got := h.RejectedBy(RejectTooLarge, SignalLog); got != beforeLarge {
			t.Errorf("RejectedBy(too_large, log) = %d, want %d (оборванное чтение — не превышение)", got, beforeLarge)
		}
	})

	t.Run("malformed-payload", func(t *testing.T) {
		h := newRejectHandler(1 << 20)
		h.Logs = &collectLogSink{}

		before := h.RejectedBy(RejectMalformed, SignalLog)
		w := httptest.NewRecorder()
		h.otlpLogs(w, otlpLogsRequest(bytes.NewReader([]byte{0xff, 0xff, 0xff, 0xff}), "application/x-protobuf", ""))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", w.Code)
		}
		if got := h.RejectedBy(RejectMalformed, SignalLog); got != before+1 {
			t.Errorf("RejectedBy(malformed, log) = %d, want %d", got, before+1)
		}
	})

	t.Run("quota", func(t *testing.T) {
		sink := &collectLogSink{}
		h := newRejectHandler(1 << 20)
		h.Logs = sink
		h.LogQuota = &fixedQuotaChecker{n: 0}

		raw, err := proto.Marshal(&logspb.LogsData{ResourceLogs: []*logspb.ResourceLogs{{
			ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{{Body: logStrVal("hello")}}}},
		}}})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}

		before := h.RejectedBy(RejectQuota, SignalLog)
		w := httptest.NewRecorder()
		h.otlpLogs(w, otlpLogsRequest(bytes.NewReader(raw), "application/x-protobuf", ""))
		if w.Code != http.StatusTooManyRequests {
			t.Fatalf("status = %d, want 429", w.Code)
		}
		if got := h.RejectedBy(RejectQuota, SignalLog); got != before+1 {
			t.Errorf("RejectedBy(quota, log) = %d, want %d", got, before+1)
		}
		if len(sink.records) != 0 {
			t.Errorf("records = %d, want 0 (квота исчерпана)", len(sink.records))
		}
	})
}

// TestNDJSONLogsRejectCounters — отказы /logs (NDJSON) с правильными причинами.
func TestNDJSONLogsRejectCounters(t *testing.T) {
	t.Run("missing-bearer", func(t *testing.T) {
		h := newRejectHandler(1 << 20)
		h.Logs = &collectLogSink{}

		before := h.RejectedBy(RejectKeyUnknown, SignalLog)
		req := httptest.NewRequest("POST", "/logs", strings.NewReader(`{"message":"hi"}`+"\n"))
		w := httptest.NewRecorder()
		h.logsNDJSON(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", w.Code)
		}
		if got := h.RejectedBy(RejectKeyUnknown, SignalLog); got != before+1 {
			t.Errorf("RejectedBy(key_unknown, log) = %d, want %d", got, before+1)
		}
	})

	t.Run("rate-limit", func(t *testing.T) {
		sink := &collectLogSink{}
		h := newRejectHandler(1 << 20)
		h.Logs = sink
		now := time.Unix(0, 0)
		h.SetRateLimit(func() time.Time { return now }, 1, 1)

		if w := postNDJSON(t, h, `{"message":"one"}`+"\n", false); w.Code != http.StatusOK {
			t.Fatalf("первый запрос: status = %d, want 200", w.Code)
		}

		before := h.RejectedBy(RejectRateLimit, SignalLog)
		w := postNDJSON(t, h, `{"message":"two"}`+"\n", false)
		if w.Code != http.StatusTooManyRequests {
			t.Fatalf("второй запрос: status = %d, want 429", w.Code)
		}
		if got := h.RejectedBy(RejectRateLimit, SignalLog); got != before+1 {
			t.Errorf("RejectedBy(rate_limit, log) = %d, want %d", got, before+1)
		}
		if len(sink.records) != 1 {
			t.Errorf("records = %d, want 1 (отброшенный по лимиту батч не пишется)", len(sink.records))
		}
	})

	t.Run("body-read-error", func(t *testing.T) {
		// ParseNDJSON вернула I/O-ошибку: частичный результат отбрасывается
		// целиком, причина — malformed, не too_large.
		sink := &collectLogSink{}
		h := newRejectHandler(1 << 20)
		h.Logs = sink

		beforeBad := h.RejectedBy(RejectMalformed, SignalLog)
		beforeLarge := h.RejectedBy(RejectTooLarge, SignalLog)
		req := httptest.NewRequest("POST", "/logs", errReader{})
		req.Header.Set("Authorization", "Bearer pub")
		w := httptest.NewRecorder()
		h.logsNDJSON(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", w.Code)
		}
		if got := h.RejectedBy(RejectMalformed, SignalLog); got != beforeBad+1 {
			t.Errorf("RejectedBy(malformed, log) = %d, want %d", got, beforeBad+1)
		}
		if got := h.RejectedBy(RejectTooLarge, SignalLog); got != beforeLarge {
			t.Errorf("RejectedBy(too_large, log) = %d, want %d (оборванное чтение — не превышение)", got, beforeLarge)
		}
		if len(sink.records) != 0 {
			t.Errorf("records = %d, want 0 (частичный батч отбрасывается целиком)", len(sink.records))
		}
	})
}
