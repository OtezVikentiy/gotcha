package ingest_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

// --- сборка OTLP-запроса (корневой SERVER-спан + db- и http.client-дети) ---

func otlpStrAttr(k, v string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{
		Value: &commonpb.AnyValue_StringValue{StringValue: v},
	}}
}

// freshExportRequest — запрос с timestamp'ами «только что»: у CH-таблиц
// transactions/spans TTL, и строка с прошлогодним временем отбрасывается прямо
// на вставке (см. freshTransactionJSON).
//
// Тело OTLP-экспорта здесь собирается как TracesData, а не как коллекторный
// ExportTraceServiceRequest: на проводе это одно и то же сообщение (см.
// TestOTLPCollectorWireBytes, где те же байты собираются руками именно так, как
// их пишет генерённый маршалер коллектора).
func freshExportRequest(traceID []byte) *tracepb.TracesData {
	end := time.Now().UTC()
	start := end.Add(-500 * time.Millisecond)
	ns := func(t time.Time) uint64 { return uint64(t.UnixNano()) }

	root := &tracepb.Span{
		TraceId:           traceID,
		SpanId:            []byte{0xde, 0xad, 0xbe, 0xef, 0x01, 0x02, 0x03, 0x04},
		Name:              "GET /checkout",
		Kind:              tracepb.Span_SPAN_KIND_SERVER,
		StartTimeUnixNano: ns(start),
		EndTimeUnixNano:   ns(end),
		Attributes: []*commonpb.KeyValue{
			otlpStrAttr("http.request.method", "GET"),
			otlpStrAttr("url.path", "/checkout"),
		},
		Status: &tracepb.Status{Code: tracepb.Status_STATUS_CODE_OK},
	}
	db := &tracepb.Span{
		TraceId:           traceID,
		SpanId:            []byte{0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88},
		ParentSpanId:      root.SpanId,
		Name:              "SELECT users",
		Kind:              tracepb.Span_SPAN_KIND_CLIENT,
		StartTimeUnixNano: ns(start.Add(100 * time.Millisecond)),
		EndTimeUnixNano:   ns(start.Add(200 * time.Millisecond)),
		Attributes: []*commonpb.KeyValue{
			otlpStrAttr("db.system", "postgresql"),
			otlpStrAttr("db.statement", "SELECT * FROM users WHERE id = 1"),
		},
	}
	httpClient := &tracepb.Span{
		TraceId:           traceID,
		SpanId:            []byte{0x99, 0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xf0, 0x01},
		ParentSpanId:      root.SpanId,
		Name:              "GET /health",
		Kind:              tracepb.Span_SPAN_KIND_CLIENT,
		StartTimeUnixNano: ns(start.Add(200 * time.Millisecond)),
		EndTimeUnixNano:   ns(start.Add(450 * time.Millisecond)),
		Attributes: []*commonpb.KeyValue{
			otlpStrAttr("http.request.method", "GET"),
			otlpStrAttr("url.full", "https://billing/health"),
		},
	}
	return &tracepb.TracesData{
		ResourceSpans: []*tracepb.ResourceSpans{{
			Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{
				otlpStrAttr("service.name", "web-01"),
				otlpStrAttr("deployment.environment", "prod"),
				otlpStrAttr("service.version", "api@1.2.3"),
			}},
			ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{root, db, httpClient}}},
		}},
	}
}

var otlpTraceID = []byte{0xab, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07,
	0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0xff}

const (
	otlpTraceIDHex = "ab0102030405060708090a0b0c0d0eff"
	otlpRootIDHex  = "deadbeef01020304"
)

func otlpProtoBody(t *testing.T, req *tracepb.TracesData) []byte {
	t.Helper()
	b, err := proto.Marshal(req)
	if err != nil {
		t.Fatalf("proto.Marshal: %v", err)
	}
	return b
}

// otlpJSONBody — тело OTLP/JSON, собранное protojson'ом: идентификаторы в нём
// уезжают в BASE64 (стандартный protobuf-JSON-маппинг байтовых полей). Это НЕ
// то, что шлёт настоящий OTLP-клиент (см. otlpJSONBodyHexIDs) — этот вариант
// оставлен затем, чтобы доказать: такое тело мы по-прежнему принимаем.
func otlpJSONBody(t *testing.T, req *tracepb.TracesData) []byte {
	t.Helper()
	b, err := protojson.Marshal(req)
	if err != nil {
		t.Fatalf("protojson.Marshal: %v", err)
	}
	return b
}

