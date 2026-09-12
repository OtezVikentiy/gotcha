package log

import (
	"bufio"
	"encoding/json"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"
)

// map[string]any: типизированные значения ({"retry_count":3}) на map[string]string роняют json.Unmarshal
// целиком и теряют валидный message; коэрсия в строку — в capNDJSONAttrs.
type ndjsonLine struct {
	Message    string          `json:"message"`
	Level      string          `json:"level"`
	Timestamp  json.RawMessage `json:"timestamp"`
	Attributes map[string]any  `json:"attributes"`
	TraceID    string          `json:"trace_id"`
	SpanID     string          `json:"span_id"`
}

// Отдельно от maxBodyBytes: сравняй их, и усечение тела (capBytes) станет недостижимым — JSON не может
// декодироваться в message длиннее самой строки. Здесь — запас на обвязку (level/timestamp/trace_id/...).
const maxNDJSONLineBytes = maxBodyBytes * 4

// НЕ bufio.Scanner: на строке длиннее буфера тот теряет весь хвост батча (ErrTooLong). При I/O-ошибке
// (err != nil) частично собранный out ОБЯЗАН быть отброшен вызывающим — батч недопринят.
func ParseNDJSON(r io.Reader, now time.Time) ([]LogRecord, error) {
	br := bufio.NewReader(r)
	var out []LogRecord
	for len(out) < maxLogsPerRequest {
		line, err := br.ReadString('\n')
		if err != nil && err != io.EOF {
			return out, err
		}
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			// Строка длиннее maxNDJSONLineBytes — заведомый мусор или атака,
			// отбрасываем целиком без разбора, но продолжаем со следующей.
			if len(trimmed) <= maxNDJSONLineBytes {
				if rec, ok := parseNDJSONLine(trimmed, now); ok {
					out = append(out, rec)
				}
			}
		}
		if err == io.EOF {
			break
		}
	}
	return out, nil
}

func parseNDJSONLine(line string, now time.Time) (LogRecord, bool) {
	var raw ndjsonLine
	if err := json.Unmarshal([]byte(line), &raw); err != nil {
		return LogRecord{}, false
	}
	if raw.Message == "" {
		return LogRecord{}, false
	}

	return LogRecord{
		// ns==0 (нет метки/битый формат/дата до эпохи) — тот же сигнал «нет времени», что в OTLP; logTime
		// сама подставляет fallback и клампит окном ретенции [now-90d, now+24h].
		Timestamp:  logTime(parseNDJSONTimestampNs(raw.Timestamp), now),
		ObservedTS: now,

		Severity:       CanonFromText(raw.Level),
		SeverityNumber: 0, // у NDJSON нет числового кода уровня, в отличие от OTLP
		SeverityText:   capRunes(raw.Level, 64),

		Body: capBytes(raw.Message, maxBodyBytes),

		// 64 — щедрый запас: валидные trace_id(32 hex)/span_id(16 hex) проходят нетронутыми, патологически
		// длинные от недоверенного клиента — режутся (в OTLP их размер уже ограничен байтами протокола).
		TraceID: capRunes(raw.TraceID, 64),
		SpanID:  capRunes(raw.SpanID, 64),

		LogAttributes: capNDJSONAttrs(raw.Attributes),
	}, true
}

// timestamp — RFC3339-строка либо unix-секунды (возможно дробные) в наносекунды с эпохи, формат logTime.
// Отсутствие/null/пустая строка/нераспознанный формат/дата не позже эпохи — 0 («нет надёжного времени»).
func parseNDJSONTimestampNs(raw json.RawMessage) uint64 {
	if len(raw) == 0 {
		return 0
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if s == "" {
			return 0
		}
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return 0
		}
		return nsSinceEpoch(t)
	}
	var sec float64
	if err := json.Unmarshal(raw, &sec); err == nil {
		if sec <= 0 {
			return 0
		}
		whole := int64(sec)
		frac := int64((sec - float64(whole)) * float64(time.Second))
		return nsSinceEpoch(time.Unix(whole, frac))
	}
	return 0
}

// t.UnixNano() с явным «не позже эпохи → 0» — иначе уйдёт в отрицательные при приведении к uint64
// (logTime трактует такой знак как отсутствие метки).
func nsSinceEpoch(t time.Time) uint64 {
	ns := t.UnixNano()
	if ns <= 0 {
		return 0
	}
	return uint64(ns)
}

// Те же капы, что attrsToMap (ключ 64/значение 200/maxAttrKeys, по отсортированным ключам при переполнении),
// но для map[string]any: nil-значение (JSON null) — ключ пропускается, а не становится строкой "null".
func capNDJSONAttrs(attrs map[string]any) map[string]string {
	if len(attrs) == 0 {
		return nil
	}
	m := make(map[string]string, len(attrs))
	for k, v := range attrs {
		if k == "" || v == nil {
			continue
		}
		m[capRunes(k, 64)] = capRunes(ndjsonAttrString(v), 200)
	}
	if len(m) == 0 {
		return nil
	}
	if len(m) <= maxAttrKeys {
		return m
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	capped := make(map[string]string, maxAttrKeys)
	for _, k := range keys[:maxAttrKeys] {
		capped[k] = m[k]
	}
	return capped
}

// Скаляры — как attrString (float64 без разделения int/float, формат "3", не "3.0"); объект/массив —
// JSON-строкой, как anyValueToString для тела OTLP-лога с kvlist/array.
func ndjsonAttrString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case bool:
		if x {
			return "true"
		}
		return "false"
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	default:
		b, err := json.Marshal(x)
		if err != nil {
			return ""
		}
		return string(b)
	}
}
