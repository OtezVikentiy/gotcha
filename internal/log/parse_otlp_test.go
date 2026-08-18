package log

import (
	"fmt"
	"strings"
	"testing"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
)

func strVal(s string) *commonpb.AnyValue {
	return &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: s}}
}

func kv(k, v string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: strVal(v)}
}

func fallbackTS() time.Time { return time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC) }

// resourceLogs собирает один ResourceLogs с одним ScopeLogs и заданными записями.
func resourceLogs(resAttrs []*commonpb.KeyValue, records ...*logspb.LogRecord) []*logspb.ResourceLogs {
	return []*logspb.ResourceLogs{{
		Resource: &resourcepb.Resource{Attributes: resAttrs},
		ScopeLogs: []*logspb.ScopeLogs{{
			LogRecords: records,
		}},
	}}
}

func TestMapOTLPLogsFullRecord(t *testing.T) {
	ts := fallbackTS().Add(-time.Hour)
	rl := resourceLogs(
		[]*commonpb.KeyValue{kv("service.name", "api"), kv("deployment.environment", "prod")},
		&logspb.LogRecord{
			TimeUnixNano:         uint64(ts.UnixNano()),
			ObservedTimeUnixNano: uint64(fallbackTS().Add(-time.Minute).UnixNano()), // ДОЛЖНО игнорироваться
			SeverityNumber:       logspb.SeverityNumber_SEVERITY_NUMBER_ERROR,
			SeverityText:         "ERROR",
			Body:                 strVal("boom"),
			TraceId:              []byte{0x01, 0x02, 0x03, 0x04},
			SpanId:               []byte{0xaa, 0xbb},
			Attributes:           []*commonpb.KeyValue{kv("http.status_code", "500")},
		},
	)

	out := MapOTLPLogs(rl, fallbackTS())
	if len(out) != 1 {
		t.Fatalf("len(out) = %d, want 1", len(out))
	}
	r := out[0]

	if !r.Timestamp.Equal(ts) {
		t.Errorf("Timestamp = %v, want %v", r.Timestamp, ts)
	}
	if !r.ObservedTS.Equal(fallbackTS()) {
		t.Errorf("ObservedTS = %v, want fallback %v (не ObservedTimeUnixNano)", r.ObservedTS, fallbackTS())
	}
	if r.Severity != SevError {
		t.Errorf("Severity = %q, want %q", r.Severity, SevError)
	}
	if r.SeverityText != "ERROR" {
		t.Errorf("SeverityText = %q, want ERROR", r.SeverityText)
	}
	if r.Body != "boom" {
		t.Errorf("Body = %q, want boom", r.Body)
	}
	if r.TraceID != "01020304" {
		t.Errorf("TraceID = %q, want 01020304", r.TraceID)
	}
	if r.SpanID != "aabb" {
		t.Errorf("SpanID = %q, want aabb", r.SpanID)
	}
	if r.Service != "api" {
		t.Errorf("Service = %q, want api", r.Service)
	}
	if r.Environment != "prod" {
		t.Errorf("Environment = %q, want prod", r.Environment)
	}
	if r.LogAttributes["http.status_code"] != "500" {
		t.Errorf("LogAttributes[http.status_code] = %q, want 500", r.LogAttributes["http.status_code"])
	}
	if r.ResourceAttrs["service.name"] != "api" {
		t.Errorf("ResourceAttrs[service.name] = %q, want api", r.ResourceAttrs["service.name"])
	}
}

// SeverityNumber==0 → severity вычисляется из текста, а не дефолтится в info.
func TestMapOTLPLogsSeverityFallbackToText(t *testing.T) {
	rl := resourceLogs(nil, &logspb.LogRecord{
		SeverityNumber: 0,
		SeverityText:   "ERROR",
		Body:           strVal("x"),
	})
	out := MapOTLPLogs(rl, fallbackTS())
	if out[0].Severity != SevError {
		t.Errorf("Severity = %q, want %q (fallback на текст при number==0)", out[0].Severity, SevError)
	}
	if out[0].SeverityNumber != 0 {
		t.Errorf("SeverityNumber = %d, want 0 (сырое значение сохраняется)", out[0].SeverityNumber)
	}
}

// TimeUnixNano==0 → timestamp = fallback (серверное время приёма), не ObservedTimeUnixNano.
func TestMapOTLPLogsTimestampZeroUsesFallback(t *testing.T) {
	rl := resourceLogs(nil, &logspb.LogRecord{
		TimeUnixNano:         0,
		ObservedTimeUnixNano: uint64(fallbackTS().Add(-time.Hour).UnixNano()),
		Body:                 strVal("x"),
	})
	out := MapOTLPLogs(rl, fallbackTS())
	if !out[0].Timestamp.Equal(fallbackTS()) {
		t.Errorf("Timestamp = %v, want fallback %v", out[0].Timestamp, fallbackTS())
	}
}