// otlpJSONBodyHexIDs — тело OTLP/JSON ровно такое, какое шлёт НАСТОЯЩИЙ клиент:
// trace_id/span_id/parent_span_id — HEX-СТРОКИ. Ровно здесь спека OTLP отступает
// от стандартного protobuf-JSON, и ровно на этом молча ломается protojson,
// декодирующий hex как base64 (он не падает: hex-символы входят в base64-алфавит,
// а 32/16 символов кратны 4).
//
// Собираем из protojson-тела, переписывая base64 обратно в hex: так в фикстуре
// не приходится дублировать всю структуру запроса руками, а на проводе получается
// байт в байт то, что пишет OTel-JS (для него JSON — кодировка ПО УМОЛЧАНИЮ) или
// otlphttpexporter с encoding: json.
func otlpJSONBodyHexIDs(t *testing.T, req *tracepb.TracesData) []byte {
	t.Helper()
	var doc any
	if err := json.Unmarshal(otlpJSONBody(t, req), &doc); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, val := range x {
				s, isStr := val.(string)
				switch k {
				case "traceId", "spanId", "parentSpanId":
					if !isStr {
						continue
					}
					raw, err := base64.StdEncoding.DecodeString(s)
					if err != nil {
						t.Fatalf("id %q не base64: %v", k, err)
					}
					x[k] = hex.EncodeToString(raw)
				default:
					walk(val)
				}
			}
		case []any:
			for _, e := range x {
				walk(e)
			}
		}
	}
	walk(doc)
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	return out
}

// collectorExportBody собирает тело POST /v1/traces ровно так, как его пишет
// настоящий OTel-коллектор: как ExportTraceServiceRequest, у которого
// единственное поле — `repeated ResourceSpans resource_spans = 1`.
//
// Коллекторный пакет (go.opentelemetry.io/proto/otlp/collector/trace/v1) сюда не
// импортируется намеренно: его сгенерённый файл тащит за собой gRPC и
// grpc-gateway, а мы от этих зависимостей и избавляемся. Поэтому framing поля 1
// (тег + длина + байты каждого ResourceSpans) выписан руками через protowire —
// это ровно то, что делает генерённый маршалер коллектора, и именно этот тест
// доказывает, что на проводе ExportTraceServiceRequest и TracesData — одно и то
// же сообщение, а не «почти».
func collectorExportBody(t *testing.T, rs []*tracepb.ResourceSpans) []byte {
	t.Helper()
	var out []byte
	for _, r := range rs {
		b, err := proto.Marshal(r)
		if err != nil {
			t.Fatalf("proto.Marshal(ResourceSpans): %v", err)
		}
		out = protowire.AppendTag(out, protowire.Number(1), protowire.BytesType)
		out = protowire.AppendBytes(out, b)
	}
	return out
}

