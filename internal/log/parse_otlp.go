package log

import (
	"encoding/hex"
	"encoding/json"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
)

// maxLogsPerRequest — потолок числа записей, разбираемых из одного OTLP
// /v1/logs-запроса (тот же приём, что maxOTLPMetricPoints/maxOTLPSpans):
// защита от амплификации памяти/CPU недоверенным экспортом.
const maxLogsPerRequest = 10000

// maxBodyBytes — потолок тела лога, 64 КиБ. Реальные сообщения приложений
// столько не весят; без капа одна запись раздула бы CH-колонку body и буфер
// писателя (см. log.Writer.logRowBytes, который явно учитывает Body).
const maxBodyBytes = 64 << 10

// MapOTLPLogs разворачивает OTLP ResourceLogs в плоские LogRecord'ы, готовые к
// записи. fallback — серверное время приёма, идёт и в ObservedTS (всегда), и
// как запасной timestamp при TimeUnixNano==0.
func MapOTLPLogs(rl []*logspb.ResourceLogs, fallback time.Time) []LogRecord {
	var out []LogRecord
	for _, r := range rl {
		service, environment := promote(r.GetResource())
		resourceAttrs := attrsToMap(r.GetResource().GetAttributes())
		for _, sl := range r.GetScopeLogs() {
			for _, lr := range sl.GetLogRecords() {
				// Кап проверяем ВНУТРИ вложенного цикла (как maxOTLPMetricPoints в
				// metric/parse.go): один ResourceLogs с гигантским ScopeLogs иначе
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
		// timestamp — event-time с клампом к окну ретенции; observed_ts — ВСЕГДА
		// серверное fallback-время приёма, ObservedTimeUnixNano не читаем (иначе
		// теряется защита от кривых часов клиента, спека C1 §1.1).
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

// capSeverityNumber приводит сырое OTLP SeverityNumber (int32, недоверенный
// ввод) к uint8-колонке (см. схему logs: severity_number UInt8, 0 = не
// задано). Валидный диапазон спецификации — 1..24; всё вне uint8 (в т.ч.
// отрицательное) хранится как 0, а не оборачивается через int32→uint8 —
// иначе, скажем, 300 молча стало бы 44 и врало бы в аудите/отладке.
func capSeverityNumber(n int32) uint8 {
	if n < 0 || n > 255 {
		return 0
	}
	return uint8(n)
}

// anyValueToString — текстовое представление OTLP AnyValue для тела лога:
// строка как есть; скаляры — их обычное строковое представление; структурные
// значения (kvlist/array) — JSON-строка (в отличие от attrString в
// sanitize.go, который для лейблов структуры не разворачивает).
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

// anyValueToNative переводит AnyValue в обычные Go-значения (map/slice/
// строка/число/bool), пригодные для json.Marshal. Нужен только для
// структурного тела лога (kvlist/array) — anyValueToString сама решает, когда
// его звать.
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