// Body-структура (kvlist) сериализуется в JSON-строку.
func TestMapOTLPLogsBodyKvlist(t *testing.T) {
	body := &commonpb.AnyValue{Value: &commonpb.AnyValue_KvlistValue{
		KvlistValue: &commonpb.KeyValueList{Values: []*commonpb.KeyValue{
			kv("msg", "hello"), kv("code", "42"),
		}},
	}}
	rl := resourceLogs(nil, &logspb.LogRecord{Body: body})
	out := MapOTLPLogs(rl, fallbackTS())
	got := out[0].Body
	if !strings.HasPrefix(got, "{") || !strings.Contains(got, `"msg":"hello"`) || !strings.Contains(got, `"code":"42"`) {
		t.Errorf("Body = %q, want JSON-объект с msg/code", got)
	}
}

// Body >64КиБ обрезается так, что итог (вместе с маркером усечения) ≤ maxBodyBytes.
func TestMapOTLPLogsBodyCapped(t *testing.T) {
	huge := strings.Repeat("a", maxBodyBytes+1000)
	rl := resourceLogs(nil, &logspb.LogRecord{Body: strVal(huge)})
	out := MapOTLPLogs(rl, fallbackTS())
	got := out[0].Body
	if len(got) > maxBodyBytes {
		t.Fatalf("len(Body) = %d, want <= %d", len(got), maxBodyBytes)
	}
	if !strings.HasSuffix(got, "…(truncated)") {
		t.Errorf("Body не содержит маркер усечения: %q", got[max(0, len(got)-30):])
	}
}

// Потолок числа записей на запрос.
func TestMapOTLPLogsMaxPerRequest(t *testing.T) {
	records := make([]*logspb.LogRecord, maxLogsPerRequest+50)
	for i := range records {
		records[i] = &logspb.LogRecord{Body: strVal("x")}
	}
	rl := resourceLogs(nil, records...)
	out := MapOTLPLogs(rl, fallbackTS())
	if len(out) != maxLogsPerRequest {
		t.Errorf("len(out) = %d, want %d", len(out), maxLogsPerRequest)
	}
}

// Окно таймстемпов: значение старше now-90d подтягивается к нижней границе.
func TestMapOTLPLogsTimestampWindowLowerBound(t *testing.T) {
	tooOld := fallbackTS().Add(-100 * 24 * time.Hour)
	rl := resourceLogs(nil, &logspb.LogRecord{
		TimeUnixNano: uint64(tooOld.UnixNano()),
		Body:         strVal("x"),
	})
	out := MapOTLPLogs(rl, fallbackTS())
	wantLo := fallbackTS().Add(-90 * 24 * time.Hour)
	if !out[0].Timestamp.Equal(wantLo) {
		t.Errorf("Timestamp = %v, want нижняя граница %v", out[0].Timestamp, wantLo)
	}
}

// Окно таймстемпов: значение новее now+24h подтягивается к верхней границе.
func TestMapOTLPLogsTimestampWindowUpperBound(t *testing.T) {
	tooNew := fallbackTS().Add(48 * time.Hour)
	rl := resourceLogs(nil, &logspb.LogRecord{
		TimeUnixNano: uint64(tooNew.UnixNano()),
		Body:         strVal("x"),
	})
	out := MapOTLPLogs(rl, fallbackTS())
	wantHi := fallbackTS().Add(24 * time.Hour)
	if !out[0].Timestamp.Equal(wantHi) {
		t.Errorf("Timestamp = %v, want верхняя граница %v", out[0].Timestamp, wantHi)
	}
}

// deployment.environment (старая семконвенция, без .name) тоже промотируется,
// когда текущего ключа нет.
func TestMapOTLPLogsPromoteLegacyEnvironmentKey(t *testing.T) {
	rl := resourceLogs([]*commonpb.KeyValue{kv("deployment.environment", "staging")},
		&logspb.LogRecord{Body: strVal("x")})
	out := MapOTLPLogs(rl, fallbackTS())
	if out[0].Environment != "staging" {
		t.Errorf("Environment = %q, want staging", out[0].Environment)
	}
}

// deployment.environment.name (текущая семконвенция) побеждает старую при
// наличии обеих — порядок обхода не должен затирать уже найденное значение.
func TestMapOTLPLogsPromoteEnvironmentNamePreferred(t *testing.T) {
	rl := resourceLogs([]*commonpb.KeyValue{
		kv("deployment.environment.name", "prod"),
		kv("deployment.environment", "legacy-value"),
	}, &logspb.LogRecord{Body: strVal("x")})
	out := MapOTLPLogs(rl, fallbackTS())
	if out[0].Environment != "prod" {
		t.Errorf("Environment = %q, want prod (текущая семконвенция приоритетнее)", out[0].Environment)
	}
}

// NUL в атрибутах и в body вырезается (PG на text падает на \x00).
func TestMapOTLPLogsNulScrubbed(t *testing.T) {
	rl := resourceLogs(nil, &logspb.LogRecord{
		Body:       strVal("boo\x00m"),
		Attributes: []*commonpb.KeyValue{kv("k\x00ey", "va\x00l")},
	})
	out := MapOTLPLogs(rl, fallbackTS())
	if out[0].Body != "boom" {
		t.Errorf("Body = %q, want boom (NUL вырезан)", out[0].Body)
	}
	if v, ok := out[0].LogAttributes["key"]; !ok || v != "val" {
		t.Errorf("LogAttributes[key] = %q, ok=%v, want val/true (NUL вырезан из ключа и значения)", v, ok)
	}
}