// postOTLP шлёт тело на /v1/traces; bearer == "" → заголовка Authorization нет.
func (s *stack) postOTLP(t *testing.T, body []byte, contentType, bearer string, gzipped bool) *http.Response {
	t.Helper()
	var buf bytes.Buffer
	if gzipped {
		zw := gzip.NewWriter(&buf)
		if _, err := zw.Write(body); err != nil {
			t.Fatal(err)
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
	} else {
		buf.Write(body)
	}
	req, err := http.NewRequest("POST", s.srv.URL+"/v1/traces", &buf)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", contentType)
	if gzipped {
		req.Header.Set("Content-Encoding", "gzip")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func readAllBody(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	return b
}

// assertOTLPRows проверяет то, ради чего эндпойнт и существует: транзакция и её
// спаны (корень + 2 ребёнка) лежат в тех же таблицах, что и Sentry-путь, с
// source='otlp'.
func assertOTLPRows(t *testing.T, s *stack) {
	t.Helper()
	pid := uint64(s.project.ID)
	if got := waitCH(t, s,
		"SELECT count(*) FROM transactions WHERE project_id = ? AND source = 'otlp'", []any{pid}, 1); got != 1 {
		t.Fatalf("transactions rows with source='otlp' = %d, want 1", got)
	}
	if got := waitCH(t, s,
		"SELECT count(*) FROM spans WHERE project_id = ? AND source = 'otlp'", []any{pid}, 3); got != 3 {
		t.Fatalf("spans rows with source='otlp' = %d, want 3", got)
	}

	ctx := context.Background()
	var name, traceID, spanID, environment, release, server string
	var durationUS uint32
	if err := s.ch.QueryRow(ctx,
		`SELECT transaction, trace_id, span_id, environment, release, server_name, duration_us
		 FROM transactions WHERE project_id = ?`, pid).
		Scan(&name, &traceID, &spanID, &environment, &release, &server, &durationUS); err != nil {
		t.Fatalf("transaction row: %v", err)
	}
	if name != "GET /checkout" || traceID != otlpTraceIDHex || spanID != otlpRootIDHex ||
		environment != "prod" || release != "api@1.2.3" || server != "web-01" ||
		durationUS != 500_000 {
		t.Errorf("transaction row = %q/%q/%q/%q/%q/%q/%d",
			name, traceID, spanID, environment, release, server, durationUS)
	}

	// Идентификаторы спанов — те же, что послал клиент: по ним трейс сшивается с
	// ошибкой Sentry и ищется по id из логов пользователя.
	var dbSpanID, dbParentID string
	if err := s.ch.QueryRow(ctx,
		"SELECT span_id, parent_span_id FROM spans WHERE project_id = ? AND op = 'db'", pid).
		Scan(&dbSpanID, &dbParentID); err != nil {
		t.Fatalf("db span ids: %v", err)
	}
	if dbSpanID != "1122334455667788" || dbParentID != otlpRootIDHex {
		t.Errorf("db span ids = %q / %q, want 1122334455667788 / %s",
			dbSpanID, dbParentID, otlpRootIDHex)
	}
	if got := waitCH(t, s,
		"SELECT count(*) FROM spans WHERE project_id = ? AND trace_id = ?",
		[]any{pid, otlpTraceIDHex}, 3); got != 3 {
		t.Errorf("spans with trace_id = %s: %d, want 3", otlpTraceIDHex, got)
	}

	var op, description string
	if err := s.ch.QueryRow(ctx,
		"SELECT op, description FROM spans WHERE project_id = ? AND op = 'db'", pid).
		Scan(&op, &description); err != nil {
		t.Fatalf("db span row: %v", err)
	}
	if description != "SELECT * FROM users WHERE id = 1" {
		t.Errorf("db span description = %q", description)
	}
	if err := s.ch.QueryRow(ctx,
		"SELECT op, description FROM spans WHERE project_id = ? AND op = 'http.client'", pid).
		Scan(&op, &description); err != nil {
		t.Fatalf("http.client span row: %v", err)
	}
	if description != "GET https://billing/health" {
		t.Errorf("http.client span description = %q", description)
	}
}

// TestOTLPProtobufEndToEnd — основной путь: protobuf-запрос → строки в CH,
// ответ — пустой ExportTraceServiceResponse в protobuf (иначе коллектор считает
// экспорт неуспешным и ретраит вечно). Полностью успешный
// ExportTraceServiceResponse (partial_success не заполнен) — это сообщение без
// единого поля, то есть ПУСТОЕ тело.
func TestOTLPProtobufEndToEnd(t *testing.T) {
	s := newStack(t)
	body := otlpProtoBody(t, freshExportRequest(otlpTraceID))

	resp := s.postOTLP(t, body, "application/x-protobuf", s.key.PublicKey, false)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/x-protobuf" {
		t.Errorf("response Content-Type = %q, want application/x-protobuf", ct)
	}
	if raw := readAllBody(t, resp); len(raw) != 0 {
		t.Errorf("response body = %q, want empty protobuf message", raw)
	}

	assertOTLPRows(t, s)
}

// TestOTLPJSONEndToEnd — тот же запрос в OTLP/JSON с ХЕКСОВЫМИ id, как его шлёт
// настоящий клиент (см. otlpJSONBodyHexIDs), даёт ровно тот же результат, что и
// protobuf, а ответ приходит в JSON: пустой ExportTraceServiceResponse в
// OTLP/JSON — это `{}`.
//
// Проверка id здесь — не формальность: protojson декодирует hex как base64 и
// молча кладёт в CH id ПРАВИЛЬНОЙ ФОРМЫ и неверного значения. Такой трейс
// невозможно сшить с Sentry-ошибкой, несущей настоящий id, поиск по id из логов
// не находит ничего, а trace.Keep считает вердикт семплирования от испорченного
// id — и один трейс, пришедший из protobuf- и json-SDK, семплируется ПО-РАЗНОМУ.
func TestOTLPJSONEndToEnd(t *testing.T) {
	s := newStack(t)
	body := otlpJSONBodyHexIDs(t, freshExportRequest(otlpTraceID))

	// Фикстура обязана нести id ЛИТЕРАЛЬНЫМ hex, иначе тест проверяет не то.
	if !bytes.Contains(body, []byte(`"`+otlpTraceIDHex+`"`)) {
		t.Fatalf("в теле нет hex trace_id %q: %s", otlpTraceIDHex, body)
	}

	resp := s.postOTLP(t, body, "application/json", s.key.PublicKey, false)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("response Content-Type = %q, want application/json", ct)
	}
	if raw := readAllBody(t, resp); string(raw) != "{}" {
		t.Errorf("response body = %q, want {}", raw)
	}

	assertOTLPRows(t, s)
}

// TestOTLPJSONBase64IDsStillAccepted — правило перекодировки узкое (только hex
// ровно нужной длины), поэтому клиент, шлющий id в base64 по стандартному
// protobuf-JSON-маппингу, продолжает работать: неоднозначности нет, base64 16
// байт — это 24 символа, 8 байт — 12, ни то, ни другое не спутать с 32/16.
func TestOTLPJSONBase64IDsStillAccepted(t *testing.T) {
	s := newStack(t)
	body := otlpJSONBody(t, freshExportRequest(otlpTraceID))

	resp := s.postOTLP(t, body, "application/json", s.key.PublicKey, false)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	assertOTLPRows(t, s)
}

// TestOTLPCollectorWireBytes — тело, собранное ровно так, как его пишет
// настоящий коллектор (ExportTraceServiceRequest, поле 1 = resource_spans;
// framing выписан руками, см. collectorExportBody), принимается эндпойнтом,
// который разбирает его как TracesData. Это и есть доказательство, что
// зависимость от gRPC-пакета убрана без изменения проводного контракта.
func TestOTLPCollectorWireBytes(t *testing.T) {
	s := newStack(t)
	req := freshExportRequest(otlpTraceID)
	body := collectorExportBody(t, req.GetResourceSpans())

	// Те же самые байты, что даёт маршалинг TracesData: сообщения идентичны.
	if want := otlpProtoBody(t, req); !bytes.Equal(body, want) {
		t.Fatalf("collector wire bytes differ from TracesData bytes:\n got %x\nwant %x", body, want)
	}

	resp := s.postOTLP(t, body, "application/x-protobuf", s.key.PublicKey, false)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if raw := readAllBody(t, resp); len(raw) != 0 {
		t.Errorf("response body = %q, want empty protobuf message", raw)
	}

	assertOTLPRows(t, s)
}

// TestOTLPGzipBody — коллектор по умолчанию жмёт тело gzip'ом.
func TestOTLPGzipBody(t *testing.T) {
	s := newStack(t)
	body := otlpProtoBody(t, freshExportRequest(otlpTraceID))

	resp := s.postOTLP(t, body, "application/x-protobuf", s.key.PublicKey, true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	assertOTLPRows(t, s)
}

// TestOTLPAuthFailures — DSN-ключ едет в Authorization: Bearer (штатная опция
// headers: у OTel-экспортёров). Нет заголовка / неизвестный ключ → 401.
func TestOTLPAuthFailures(t *testing.T) {
	s := newStack(t)
	body := otlpProtoBody(t, freshExportRequest(otlpTraceID))

	if resp := s.postOTLP(t, body, "application/x-protobuf", "", false); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no bearer: status = %d, want 401", resp.StatusCode)
	}
	if resp := s.postOTLP(t, body, "application/x-protobuf",
		"00000000000000000000000000000000", false); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("unknown key: status = %d, want 401", resp.StatusCode)
	}
}

