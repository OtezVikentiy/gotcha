package ingest

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/protobuf/proto"

	"gitflic.ru/otezvikentiy/gotcha/internal/log"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
)

// collectLogSink копит принятые записи лога — для проверки санитизации,
// квоты и наличия/отсутствия записи (LogSink nil-noop).
type collectLogSink struct{ records []log.LogRecord }

func (s *collectLogSink) Add(_ int64, r log.LogRecord) {
	s.records = append(s.records, r)
}

// logStrVal/logKV — сборка AnyValue/KeyValue для OTLP LogRecord, калька strVal/
// kv из internal/log/parse_otlp_test.go (другой пакет, свои копии).
func logStrVal(s string) *commonpb.AnyValue {
	return &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: s}}
}

func logKV(k, v string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: logStrVal(v)}
}

// postOTLPLogs — калька postOTLPMetrics (otlp_test.go:1507): POST /v1/logs с
// protobuf-телом и Bearer-DSN auth.
func postOTLPLogs(t *testing.T, h *Handler, rl []*logspb.ResourceLogs) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := proto.Marshal(&logspb.LogsData{ResourceLogs: rl})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest("POST", "/v1/logs", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("Authorization", "Bearer pub")
	w := httptest.NewRecorder()
	h.otlpLogs(w, req)
	return w
}

// postNDJSON — POST /logs. gzipped=true шлёт тело gzip'ом с Content-Encoding
// (проверка, что logsNDJSON читает тело через h.body, а не r.Body напрямую).
func postNDJSON(t *testing.T, h *Handler, body string, gzipped bool) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if gzipped {
		zw := gzip.NewWriter(&buf)
		if _, err := zw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
	} else {
		buf.WriteString(body)
	}
	req := httptest.NewRequest("POST", "/api/v1/logs", &buf)
	req.Header.Set("Authorization", "Bearer pub")
	if gzipped {
		req.Header.Set("Content-Encoding", "gzip")
	}
	w := httptest.NewRecorder()
	h.logsNDJSON(w, req)
	return w
}

func newLogsTestHandler(sink *collectLogSink) *Handler {
	h := NewHandler(NewKeyCache(stubKeyResolver{key: org.Key{ProjectID: 1, OrgID: 1, Kind: org.KindLegacy}}), nil, nil, 1<<20)
	h.Logs = sink
	return h
}

