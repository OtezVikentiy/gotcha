package ingest

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/trace"
)

var ErrNoTraceContext = errors.New("ingest: transaction has no contexts.trace")

var ErrTimestampOutOfWindow = errors.New("ingest: transaction timestamp is outside the retention window")

// Имя транзакции попадает в ORDER BY CH-таблицы, op и status — в LowCardinality.
const (
	maxTransactionName = 200
	maxSpanDescription = 2000
	maxOp              = 100
	maxStatus          = 50
	// trace_id — 32 hex-символа, span_id — 16; каппим по длине, не валидируем строго.
	maxTraceID = 32
	maxSpanID  = 16
	maxSpans   = 1000
	// Выбор при переполнении детерминирован — первые maxMeasurements в отсортированном порядке.
	maxMeasurements   = 40
	maxMeasurementKey = 100
)

type sentryTraceContext struct {
	TraceID string `json:"trace_id"`
	SpanID  string `json:"span_id"`
	Op      string `json:"op"`
	Status  string `json:"status"`
}

type sentrySpan struct {
	SpanID       string          `json:"span_id"`
	ParentSpanID string          `json:"parent_span_id"`
	Op           string          `json:"op"`
	Description  string          `json:"description"`
	Start        json.RawMessage `json:"start_timestamp"`
	End          json.RawMessage `json:"timestamp"`
	Status       string          `json:"status"`
	Data         map[string]any  `json:"data"`
}

// Unit нужен, чтобы привести секунды к миллисекундам; у CLS unit пустой.
type sentryMeasurement struct {
	Value float64 `json:"value"`
	Unit  string  `json:"unit"`
}

type sentryTransaction struct {
	Transaction string          `json:"transaction"`
	Start       json.RawMessage `json:"start_timestamp"`
	End         json.RawMessage `json:"timestamp"`
	Contexts    struct {
		Trace *sentryTraceContext `json:"trace"`
	} `json:"contexts"`
	Spans       []sentrySpan `json:"spans"`
	Environment string       `json:"environment"`
	Release     string       `json:"release"`
	ServerName  string       `json:"server_name"`
	User        *struct {
		ID string `json:"id"`
	} `json:"user"`
	Tags         json.RawMessage              `json:"tags"`
	Measurements map[string]sentryMeasurement `json:"measurements"`
}

// Timestamp вне окна хранения (см. timestamp.go) роняет ВСЮ транзакцию
// (ErrTimestampOutOfWindow), а не клампится, как у событий: сдвиг сломал бы длительности.
func ParseTransaction(raw []byte) (trace.Transaction, error) {
	var st sentryTransaction
	if err := json.Unmarshal(raw, &st); err != nil {
		return trace.Transaction{}, fmt.Errorf("ingest: transaction json: %w", err)
	}
	if st.Contexts.Trace == nil {
		return trace.Transaction{}, ErrNoTraceContext
	}
	tc := st.Contexts.Trace
	traceID := normalizeID(tc.TraceID, maxTraceID)
	if traceID == "" {
		return trace.Transaction{}, ErrNoTraceContext
	}

	// Начало без конца → нулевая длительность, а не отрицательная.
	now := time.Now().UTC()
	end, ok := parseTraceTime(st.End)
	if !ok {
		end = now
	}
	start, ok := parseTraceTime(st.Start)
	if !ok {
		start = end
	}
	// В CH timestamp транзакции — это start, по нему и партиционирование.
	if !inRetentionWindow(start, now) {
		return trace.Transaction{}, ErrTimestampOutOfWindow
	}

	tags := map[string]string{}
	parseTags(st.Tags, tags)

	tx := trace.Transaction{
		TraceID:      traceID,
		SpanID:       normalizeID(tc.SpanID, maxSpanID),
		Name:         capRunes(st.Transaction, maxTransactionName),
		Op:           capRunes(tc.Op, maxOp),
		Status:       transactionStatus(tc.Status),
		Start:        start,
		End:          end,
		Environment:  capRunes(st.Environment, 200),
		Release:      capRunes(st.Release, 200),
		ServerName:   capRunes(st.ServerName, 200),
		Tags:         capTags(tags),
		Source:       "sentry",
		Measurements: parseMeasurements(st.Measurements),
	}
	if st.User != nil {
		tx.UserID = capRunes(st.User.ID, 200)
	}

	spans := st.Spans
	if len(spans) > maxSpans {
		spans = spans[:maxSpans]
	}
	tx.Spans = make([]trace.Span, 0, len(spans))
	for _, ss := range spans {
		sEnd, ok := parseTraceTime(ss.End)
		if !ok {
			sEnd = end
		}
		sStart, ok := parseTraceTime(ss.Start)
		if !ok {
			sStart = sEnd
		}
		if !inRetentionWindow(sStart, now) {
			continue // спан-«отравитель» отбрасывается молча, транзакция остаётся
		}
		tx.Spans = append(tx.Spans, trace.Span{
			SpanID:       normalizeID(ss.SpanID, maxSpanID),
			ParentSpanID: normalizeID(ss.ParentSpanID, maxSpanID),
			Op:           capRunes(ss.Op, maxOp),
			// байтовый кап, не рунный: Description идёт в NormalizeSQL, чья
			// цена зависит от длины в байтах (см. capBytes).
			Description: capBytes(ss.Description, maxSpanDescription),
			Start:       sStart,
			End:         sEnd,
			Status:      transactionStatus(ss.Status),
			// Тот же ограничитель числа ключей/длины, что у OTLP-пути (capDataMap/otlpAttrMap).
			Data: capDataMap(ss.Data),
		})
	}
	return tx, nil
}

// Не-конечные (NaN/Inf) и отрицательные значения отбрасываются, а не зануляются.
func parseMeasurements(raw map[string]sentryMeasurement) map[string]float64 {
	if len(raw) == 0 {
		return nil
	}
	out := make(map[string]float64, len(raw))
	for k, m := range raw {
		v := m.Value
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			continue
		}
		if m.Unit == "second" {
			v *= 1000 // ms-vitals хранятся в миллисекундах
		}
		out[capRunes(k, maxMeasurementKey)] = v
	}
	if len(out) == 0 {
		return nil
	}
	return capMeasurements(out)
}

func capMeasurements(m map[string]float64) map[string]float64 {
	if len(m) <= maxMeasurements {
		return m
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make(map[string]float64, maxMeasurements)
	for _, k := range keys[:maxMeasurements] {
		out[k] = m[k]
	}
	return out
}

// MV transactions_5m считает провалом всё, что != 'ok' — пустая строка
// раздула бы failure rate до 100%, поэтому дефолт "ok".
func transactionStatus(status string) string {
	if status == "" {
		return "ok"
	}
	return capRunes(status, maxStatus)
}

// В отличие от parseTimestamp (события), не подставляет time.Now() — вызывающий
// сам решает замену, чтобы длительность не считалась от «сейчас».
func parseTraceTime(raw json.RawMessage) (time.Time, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return time.Time{}, false
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err == nil && f > 0 {
		sec := int64(f)
		// Округление до микросекунды: float64 хранит unix-секунды с точностью ~0.5
		// мкс, без округления 500 мс между timestamp'ами дали бы duration_us=499999.
		return time.Unix(sec, int64((f-float64(sec))*1e9)).UTC().Round(time.Microsecond), true
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if ts, err := time.Parse(time.RFC3339Nano, s); err == nil {
			return ts.UTC(), true
		}
	}
	return time.Time{}, false
}