// TestOTLPTransactionQuotaExhaustedStillAcceptsErrors — OTLP тратит ТУ ЖЕ квоту
// транзакций, что и Sentry-транзакции (429 при исчерпании), и НЕ трогает квоту
// ошибок: события тем же DSN-ключом принимаются и после 429.
func TestOTLPTransactionQuotaExhaustedStillAcceptsErrors(t *testing.T) {
	s := newStack(t)
	ctx := context.Background()
	if err := s.orgSvc.SetTransactionQuota(ctx, s.org.ID, 2); err != nil {
		t.Fatalf("SetTransactionQuota: %v", err)
	}
	body := otlpProtoBody(t, freshExportRequest(otlpTraceID))

	for i := 0; i < 2; i++ {
		resp := s.postOTLP(t, body, "application/x-protobuf", s.key.PublicKey, false)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("export %d: status = %d, want 200", i, resp.StatusCode)
		}
	}
	resp := s.postOTLP(t, body, "application/x-protobuf", s.key.PublicKey, false)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("third export: status = %d, want 429", resp.StatusCode)
	}
	if ra := resp.Header.Get("Retry-After"); ra == "" {
		t.Error("Retry-After header missing")
	}

	// Квота ошибок не тронута.
	errResp := s.post(t, fmt.Sprintf("/api/%d/envelope/", s.project.ID),
		envelopeBody(testEventJSON), false, s.key.PublicKey)
	if errResp.StatusCode != http.StatusOK {
		t.Fatalf("error event after otlp quota exhausted: status = %d, want 200", errResp.StatusCode)
	}
	waitIssue(t, s.pool, s.project.ID, 1)
}