// accepted декодирует ответ NDJSON-эндпоинта {"accepted":N}.
func accepted(t *testing.T, w *httptest.ResponseRecorder) int {
	t.Helper()
	var body struct {
		Accepted int `json:"accepted"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v (body=%s)", err, w.Body.String())
	}
	return body.Accepted
}

// --- OTLP /v1/logs ---

func TestOTLPLogsBasic(t *testing.T) {
	sink := &collectLogSink{}
	h := newLogsTestHandler(sink)

	rl := []*logspb.ResourceLogs{{
		Resource:  &resourcepb.Resource{Attributes: []*commonpb.KeyValue{logKV("service.name", "api")}},
		ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{{Body: logStrVal("hello")}}}},
	}}
	w := postOTLPLogs(t, h, rl)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", w.Code, w.Body.String())
	}
	if len(sink.records) != 1 {
		t.Fatalf("records = %d, want 1", len(sink.records))
	}
	if sink.records[0].Body != "hello" {
		t.Errorf("Body = %q, want hello", sink.records[0].Body)
	}
	if sink.records[0].Service != "api" {
		t.Errorf("Service = %q, want api", sink.records[0].Service)
	}
}

// TestOTLPLogsJSONHexTraceID — Fix A: OTLP/JSON кодирует trace_id/span_id как
// HEX (не base64), а protojson без предварительного otlpJSONHexIDs молча
// декодирует hex как base64 и портит id той же формы, но другого значения
// (см. TestOTLPUnmarshalJSONHexIDs в otlp_test.go — тот же класс дефекта для
// трасс). До фикса otlpUnmarshalLogs шёл в protojson напрямую — эта дыра была
// не покрыта тестами: parse_otlp_test строит структуры напрямую, минуя JSON.
func TestOTLPLogsJSONHexTraceID(t *testing.T) {
	const (
		wantTrace = "ab0102030405060708090a0b0c0d0eff"
		wantSpan  = "deadbeef01020304"
	)
	sink := &collectLogSink{}
	h := newLogsTestHandler(sink)

	body := `{"resourceLogs":[{"scopeLogs":[{"logRecords":[
		{"body":{"stringValue":"hello"},
		 "traceId":"` + wantTrace + `",
		 "spanId":"` + wantSpan + `"}]}]}]}`
	req := httptest.NewRequest("POST", "/v1/logs", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer pub")
	w := httptest.NewRecorder()
	h.otlpLogs(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", w.Code, w.Body.String())
	}
	if len(sink.records) != 1 {
		t.Fatalf("records = %d, want 1", len(sink.records))
	}
	r := sink.records[0]
	if r.TraceID != wantTrace {
		t.Errorf("TraceID = %q, want %q (hex испорчен protojson-декодированием как base64?)", r.TraceID, wantTrace)
	}
	if r.SpanID != wantSpan {
		t.Errorf("SpanID = %q, want %q", r.SpanID, wantSpan)
	}
}

func TestOTLPLogsUnauthenticated(t *testing.T) {
	fr := &fakeResolver{keys: map[string]org.Key{"pub": {ID: 1, ProjectID: 1, OrgID: 1, PublicKey: "pub"}}}
	sink := &collectLogSink{}
	h := NewHandler(NewKeyCache(fr), nil, nil, 1<<20)
	h.Logs = sink

	req := httptest.NewRequest("POST", "/v1/logs", bytes.NewReader(nil))
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("Authorization", "Bearer wrong-key")
	w := httptest.NewRecorder()
	h.otlpLogs(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
	if len(sink.records) != 0 {
		t.Errorf("records = %d, want 0 (не аутентифицирован)", len(sink.records))
	}
}

// TestOTLPLogsSinkNil: h.Logs == nil → 200 без записи (эндпоинт выключен, но
// коллектор не должен ретраить вечно).
func TestOTLPLogsSinkNil(t *testing.T) {
	h := NewHandler(NewKeyCache(stubKeyResolver{key: org.Key{ProjectID: 1, OrgID: 1, Kind: org.KindLegacy}}), nil, nil, 1<<20)

	rl := []*logspb.ResourceLogs{{
		ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{{Body: logStrVal("hello")}}}},
	}}
	w := postOTLPLogs(t, h, rl)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
}

// TestOTLPLogsTooLarge: тело сверх maxBytes читается через h.body (MaxBytesReader) →
// 413, а не 400/500.
func TestOTLPLogsTooLarge(t *testing.T) {
	sink := &collectLogSink{}
	h := NewHandler(NewKeyCache(stubKeyResolver{key: org.Key{ProjectID: 1, OrgID: 1, Kind: org.KindLegacy}}), nil, nil, 16)
	h.Logs = sink

	rl := []*logspb.ResourceLogs{{
		ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{{
			Body: logStrVal(strings.Repeat("x", 200)),
		}}}},
	}}
	w := postOTLPLogs(t, h, rl)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", w.Code)
	}
	if len(sink.records) != 0 {
		t.Errorf("records = %d, want 0", len(sink.records))
	}
}

// --- NDJSON /logs ---

func TestNDJSONLogsBasic(t *testing.T) {
	sink := &collectLogSink{}
	h := newLogsTestHandler(sink)

	body := `{"message":"line one"}` + "\n" + `{"message":"line two","level":"warn"}` + "\n"
	w := postNDJSON(t, h, body, false)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", w.Code, w.Body.String())
	}
	if len(sink.records) != 2 {
		t.Fatalf("records = %d, want 2", len(sink.records))
	}
	if got := accepted(t, w); got != 2 {
		t.Errorf(`accepted = %d, want 2 в {"accepted":N}`, got)
	}
}

// TestNDJSONLogsGzip: тело gzip'ом с Content-Encoding — logsNDJSON обязан
// читать его через h.body (распаковка), а не r.Body напрямую (спека §2.3).
func TestNDJSONLogsGzip(t *testing.T) {
	sink := &collectLogSink{}
	h := newLogsTestHandler(sink)

	body := `{"message":"gzipped line"}` + "\n"
	w := postNDJSON(t, h, body, true)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", w.Code, w.Body.String())
	}
	if len(sink.records) != 1 {
		t.Fatalf("records = %d, want 1 (gzip не распакован?)", len(sink.records))
	}
	if sink.records[0].Body != "gzipped line" {
		t.Errorf("Body = %q, want %q", sink.records[0].Body, "gzipped line")
	}
}

// TestNDJSONLogsTooLarge: несжатое тело сверх maxBytes → 413 через h.body
// (MaxBytesReader), не 400/500.
func TestNDJSONLogsTooLarge(t *testing.T) {
	sink := &collectLogSink{}
	h := NewHandler(NewKeyCache(stubKeyResolver{key: org.Key{ProjectID: 1, OrgID: 1, Kind: org.KindLegacy}}), nil, nil, 8)
	h.Logs = sink

	body := `{"message":"` + strings.Repeat("x", 200) + `"}` + "\n"
	w := postNDJSON(t, h, body, false)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", w.Code)
	}
	if len(sink.records) != 0 {
		t.Errorf("records = %d, want 0", len(sink.records))
	}
}

// TestNDJSONLogsBadBody: нечитаемая кодировка тела (Content-Encoding: gzip на
// не-gzip теле) → 400, не 500/413. Заодно проверяет self-метрику: T6 завела
// gotcha_ingest_rejected_total{reason,signal}, и (malformed, log) — одна из
// 29 пар, которые ничем не были защищены (см. countRejected в logsNDJSON).
func TestNDJSONLogsBadBody(t *testing.T) {
	sink := &collectLogSink{}
	h := newLogsTestHandler(sink)
	before := h.RejectedBy(RejectMalformed, SignalLog)

	req := httptest.NewRequest("POST", "/api/v1/logs", strings.NewReader("не gzip"))
	req.Header.Set("Authorization", "Bearer pub")
	req.Header.Set("Content-Encoding", "gzip")
	w := httptest.NewRecorder()
	h.logsNDJSON(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	if got := h.RejectedBy(RejectMalformed, SignalLog); got != before+1 {
		t.Errorf("RejectedBy(malformed, log) = %d, want %d", got, before+1)
	}
}

// TestNDJSONLogsSinkNil: h.Logs == nil → 200 без записи, {"accepted":0}.
func TestNDJSONLogsSinkNil(t *testing.T) {
	h := NewHandler(NewKeyCache(stubKeyResolver{key: org.Key{ProjectID: 1, OrgID: 1, Kind: org.KindLegacy}}), nil, nil, 1<<20)

	w := postNDJSON(t, h, `{"message":"hi"}`+"\n", false)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := accepted(t, w); got != 0 {
		t.Errorf(`accepted = %d, want 0`, got)
	}
}

// --- Санитизация ---

// TestLogsSanitizeNUL: NUL в теле лога и в NDJSON trace_id/span_id (последние
// НЕ проходят ни через один кап парсера — только через sanitizeLog) вырезаны
// перед записью.
func TestLogsSanitizeNUL(t *testing.T) {
	sink := &collectLogSink{}
	h := newLogsTestHandler(sink)

	body := `{"message":"hi\u0000there","trace_id":"ab\u0000cd","span_id":"12\u000034"}` + "\n"
	w := postNDJSON(t, h, body, false)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", w.Code, w.Body.String())
	}
	if len(sink.records) != 1 {
		t.Fatalf("records = %d, want 1", len(sink.records))
	}
	r := sink.records[0]
	if r.Body != "hithere" {
		t.Errorf("Body = %q, want %q (NUL вырезан)", r.Body, "hithere")
	}
	if r.TraceID != "abcd" {
		t.Errorf("TraceID = %q, want %q (NUL вырезан, sanitizeLog — не капается парсером)", r.TraceID, "abcd")
	}
	if r.SpanID != "1234" {
		t.Errorf("SpanID = %q, want %q (NUL вырезан)", r.SpanID, "1234")
	}
}

// TestLogsSanitizeDenylistAttribute: денилист-ключ в LogAttributes/ResourceAttrs
// маскируется скрубером — та же дисциплина, что у otlpMetrics.
func TestLogsSanitizeDenylistAttribute(t *testing.T) {
	sink := &collectLogSink{}
	h := newLogsTestHandler(sink)
	h.Scrub = NewScrubber(false, false, []string{"token"})

	rl := []*logspb.ResourceLogs{{
		Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{logKV("token", "secret-abc")}},
		ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{{
			Body:       logStrVal("hello"),
			Attributes: []*commonpb.KeyValue{logKV("token", "secret-xyz")},
		}}}},
	}}
	w := postOTLPLogs(t, h, rl)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if len(sink.records) != 1 {
		t.Fatalf("records = %d, want 1", len(sink.records))
	}
	r := sink.records[0]
	if r.LogAttributes["token"] != scrubMask {
		t.Errorf("LogAttributes[token] = %q, want маску %q", r.LogAttributes["token"], scrubMask)
	}
	if r.ResourceAttrs["token"] != scrubMask {
		t.Errorf("ResourceAttrs[token] = %q, want маску %q", r.ResourceAttrs["token"], scrubMask)
	}
}

// TestLogsSanitizeBodyURLScrub — Fix C: тело лога (`body`) — единственное
// свободнотекстовое поле пайплайна логов, и без ScrubMessage оно обходило бы
// безусловный скраб query-токенов/basic-auth в URL, применяемый к message
// событий, имени транзакции и описанию спанов (см. пайплайн событий,
// TestScrubReAuditRound2/M2 — тот же контракт: чистится ВСЕГДА, даже при
// ScrubFreeText=false). Обычный текст без URL не должен искажаться.
func TestLogsSanitizeBodyURLScrub(t *testing.T) {
	sink := &collectLogSink{}
	h := newLogsTestHandler(sink)
	h.Scrub = NewScrubber(false, false, []string{"token"}) // ScrubFreeText=false — URL всё равно чистится (M2)

	body := `{"message":"GET https://api.example/reset?token=SECRET&ok=1"}` + "\n" +
		`{"message":"DSN https://user:hunter2@db.example/app fell over"}` + "\n" +
		`{"message":"plain error without any url"}` + "\n"
	w := postNDJSON(t, h, body, false)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", w.Code, w.Body.String())
	}
	if len(sink.records) != 3 {
		t.Fatalf("records = %d, want 3", len(sink.records))
	}

	tokenMsg := sink.records[0].Body
	if strings.Contains(tokenMsg, "SECRET") || !strings.Contains(tokenMsg, "token=[scrubbed]") || !strings.Contains(tokenMsg, "ok=1") {
		t.Errorf("query-токен в теле лога не вычищен: %q", tokenMsg)
	}
	authMsg := sink.records[1].Body
	if strings.Contains(authMsg, "hunter2") || !strings.Contains(authMsg, "user:[scrubbed]@") {
		t.Errorf("basic-auth в теле лога не вычищен: %q", authMsg)
	}
	plainMsg := sink.records[2].Body
	if plainMsg != "plain error without any url" {
		t.Errorf("текст без URL исказился: %q", plainMsg)
	}
}

// TestLogsSanitizeServiceCardinality: service под гардом кардинальности —
// новое значение сверх потолка схлопывается в CardinalityOverflow.
func TestLogsSanitizeServiceCardinality(t *testing.T) {
	sink := &collectLogSink{}
	h := newLogsTestHandler(sink)
	h.Cardinality = NewCardinalityGuard(1, time.Hour)
	h.Cardinality.Value(1, FieldService, "svc-a")

	rl := []*logspb.ResourceLogs{{
		Resource:  &resourcepb.Resource{Attributes: []*commonpb.KeyValue{logKV("service.name", "svc-b")}},
		ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{{Body: logStrVal("hello")}}}},
	}}
	w := postOTLPLogs(t, h, rl)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if len(sink.records) != 1 {
		t.Fatalf("records = %d, want 1", len(sink.records))
	}
	if sink.records[0].Service != CardinalityOverflow {
		t.Errorf("Service = %q, want %q (потолок кардинальности исчерпан)", sink.records[0].Service, CardinalityOverflow)
	}
}

// TestLogsSanitizeEnvironmentCardinality: environment под тем же гардом.
func TestLogsSanitizeEnvironmentCardinality(t *testing.T) {
	sink := &collectLogSink{}
	h := newLogsTestHandler(sink)
	h.Cardinality = NewCardinalityGuard(1, time.Hour)
	h.Cardinality.Value(1, FieldEnvironment, "prod")

	rl := []*logspb.ResourceLogs{{
		Resource:  &resourcepb.Resource{Attributes: []*commonpb.KeyValue{logKV("deployment.environment", "staging")}},
		ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{{Body: logStrVal("hello")}}}},
	}}
	w := postOTLPLogs(t, h, rl)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if len(sink.records) != 1 {
		t.Fatalf("records = %d, want 1", len(sink.records))
	}
	if sink.records[0].Environment != CardinalityOverflow {
		t.Errorf("Environment = %q, want %q (потолок кардинальности исчерпан)", sink.records[0].Environment, CardinalityOverflow)
	}
}

// --- Квота ---

// TestLogsQuotaExhausted: LogQuota в 0 → 429, ничего не записано.
func TestLogsQuotaExhausted(t *testing.T) {
	sink := &collectLogSink{}
	h := newLogsTestHandler(sink)
	h.LogQuota = zeroQuotaChecker{}

	body := `{"message":"hi"}` + "\n"
	w := postNDJSON(t, h, body, false)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", w.Code)
	}
	if len(sink.records) != 0 {
		t.Errorf("records = %d, want 0 (квота исчерпана)", len(sink.records))
	}
}

// fixedQuotaChecker выдаёт ровно n единиц один раз (остаток запросов той же
// орги — 0): достаточно для проверки частичного списания в один запрос.
type fixedQuotaChecker struct{ n int64 }

func (q *fixedQuotaChecker) CheckAndCount(_ context.Context, _ int64, want int64) (int64, error) {
	if want <= q.n {
		granted := want
		q.n -= granted
		return granted, nil
	}
	granted := q.n
	q.n = 0
	return granted, nil
}

// TestLogsQuotaPartial: квота впритык на 1 запись из 2 → 200 (по 1 записи
// принято), остаток дропнут и посчитан через countDrop(dropLog) →
// DropCounter.IncDroppedLogs.
func TestLogsQuotaPartial(t *testing.T) {
	sink := &collectLogSink{}
	h := newLogsTestHandler(sink)
	h.LogQuota = &fixedQuotaChecker{n: 1}
	dc := newFakeDropCounter()
	h.DropCounter = dc

	body := `{"message":"line one"}` + "\n" + `{"message":"line two"}` + "\n"
	w := postNDJSON(t, h, body, false)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (частично влезло), body=%s", w.Code, w.Body.String())
	}
	if len(sink.records) != 1 {
		t.Fatalf("records = %d, want 1", len(sink.records))
	}
	if got := accepted(t, w); got != 1 {
		t.Errorf(`accepted = %d, want 1`, got)
	}
	if dc.logsCalls != 1 {
		t.Errorf("IncDroppedLogs вызовов = %d, want 1 (остаток посчитан в дропы)", dc.logsCalls)
	}
}
