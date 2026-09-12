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

// h.Logs == nil → отвечаем успехом без записи (коллектор не ретраит вечно).
func (h *Handler) otlpLogs(w http.ResponseWriter, r *http.Request) {
	key, ok := h.otlpAuthenticate(w, r, SignalLog)
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
	if h.rateLimited(w, key.OrgID, key.ProjectID, SignalLog) {
		return
	}
	if h.overloaded(w, key.OrgID, key.ProjectID, SignalLog, saturationOf(h.Logs)) {
		return
	}
	body, closeBody, err := h.body(w, r)
	if err != nil {
		h.countRejected(RejectMalformed, SignalLog)
		writeJSONError(w, http.StatusBadRequest, "bad body encoding")
		return
	}
	defer closeBody()
	raw, err := io.ReadAll(body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.Is(err, ErrTooLarge) || errors.As(err, &maxErr) {
			h.countRejected(RejectTooLarge, SignalLog)
			writeJSONError(w, http.StatusRequestEntityTooLarge, "export too large")
			return
		}
		h.countRejected(RejectMalformed, SignalLog)
		writeJSONError(w, http.StatusBadRequest, "bad body")
		return
	}
	data, err := otlpUnmarshalLogs(enc, raw)
	if err != nil {
		h.countRejected(RejectMalformed, SignalLog)
		writeJSONError(w, http.StatusBadRequest, "malformed otlp payload")
		return
	}
	records := log.MapOTLPLogs(data.GetResourceLogs(), time.Now())
	granted := h.grantAndSanitizeLogs(r.Context(), key.OrgID, key.ProjectID, records)
	if granted == 0 && len(records) > 0 {
		h.writeQuotaExceeded(w, SignalLog, "log quota exceeded")
		return
	}
	writeOTLPResponse(w, enc)
}

// Вход для источников без OTLP-экспортёра; тот же поток, что у otlpLogs, но
// свой JSON-ответ вместо пустого OTLP-конверта.
func (h *Handler) logsNDJSON(w http.ResponseWriter, r *http.Request) {
	key, ok := h.otlpAuthenticate(w, r, SignalLog)
	if !ok {
		return
	}
	if h.Logs == nil {
		writeJSON(w, http.StatusOK, map[string]int{"accepted": 0})
		return
	}
	if h.rateLimited(w, key.OrgID, key.ProjectID, SignalLog) {
		return
	}
	if h.overloaded(w, key.OrgID, key.ProjectID, SignalLog, saturationOf(h.Logs)) {
		return
	}
	body, closeBody, err := h.body(w, r)
	if err != nil {
		h.countRejected(RejectMalformed, SignalLog)
		writeJSONError(w, http.StatusBadRequest, "bad body encoding")
		return
	}
	defer closeBody()
	// err != nil — чтение оборвалось; records в этом случае частичный
	// результат, отбрасывается целиком.
	records, err := log.ParseNDJSON(body, time.Now())
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.Is(err, ErrTooLarge) || errors.As(err, &maxErr) {
			h.countRejected(RejectTooLarge, SignalLog)
			writeJSONError(w, http.StatusRequestEntityTooLarge, "body too large")
			return
		}
		h.countRejected(RejectMalformed, SignalLog)
		writeJSONError(w, http.StatusBadRequest, "bad body")
		return
	}
	granted := h.grantAndSanitizeLogs(r.Context(), key.OrgID, key.ProjectID, records)
	if granted == 0 && len(records) > 0 {
		h.writeQuotaExceeded(w, SignalLog, "log quota exceeded")
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"accepted": granted})
}

func (h *Handler) grantAndSanitizeLogs(ctx context.Context, orgID, projectID int64, records []log.LogRecord) int {
	granted, _ := h.grant(ctx, h.LogQuota, orgID, "log", len(records))
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

// stripNUL — defense-in-depth: NUL в text-колонках роняет PostgreSQL,
// повторный вызов дёшев и гарантирует отсутствие NUL независимо от пути.
func sanitizeLog(h *Handler, projectID int64, r *log.LogRecord) {
	h.Scrub.ScrubTags(r.LogAttributes)
	h.Scrub.ScrubTags(r.ResourceAttrs)
	r.Body = stripNUL(r.Body)
	// Без ScrubMessage тело обходило бы скраб query-токенов/basic-auth,
	// применяемый к message событий и другим свободнотекстовым полям.
	r.Body = h.Scrub.ScrubMessage(r.Body)
	r.TraceID = stripNUL(r.TraceID)
	r.SpanID = stripNUL(r.SpanID)
	r.Service = h.Cardinality.Value(projectID, FieldService, r.Service)
	r.Environment = h.Cardinality.Value(projectID, FieldEnvironment, r.Environment)
}

// Отдельно от capRunes: та ещё и обрезает по длине, а капы здесь уже наложены парсером.
func stripNUL(s string) string {
	if strings.IndexByte(s, 0) < 0 {
		return s
	}
	return strings.ReplaceAll(s, "\x00", "")
}

// trace_id/span_id LogRecord закодированы в OTLP/JSON как HEX, не base64 —
// без otlpJSONHexIDs protojson декодирует hex как base64 и портит id.
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
