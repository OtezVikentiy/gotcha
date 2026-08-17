package ingest

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"gitflic.ru/otezvikentiy/gotcha/internal/log"
)

// otlpLogs — приём OTLP-логов (C1). Каркас копирует otlpMetrics (Bearer-DSN
// auth, квота, лимит тела, proto+JSON): логи, как и метрики, не семплируются и
// не зависят от флага трейсинга. Логи выключены (h.Logs == nil) → отвечаем
// успехом без записи (коллектор не ретраит вечно).
func (h *Handler) otlpLogs(w http.ResponseWriter, r *http.Request) {
	key, ok := h.otlpAuthenticate(w, r)
	if !ok {
		return
	}
	enc, ok := otlpEncodingOf(r.Header.Get("Content-Type"))
	if !ok {
		writeJSONError(w, http.StatusUnsupportedMediaType, "unsupported content-type")
		return
	}
	if h.Logs == nil {
		writeOTLPResponse(w, enc)
		return
	}
	if h.rateLimited(w, key.OrgID, key.ProjectID) {
		return
	}
	body, closeBody, err := h.body(w, r)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "bad body encoding")
		return
	}
	defer closeBody()
	raw, err := io.ReadAll(body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.Is(err, ErrTooLarge) || errors.As(err, &maxErr) {
			writeJSONError(w, http.StatusRequestEntityTooLarge, "export too large")
			return
		}
		writeJSONError(w, http.StatusBadRequest, "bad body")
		return
	}
	data, err := otlpUnmarshalLogs(enc, raw)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "malformed otlp payload")
		return
	}
	records := log.MapOTLPLogs(data.GetResourceLogs(), time.Now())
	granted := h.grantAndSanitizeLogs(r.Context(), key.OrgID, key.ProjectID, records)
	if granted == 0 && len(records) > 0 {
		writeQuotaExceeded(w, "log quota exceeded")
		return
	}
	writeOTLPResponse(w, enc)
}

// logsNDJSON — приём логов построчным newline-delimited JSON: вход для
// источников без OTLP-экспортёра (произвольный скрипт/агент, слог через curl).
// Поток тот же, что у otlpLogs (auth → квота → лимит тела → санитизация), но
// без согласования кодировки (NDJSON — единственный формат тела) и с
// собственным JSON-ответом ({"accepted":N}), а не пустым OTLP-конвертом.
func (h *Handler) logsNDJSON(w http.ResponseWriter, r *http.Request) {
	key, ok := h.otlpAuthenticate(w, r)
	if !ok {
		return
	}
	if h.Logs == nil {
		writeJSON(w, http.StatusOK, map[string]int{"accepted": 0})
		return
	}
	if h.rateLimited(w, key.OrgID, key.ProjectID) {
		return
	}
	body, closeBody, err := h.body(w, r)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "bad body encoding")
		return
	}
	defer closeBody()
	// Контракт ParseNDJSON: err != nil означает, что чтение тела оборвалось
	// (I/O-ошибка, не битая строка), и records в этом случае — ЧАСТИЧНЫЙ
	// результат, который обязан быть отброшен целиком (см. её докблок) —
	// батч из оборванного тела недопринят, а не принят частично.
	records, err := log.ParseNDJSON(body, time.Now())
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.Is(err, ErrTooLarge) || errors.As(err, &maxErr) {
			writeJSONError(w, http.StatusRequestEntityTooLarge, "body too large")
			return
		}
		writeJSONError(w, http.StatusBadRequest, "bad body")
		return
	}
	granted := h.grantAndSanitizeLogs(r.Context(), key.OrgID, key.ProjectID, records)
	if granted == 0 && len(records) > 0 {
		writeQuotaExceeded(w, "log quota exceeded")
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"accepted": granted})
}

