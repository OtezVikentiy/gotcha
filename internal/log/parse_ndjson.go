package log

import (
	"bufio"
	"encoding/json"
	"io"
	"sort"
	"strings"
	"time"
)

// ndjsonLine — минимальная схема одной строки NDJSON-лога. Источник вдвойне
// недоверенный по сравнению с OTLP: сам формат необязательно валиден
// построчно, поэтому распознаём только то, что нужно, остального не
// заявляем — лишние поля json.Unmarshal молча игнорирует.
type ndjsonLine struct {
	Message    string            `json:"message"`
	Level      string            `json:"level"`
	Timestamp  json.RawMessage   `json:"timestamp"`
	Attributes map[string]string `json:"attributes"`
	TraceID    string            `json:"trace_id"`
	SpanID     string            `json:"span_id"`
}

// maxNDJSONLineBytes — потолок сырой строки, после которого она отбрасывается
// БЕЗ попытки json.Unmarshal. Это отдельная величина от maxBodyBytes: JSON
// не может декодироваться в строку длиннее самого себя (экранирование и
// служебные символы только увеличивают байтовый размер), поэтому строка
// ровно на maxBodyBytes уже физически не может нести message длиннее
// maxBodyBytes — приравняй порог отбрасывания к maxBodyBytes, и capBytes
// (усечение тела с маркером) стало бы недостижимым кодом. Порог здесь —
// щедрый запас на JSON-обвязку (level/timestamp/attributes/trace_id/span_id)
// вокруг тела около границы капа, а не сам кап; отсечение по-настоящему
// патологических строк (на порядки больше тела) остаётся его задачей —
// экономит json.Unmarshal на заведомо мусорном/атакующем вводе.
const maxNDJSONLineBytes = maxBodyBytes * 4

// ParseNDJSON разбирает тело запроса построчно (newline-delimited JSON) в
// LogRecord. Битая строка (не-JSON, пустой message) пропускается и не роняет
// весь батч — калька семантики MapOTLPLogs, где один кривой LogRecord не
// портит остальные.
//
// Намеренно НЕ bufio.Scanner: на строке длиннее внутреннего буфера Scanner
// возвращает bufio.ErrTooLong и после этого больше не сканирует — теряется
// весь хвост батча. bufio.Reader.ReadString сам растит буфер до \n
// независимо от длины строки, так что после отбрасывания одной гигантской
// строки чтение следующих продолжается штатно.
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

// parseNDJSONLine разбирает одну непустую строку. ok=false — строка не
// распознана (не JSON или пустой message), вызывающий код её пропускает.
func parseNDJSONLine(line string, now time.Time) (LogRecord, bool) {
	var raw ndjsonLine
	if err := json.Unmarshal([]byte(line), &raw); err != nil {
		return LogRecord{}, false
	}
	if raw.Message == "" {
		return LogRecord{}, false
	}

	return LogRecord{
		// ns==0 (нет метки, битый формат, дата до эпохи) — тот же сигнал
		// «нет времени», что и в OTLP-пути, logTime сама даёт fallback и
		// клампит окном ретенции [now-90d, now+24h].
		Timestamp:  logTime(parseNDJSONTimestampNs(raw.Timestamp), now),
		ObservedTS: now,

		Severity:       CanonFromText(raw.Level),
		SeverityNumber: 0, // у NDJSON нет числового кода уровня, в отличие от OTLP
		SeverityText:   capRunes(raw.Level, 64),

		Body: capBytes(raw.Message, maxBodyBytes),

		TraceID: raw.TraceID,
		SpanID:  raw.SpanID,

		LogAttributes: capNDJSONAttrs(raw.Attributes),
	}, true
}

// parseNDJSONTimestampNs переводит поле timestamp (RFC3339-строка либо
// unix-время в секундах, возможно дробное) в наносекунды с эпохи — тот же
// формат, что ожидает logTime. Отсутствие поля, null, пустая строка,
// нераспознанный формат или дата не позже эпохи — 0, что для logTime
// эквивалентно «нет надёжного времени».
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

// nsSinceEpoch — t.UnixNano(), но с явным «не позже эпохи → 0», чтобы не
// уйти в отрицательные значения при приведении к uint64 (logTime трактует
// такой знак как отсутствие метки).
func nsSinceEpoch(t time.Time) uint64 {
	ns := t.UnixNano()
	if ns <= 0 {
		return 0
	}
	return uint64(ns)
}

// capNDJSONAttrs — те же капы, что attrsToMap в sanitize.go (ключ 64/значение
// 200/maxAttrKeys, детерминированно по отсортированным ключам при
// переполнении), но для уже готовой map[string]string: NDJSON присылает
// attributes сразу объектом, а не OTLP-шным []*commonpb.KeyValue, под который
// заточена attrsToMap, — переиспользовать её сигнатуру не выйдет, capRunes и
// maxAttrKeys переиспользуются как есть.
func capNDJSONAttrs(attrs map[string]string) map[string]string {
	if len(attrs) == 0 {
		return nil
	}
	m := make(map[string]string, len(attrs))
	for k, v := range attrs {
		if k == "" {
			continue
		}
		m[capRunes(k, 64)] = capRunes(v, 200)
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
