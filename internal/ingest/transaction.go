package ingest

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/trace"
)

// ErrNoTraceContext — в transaction-payload нет contexts.trace или пуст
// trace_id: связать такую транзакцию не с чем, писать её в CH — мусор.
var ErrNoTraceContext = errors.New("ingest: transaction has no contexts.trace")

// ErrTimestampOutOfWindow — timestamp транзакции вне окна хранения
// [now-90d, now+1d] (см. timestamp.go): такую строку ClickHouse всё равно
// выбросит по TTL, а пачка таких строк с timestamp'ами из разных месяцев
// намертво заклинивает запись (Too many partitions for single INSERT block).
var ErrTimestampOutOfWindow = errors.New("ingest: transaction timestamp is outside the retention window")

// Лимиты недоверенных строк транзакции (та же дисциплина, что у событий, см.
// capRunes): имя транзакции попадает в ORDER BY CH-таблицы, описание спана —
// в самую широкую колонку, op — в LowCardinality.
const (
	maxTransactionName = 200
	maxSpanDescription = 2000
	maxOp              = 100
	// maxStatus — статусы Sentry (ok, internal_error, deadline_exceeded, ...)
	// короткие; колонка LowCardinality, мусор в ней дорог.
	maxStatus = 50
	// maxTraceID/maxSpanID — trace_id это 32 hex-символа, span_id — 16;
	// каппим по этим же длинам, а не валидируем строго: SDK бывают вольны с
	// форматом, но раздувать колонки им нельзя.
	maxTraceID = 32
	maxSpanID  = 16
	// maxSpans — верхняя граница числа спанов в одной транзакции: защита от
	// раздутого payload'а (лишние спаны отбрасываются, транзакция остаётся).
	maxSpans = 1000
)

// sentryTraceContext — contexts.trace транзакции: корневой спан трейса.
type sentryTraceContext struct {
	TraceID string `json:"trace_id"`
	SpanID  string `json:"span_id"`
	Op      string `json:"op"`
	Status  string `json:"status"`
}

// sentrySpan — элемент spans[] transaction-payload'а.
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

// sentryTransaction — transaction-item Sentry-envelope'а.
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
	Tags json.RawMessage `json:"tags"`
}

// ParseTransaction разбирает Sentry transaction-payload в trace.Transaction.
// Терпим к вариациям SDK: timestamps приходят и unix-числом, и RFC3339-строкой
// (sentry-php/sentry-python шлют по-разному). Отсутствие contexts.trace или
// пустой trace_id → ErrNoTraceContext.
//
// trace_id/span_id/parent_span_id нормализуются к нижнему регистру: регистр hex
// выбирает тот, кто его кодирует (в OTLP trace id едет 16 СЫРЫМИ байтами), а
// хранить и семплировать (см. trace.Keep) один и тот же трейс надо одинаково,
// как бы его ни написал SDK.
//
// Timestamp'ы вне окна хранения (см. timestamp.go) ОТБРАСЫВАЮТСЯ: транзакция
// целиком → ErrTimestampOutOfWindow, отдельный спан — молча выкидывается из
// tx.Spans. Не теряем ничего настоящего: такие строки ClickHouse всё равно
// снесёт по TTL, зато пачка «месяц назад, два, три...» больше не может
// заклинить запись всего инстанса.
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

	// Конец транзакции известен всегда (SDK его шлёт); если нет — считаем, что
	// она закончилась сейчас. Начало без конца → нулевая длительность, а не
	// отрицательная (см. trace.Transaction.DurationUS).
	now := time.Now().UTC()
	end, ok := parseTraceTime(st.End)
	if !ok {
		end = now
	}
	start, ok := parseTraceTime(st.Start)
	if !ok {
		start = end
	}
	// В CH timestamp транзакции — это start (см. trace.SpanWriter.Add), по нему
	// и партиционирование, его и проверяем.
	if !inRetentionWindow(start, now) {
		return trace.Transaction{}, ErrTimestampOutOfWindow
	}

	tags := map[string]string{}
	parseTags(st.Tags, tags)

	tx := trace.Transaction{
		TraceID:     traceID,
		SpanID:      normalizeID(tc.SpanID, maxSpanID),
		Name:        capRunes(st.Transaction, maxTransactionName),
		Op:          capRunes(tc.Op, maxOp),
		Status:      transactionStatus(tc.Status),
		Start:       start,
		End:         end,
		Environment: capRunes(st.Environment, 200),
		Release:     capRunes(st.Release, 200),
		ServerName:  capRunes(st.ServerName, 200),
		Tags:        capTags(tags),
		Source:      "sentry",
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
			continue // спан-«отравитель»: см. ErrTimestampOutOfWindow
		}
		tx.Spans = append(tx.Spans, trace.Span{
			SpanID:       normalizeID(ss.SpanID, maxSpanID),
			ParentSpanID: normalizeID(ss.ParentSpanID, maxSpanID),
			Op:           capRunes(ss.Op, maxOp),
			Description:  capRunes(ss.Description, maxSpanDescription),
			Start:        sStart,
			End:          sEnd,
			Status:       transactionStatus(ss.Status),
			Data:         ss.Data,
		})
	}
	return tx, nil
}

// transactionStatus нормализует статус: SDK его часто опускают у успешных
// транзакций, а MV transactions_5m считает провалом всё, что != 'ok', — пустая
// строка иначе раздула бы failure rate до 100%.
func transactionStatus(status string) string {
	if status == "" {
		return "ok"
	}
	return capRunes(status, maxStatus)
}

// parseTraceTime разбирает timestamp транзакции/спана: unix-число (в т.ч.
// дробное) ИЛИ RFC3339-строка. В отличие от parseTimestamp (события), не
// подставляет time.Now() — вызывающий сам решает, чем заменить отсутствующее
// значение, чтобы длительность не считалась от «сейчас».
func parseTraceTime(raw json.RawMessage) (time.Time, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return time.Time{}, false
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err == nil && f > 0 {
		sec := int64(f)
		// Округление до микросекунды: float64 хранит unix-секунды с точностью
		// ~0.5 мкс, и без округления 500 мс между двумя timestamp'ами дали бы
		// duration_us = 499999. Больше микросекунды нам всё равно не нужно —
		// колонка duration_us в микросекундах.
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