// grantAndSanitizeLogs списывает квоту логов за records (по элементу, как у
// метрик — см. h.grant), отброшенное по квоте считает в дропы, а оставшееся
// санитизирует и кладёт в LogSink. Общий хвост otlpLogs и logsNDJSON — разбор
// тела и формат ответа у них разный, а дальше поток идентичен otlpMetrics.
func (h *Handler) grantAndSanitizeLogs(ctx context.Context, orgID, projectID int64, records []log.LogRecord) int {
	granted := h.grant(ctx, h.LogQuota, orgID, "log", len(records))
	if dropped := len(records) - granted; dropped > 0 {
		h.countDrop(ctx, dropLog, orgID, dropped)
		slog.Warn("ingest: log quota exceeded, dropping records",
			"dropped", dropped, "accepted", granted, "project_id", projectID, "org_id", orgID)
	}
	for i := range records[:granted] {
		sanitizeLog(h, projectID, &records[i])
		h.Logs.Add(projectID, records[i])
	}
	return granted
}

// sanitizeLog чистит запись лога перед записью — та же граница, что у
// otlpMetrics (otlp.go): капы длины и число атрибутов УЖЕ наложены в парсерах
// (log.MapOTLPLogs/log.ParseNDJSON), здесь их не дублируем.
//
// stripNUL — на TraceID/SpanID/Body: у OTLP-пути это уже чистые значения
// (hex/capBytes в парсере), а у NDJSON trace_id/span_id приходят от клиента
// БЕЗ капа парсера (в отличие от Body, который parseNDJSONLine уже прогнал
// через capBytes) — NUL в них ничем не запрещён, а PostgreSQL падает на нём в
// text-колонках при дальнейшей склейке с трейсами (тот же класс дефекта, что
// у capRunes в sentry.go).
func sanitizeLog(h *Handler, projectID int64, r *log.LogRecord) {
	h.Scrub.ScrubTags(r.LogAttributes)
	h.Scrub.ScrubTags(r.ResourceAttrs)
	r.Body = stripNUL(r.Body)
	// Тело лога — единственное свободнотекстовое поле пайплайна логов, и без
	// ScrubMessage оно обходило бы безусловный скраб query-токенов/basic-auth
	// из URL, который уже применяется к message событий, имени транзакции и
	// описанию спанов (pipeline.go, handler.go) — паритет приватности.
	r.Body = h.Scrub.ScrubMessage(r.Body)
	r.TraceID = stripNUL(r.TraceID)
	r.SpanID = stripNUL(r.SpanID)
	r.Service = h.Cardinality.Value(projectID, FieldService, r.Service)
	r.Environment = h.Cardinality.Value(projectID, FieldEnvironment, r.Environment)
}

// stripNUL вырезает байты NUL (0x00) из s. Отдельная функция от capRunes
// (sentry.go): та ещё и обрезает по длине, а капы длины здесь уже наложены
// парсером — повторное применение сдвинуло бы границу капа не туда.
func stripNUL(s string) string {
	if strings.IndexByte(s, 0) < 0 {
		return s
	}
	return strings.ReplaceAll(s, "\x00", "")
}

// otlpUnmarshalLogs — разбор тела /v1/logs, калька otlpUnmarshal (трейсы): у
// логов, как и у трейсов, есть верхнеуровневые байтовые идентификаторы
// (trace_id/span_id LogRecord), закодированные в OTLP/JSON как HEX, а не
// base64 — без otlpJSONHexIDs protojson молча декодирует hex как base64 и
// портит id (см. её докблок в otlp.go). Раньше здесь ошибочно считалось, что
// у логов таких идентификаторов нет.
func otlpUnmarshalLogs(enc otlpEncoding, raw []byte) (*logspb.LogsData, error) {
	var data logspb.LogsData
	var err error
	if enc == otlpJSON {
		raw, err = otlpJSONHexIDs(raw)
		if err != nil {
			return nil, err // errJSONTooDeep — единственная ошибка, otlpJSONHexIDs больше не отдаёт
		}
		err = protojson.UnmarshalOptions{DiscardUnknown: true}.Unmarshal(raw, &data)
	} else {
		err = proto.Unmarshal(raw, &data)
	}
	if err != nil {
		return nil, err
	}
	return &data, nil
}