// TestOTLPSampleRateZeroWritesNothing — семплирование то же самое (trace.Keep по
// transaction_sample_rate проекта): при rate=0 запрос принимается, но в CH не
// попадает ничего.
func TestOTLPSampleRateZeroWritesNothing(t *testing.T) {
	s := newStack(t)
	ctx := context.Background()
	if _, err := s.pool.Exec(ctx,
		"UPDATE projects SET transaction_sample_rate = 0 WHERE id = $1", s.project.ID); err != nil {
		t.Fatalf("set sample rate: %v", err)
	}
	body := otlpProtoBody(t, freshExportRequest(otlpTraceID))

	resp := s.postOTLP(t, body, "application/x-protobuf", s.key.PublicKey, false)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	// Дать пайплайну и писателю заведомо больше времени, чем нужно на запись.
	time.Sleep(2 * time.Second)
	s.pipeline.Close()
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := s.spans.Close(cctx); err != nil {
		t.Fatalf("span writer close: %v", err)
	}

	pid := uint64(s.project.ID)
	var txCount, spanCount uint64
	if err := s.ch.QueryRow(ctx,
		"SELECT count(*) FROM transactions WHERE project_id = ?", pid).Scan(&txCount); err != nil {
		t.Fatalf("count transactions: %v", err)
	}
	if err := s.ch.QueryRow(ctx,
		"SELECT count(*) FROM spans WHERE project_id = ?", pid).Scan(&spanCount); err != nil {
		t.Fatalf("count spans: %v", err)
	}
	if txCount != 0 || spanCount != 0 {
		t.Fatalf("sample_rate=0: transactions=%d spans=%d, want 0/0", txCount, spanCount)
	}
}