// Атрибуты сверх maxAttrKeys каппятся детерминированно (по отсортированным
// ключам) — калька теста metric.attrsToMap.
func TestMapOTLPLogsAttributesCapped(t *testing.T) {
	attrs := make([]*commonpb.KeyValue, 0, maxAttrKeys+10)
	for i := 0; i < maxAttrKeys+10; i++ {
		attrs = append(attrs, kv(fmt.Sprintf("attr-%03d", i), "v"))
	}
	rl := resourceLogs(nil, &logspb.LogRecord{Body: strVal("x"), Attributes: attrs})
	out := MapOTLPLogs(rl, fallbackTS())
	if len(out[0].LogAttributes) != maxAttrKeys {
		t.Errorf("len(LogAttributes) = %d, want %d", len(out[0].LogAttributes), maxAttrKeys)
	}
}

// Скалярные типы AnyValue (bool/int/double) в атрибутах и в теле — строковое
// представление, а не JSON.
func TestMapOTLPLogsScalarAttributeTypes(t *testing.T) {
	attrs := []*commonpb.KeyValue{
		{Key: "ok", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: true}}},
		{Key: "n", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: 42}}},
		{Key: "f", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: 3.5}}},
	}
	body := &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: false}}
	rl := resourceLogs(nil, &logspb.LogRecord{Body: body, Attributes: attrs})
	out := MapOTLPLogs(rl, fallbackTS())
	if out[0].LogAttributes["ok"] != "true" || out[0].LogAttributes["n"] != "42" || out[0].LogAttributes["f"] != "3.5" {
		t.Errorf("LogAttributes = %+v, want ok=true n=42 f=3.5", out[0].LogAttributes)
	}
	if out[0].Body != "false" {
		t.Errorf("Body = %q, want false", out[0].Body)
	}
}

// Body-массив (array) тоже сериализуется в JSON, как kvlist.
func TestMapOTLPLogsBodyArray(t *testing.T) {
	body := &commonpb.AnyValue{Value: &commonpb.AnyValue_ArrayValue{
		ArrayValue: &commonpb.ArrayValue{Values: []*commonpb.AnyValue{strVal("a"), strVal("b")}},
	}}
	rl := resourceLogs(nil, &logspb.LogRecord{Body: body})
	out := MapOTLPLogs(rl, fallbackTS())
	if out[0].Body != `["a","b"]` {
		t.Errorf("Body = %q, want [\"a\",\"b\"]", out[0].Body)
	}
}

// SeverityNumber вне диапазона uint8 (недоверенный клиент) не оборачивается
// молча — хранится как 0, а не как испорченное число.
func TestMapOTLPLogsSeverityNumberOutOfRangeStoredAsZero(t *testing.T) {
	rl := resourceLogs(nil, &logspb.LogRecord{
		SeverityNumber: logspb.SeverityNumber(300),
		Body:           strVal("x"),
	})
	out := MapOTLPLogs(rl, fallbackTS())
	if out[0].SeverityNumber != 0 {
		t.Errorf("SeverityNumber = %d, want 0 (вне диапазона uint8, не 300%%256)", out[0].SeverityNumber)
	}
}

// Тело точно на границе (<= maxBodyBytes) не трогается — маркер добавляется
// только когда усечение реально требуется.
func TestMapOTLPLogsBodyExactlyAtCapNotTruncated(t *testing.T) {
	exact := strings.Repeat("a", maxBodyBytes)
	rl := resourceLogs(nil, &logspb.LogRecord{Body: strVal(exact)})
	out := MapOTLPLogs(rl, fallbackTS())
	if out[0].Body != exact {
		t.Errorf("Body усечено на границе, len = %d, want %d без маркера", len(out[0].Body), maxBodyBytes)
	}
}

// severity_text длиннее 64 рун каппится (capRunes) — недоверенный текст не
// должен раздувать LowCardinality-колонку.
func TestMapOTLPLogsSeverityTextCapped(t *testing.T) {
	long := strings.Repeat("э", 100) // многобайтовая руна — заодно проверяет рунный, не байтовый, кап
	rl := resourceLogs(nil, &logspb.LogRecord{SeverityText: long, Body: strVal("x")})
	out := MapOTLPLogs(rl, fallbackTS())
	if r := []rune(out[0].SeverityText); len(r) != 64 {
		t.Errorf("len([]rune(SeverityText)) = %d, want 64", len(r))
	}
}

// Значение атрибута лога длиннее 200 рун каппится.
func TestMapOTLPLogsAttributeValueCapped(t *testing.T) {
	long := strings.Repeat("x", 250)
	rl := resourceLogs(nil, &logspb.LogRecord{
		Body:       strVal("x"),
		Attributes: []*commonpb.KeyValue{kv("k", long)},
	})
	out := MapOTLPLogs(rl, fallbackTS())
	if got := out[0].LogAttributes["k"]; len(got) != 200 {
		t.Errorf("len(LogAttributes[k]) = %d, want 200", len(got))
	}
}
