package log

import (
	"encoding/hex"
	"encoding/json"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
)

// Защита от амплификации памяти/CPU недоверенным экспортом (тот же приём, что maxOTLPMetricPoints/maxOTLPSpans).
const maxLogsPerRequest = 10000

// Реальные сообщения столько не весят; без капа запись раздула бы CH-колонку body и буфер писателя (logRowBytes).
const maxBodyBytes = 64 << 10

// fallback — серверное время приёма: всегда идёт в ObservedTS и как запасной timestamp при TimeUnixNano==0.
func MapOTLPLogs(rl []*logspb.ResourceLogs, fallback time.Time) []LogRecord {
	var out []LogRecord
	for _, r := range rl {
		service, environment := promote(r.GetResource())
		resourceAttrs := attrsToMap(r.GetResource().GetAttributes())
		for _, sl := range r.GetScopeLogs() {
			for _, lr := range sl.GetLogRecords() {
				// Кап — ВНУТРИ вложенного цикла: один ResourceLogs с гигантским ScopeLogs иначе
				// аллоцировал бы всё, проскочив проверку снаружи.
				if len(out) >= maxLogsPerRequest {
					return out
				}
				out = append(out, mapLogRecord(lr, service, environment, resourceAttrs, fallback))
			}
		}
	}
	return out
}

func mapLogRecord(lr *logspb.LogRecord, service, environment string, resourceAttrs map[string]string, fallback time.Time) LogRecord {
	// Severity: число — источник истины при заполнении; text — запасной путь
	// ровно для number==0 (поле не заполнено), а не общий дефолт поверх числа.
	rawNumber := int32(lr.GetSeverityNumber())
	sevText := lr.GetSeverityText()
	sev := CanonFromNumber(rawNumber)
	if rawNumber == 0 {
		sev = CanonFromText(sevText)
	}

	return LogRecord{
		// observed_ts — ВСЕГДА серверное fallback-время приёма; ObservedTimeUnixNano намеренно не читаем —
		// иначе теряется защита от кривых часов клиента.
		Timestamp:  logTime(lr.GetTimeUnixNano(), fallback),
		ObservedTS: fallback,

		Severity:       sev,
		SeverityNumber: capSeverityNumber(rawNumber),
		SeverityText:   capRunes(sevText, 64),

		Body: capBytes(anyValueToString(lr.GetBody()), maxBodyBytes),

		// Пустой срез байтов даёт "" — hex.EncodeToString(nil) возвращает "".
		TraceID: hex.EncodeToString(lr.GetTraceId()),
		SpanID:  hex.EncodeToString(lr.GetSpanId()),

		Service:     service,
		Environment: environment,

		LogAttributes: attrsToMap(lr.GetAttributes()),
		ResourceAttrs: resourceAttrs,
	}
}

// Валидный диапазон спецификации — 1..24; вне uint8 (в т.ч. отрицательное) — 0, а не int32→uint8
// обёрткой: иначе 300 молча стало бы 44 и врало бы в отладке.
func capSeverityNumber(n int32) uint8 {
	if n < 0 || n > 255 {
		return 0
	}
	return uint8(n)
}

// Скаляры — обычное строковое представление; kvlist/array — JSON-строкой (в отличие от attrString,
// который для структур не разворачивает).
func anyValueToString(v *commonpb.AnyValue) string {
	switch x := v.GetValue().(type) {
	case *commonpb.AnyValue_StringValue:
		return x.StringValue
	case *commonpb.AnyValue_KvlistValue, *commonpb.AnyValue_ArrayValue:
		b, err := json.Marshal(anyValueToNative(v))
		if err != nil {
			return ""
		}
		return string(b)
	default:
		return attrString(v)
	}
}

// Нужен только для структурного тела (kvlist/array) — anyValueToString сама решает, когда его звать.
func anyValueToNative(v *commonpb.AnyValue) any {
	switch x := v.GetValue().(type) {
	case *commonpb.AnyValue_StringValue:
		return x.StringValue
	case *commonpb.AnyValue_BoolValue:
		return x.BoolValue
	case *commonpb.AnyValue_IntValue:
		return x.IntValue
	case *commonpb.AnyValue_DoubleValue:
		return x.DoubleValue
	case *commonpb.AnyValue_BytesValue:
		return x.BytesValue // json.Marshal кодирует []byte в base64-строку
	case *commonpb.AnyValue_ArrayValue:
		vals := x.ArrayValue.GetValues()
		out := make([]any, len(vals))
		for i, e := range vals {
			out[i] = anyValueToNative(e)
		}
		return out
	case *commonpb.AnyValue_KvlistValue:
		kvs := x.KvlistValue.GetValues()
		out := make(map[string]any, len(kvs))
		for _, kv := range kvs {
			out[kv.GetKey()] = anyValueToNative(kv.GetValue())
		}
		return out
	default:
		return nil
	}
}