// TestOTLPMultiServiceBatchOwnsItsSpans — batch-процессор коллектора ШТАТНО
// склеивает ResourceSpans разных сервисов в один экспорт, и трейс, прошедший
// через два сервиса, приезжает с двумя корнями (SERVER-спан второго сервиса —
// корень по правилу kind). SpanWriter копирует в строку спана transaction и
// environment ВЛАДЕЮЩЕЙ транзакции — значит, привязка спана к чужому корню
// заставляет врать и фильтр по окружению, и подробности медленных запросов по
// эндпойнту. Проверяем ровно колонки CH, из которых эти вьюхи и читают.
func TestOTLPMultiServiceBatchOwnsItsSpans(t *testing.T) {
	s := newStack(t)

	end := time.Now().UTC()
	start := end.Add(-500 * time.Millisecond)
	ns := func(t time.Time) uint64 { return uint64(t.UnixNano()) }
	feRoot := []byte{0xf0, 0x01, 0x01, 0x01, 0x01, 0x01, 0x01, 0x01}
	feClient := []byte{0xf0, 0x02, 0x02, 0x02, 0x02, 0x02, 0x02, 0x02}
	blRoot := []byte{0xb0, 0x01, 0x01, 0x01, 0x01, 0x01, 0x01, 0x01}
	blDB := []byte{0xb0, 0x02, 0x02, 0x02, 0x02, 0x02, 0x02, 0x02}

	req := &tracepb.TracesData{ResourceSpans: []*tracepb.ResourceSpans{
		{
			Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{
				otlpStrAttr("service.name", "frontend"),
				otlpStrAttr("deployment.environment", "prod"),
			}},
			ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{
				{
					TraceId: otlpTraceID, SpanId: feRoot,
					Name: "GET /a", Kind: tracepb.Span_SPAN_KIND_SERVER,
					StartTimeUnixNano: ns(start), EndTimeUnixNano: ns(end),
				},
				{
					TraceId: otlpTraceID, SpanId: feClient, ParentSpanId: feRoot,
					Name: "POST /charge", Kind: tracepb.Span_SPAN_KIND_CLIENT,
					StartTimeUnixNano: ns(start), EndTimeUnixNano: ns(end),
					Attributes: []*commonpb.KeyValue{
						otlpStrAttr("http.request.method", "POST"),
						otlpStrAttr("url.full", "https://billing/charge"),
					},
				},
			}}},
		},
		{
			Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{
				otlpStrAttr("service.name", "billing"),
				otlpStrAttr("deployment.environment", "staging"),
			}},
			ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{
				{
					TraceId: otlpTraceID, SpanId: blRoot, ParentSpanId: feClient,
					Name: "POST /charge", Kind: tracepb.Span_SPAN_KIND_SERVER,
					StartTimeUnixNano: ns(start), EndTimeUnixNano: ns(end),
				},
				{
					TraceId: otlpTraceID, SpanId: blDB, ParentSpanId: blRoot,
					Name: "SELECT accounts", Kind: tracepb.Span_SPAN_KIND_CLIENT,
					StartTimeUnixNano: ns(start), EndTimeUnixNano: ns(end),
					Attributes: []*commonpb.KeyValue{
						otlpStrAttr("db.system", "postgresql"),
						otlpStrAttr("db.statement", "SELECT * FROM accounts"),
					},
				},
			}}},
		},
	}}

	resp := s.postOTLP(t, otlpProtoBody(t, req), "application/x-protobuf", s.key.PublicKey, false)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	pid := uint64(s.project.ID)
	if got := waitCH(t, s,
		"SELECT count(*) FROM transactions WHERE project_id = ?", []any{pid}, 2); got != 2 {
		t.Fatalf("transactions = %d, want 2 (по корню на сервис)", got)
	}
	// 4 строки спанов: корень каждой транзакции пишется строкой спана тоже
	// (см. SpanWriter), плюс по одному ребёнку у каждой.
	if got := waitCH(t, s, "SELECT count(*) FROM spans WHERE project_id = ?", []any{pid}, 4); got != 4 {
		t.Fatalf("spans = %d, want 4", got)
	}

	ctx := context.Background()
	// db-спан биллинга обязан лежать под транзакцией БИЛЛИНГА и с его окружением.
	var txName, env string
	if err := s.ch.QueryRow(ctx,
		"SELECT transaction, environment FROM spans WHERE project_id = ? AND op = 'db'", pid).
		Scan(&txName, &env); err != nil {
		t.Fatalf("db span row: %v", err)
	}
	if txName != "POST /charge" || env != "staging" {
		t.Errorf("db span биллинга: transaction=%q environment=%q, want POST /charge / staging",
			txName, env)
	}
	// А http.client фронтенда — под транзакцией фронтенда, с его окружением.
	if err := s.ch.QueryRow(ctx,
		"SELECT transaction, environment FROM spans WHERE project_id = ? AND op = 'http.client'", pid).
		Scan(&txName, &env); err != nil {
		t.Fatalf("http.client span row: %v", err)
	}
	if txName != "GET /a" || env != "prod" {
		t.Errorf("http.client спан фронтенда: transaction=%q environment=%q, want GET /a / prod",
			txName, env)
	}
}
