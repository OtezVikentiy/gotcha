package ingest

import (
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/klauspost/compress/zstd"

	"gitflic.ru/otezvikentiy/gotcha/internal/org"
)

// Handler — HTTP-слой Sentry-протокола.
type Handler struct {
	keys     *KeyCache
	quota    QuotaChecker
	pipeline *Pipeline
	maxBytes int64
}

func NewHandler(keys *KeyCache, quota QuotaChecker, pipeline *Pipeline, maxEventBytes int64) *Handler {
	return &Handler{keys: keys, quota: quota, pipeline: pipeline, maxBytes: maxEventBytes}
}

func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/{project}/envelope/{$}", h.envelope)
	mux.HandleFunc("POST /api/{project}/store/{$}", h.store)
}

// authenticate проверяет ключ проекта и учитывает запрос в месячной квоте
// организации; при успехе возвращает разрешённый ключ и true. При отказе
// сама пишет ошибку в w и возвращает false.
func (h *Handler) authenticate(w http.ResponseWriter, r *http.Request) (org.Key, bool) {
	projectID, err := strconv.ParseInt(r.PathValue("project"), 10, 64)
	if err != nil {
		writeJSONError(w, http.StatusNotFound, "unknown project")
		return org.Key{}, false
	}
	pub := PublicKeyFromRequest(r)
	if pub == "" {
		writeJSONError(w, http.StatusUnauthorized, "missing sentry_key")
		return org.Key{}, false
	}
	key, err := h.keys.Resolve(r.Context(), pub)
	switch {
	case errors.Is(err, org.ErrNotFound):
		writeJSONError(w, http.StatusForbidden, "invalid sentry_key")
		return org.Key{}, false
	case err != nil:
		writeJSONError(w, http.StatusServiceUnavailable, "key lookup failed")
		return org.Key{}, false
	case key.ProjectID != projectID:
		writeJSONError(w, http.StatusForbidden, "sentry_key does not match project")
		return org.Key{}, false
	}

	if h.quota != nil {
		allowed, err := h.quota.CheckAndCount(r.Context(), key.OrgID)
		if err != nil {
			// Fail-open: терять события из-за сбоя счётчика квот хуже, чем
			// иногда пропустить организацию сверх квоты.
			slog.Warn("ingest: quota check failed, allowing event", "org_id", key.OrgID, "error", err)
		} else if !allowed {
			writeQuotaExceeded(w)
			return org.Key{}, false
		}
	}
	return key, true
}

// writeQuotaExceeded пишет 429 с Retry-After — числом секунд до 1-го числа
// следующего месяца UTC, когда счётчик организации обнулится.
func writeQuotaExceeded(w http.ResponseWriter) {
	w.Header().Set("Retry-After", strconv.FormatInt(secondsUntilNextMonth(time.Now().UTC()), 10))
	writeJSONError(w, http.StatusTooManyRequests, "event quota exceeded")
}

func secondsUntilNextMonth(now time.Time) int64 {
	now = now.UTC()
	y, m, _ := now.Date()
	next := time.Date(y, m+1, 1, 0, 0, 0, 0, time.UTC)
	secs := int64(next.Sub(now).Seconds())
	if secs < 1 {
		secs = 1
	}
	return secs
}

// noopClose — заглушка для тела без компрессии: закрывать нечего.
func noopClose() {}

// body возвращает reader тела с учётом лимитов и Content-Encoding, и функцию
// закрытия декомпрессора (нужно звать defer'ом у вызывающего: zstd.Decoder
// держит фоновую горутину, gzip.Reader — что-то из sync.Pool у большинства
// реализаций, оба реализуют io.Closer, который раньше терялся в io.LimitReader).
func (h *Handler) body(w http.ResponseWriter, r *http.Request) (io.Reader, func(), error) {
	raw := http.MaxBytesReader(w, r.Body, h.maxBytes)
	switch r.Header.Get("Content-Encoding") {
	case "gzip":
		zr, err := gzip.NewReader(raw)
		if err != nil {
			return nil, noopClose, err
		}
		return newLimitedReader(zr, h.maxBytes*10), func() { _ = zr.Close() }, nil
	case "zstd":
		zr, err := zstd.NewReader(raw)
		if err != nil {
			return nil, noopClose, err
		}
		return newLimitedReader(zr.IOReadCloser(), h.maxBytes*10), zr.Close, nil
	default:
		return raw, noopClose, nil
	}
}

// limitedReader отдаёт ErrTooLarge, если из потока прочитано больше limit
// байт — в отличие от io.LimitReader, который тихо обрезает поток до limit
// и возвращает io.EOF, маскируя bomb-подобное переполнение под успешный
// (но усечённый) результат.
type limitedReader struct {
	r    io.Reader
	left int64 // limit+1: чтение (limit+1)-го байта = превышение
}

func newLimitedReader(r io.Reader, limit int64) *limitedReader {
	return &limitedReader{r: r, left: limit + 1}
}

func (l *limitedReader) Read(p []byte) (int, error) {
	if l.left <= 0 {
		return 0, ErrTooLarge
	}
	if int64(len(p)) > l.left {
		p = p[:l.left]
	}
	n, err := l.r.Read(p)
	l.left -= int64(n)
	if l.left <= 0 {
		return n, ErrTooLarge
	}
	return n, err
}

func (h *Handler) envelope(w http.ResponseWriter, r *http.Request) {
	key, ok := h.authenticate(w, r)
	if !ok {
		return
	}
	projectID := key.ProjectID
	body, closeBody, err := h.body(w, r)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "bad body encoding")
		return
	}
	defer closeBody()
	env, err := ParseEnvelope(body, h.maxBytes)
	if err != nil {
		status := http.StatusBadRequest
		var maxErr *http.MaxBytesError
		if errors.Is(err, ErrTooLarge) || errors.As(err, &maxErr) {
			status = http.StatusRequestEntityTooLarge
		}
		writeJSONError(w, status, "malformed envelope")
		return
	}

	id := env.EventID
	for _, raw := range env.Events {
		pe, err := ParseEvent(raw)
		if err != nil {
			continue // битый item не валит весь envelope
		}
		if id == "" {
			id = pe.EventID
		}
		h.pipeline.Enqueue(projectID, pe)
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": id})
}

func (h *Handler) store(w http.ResponseWriter, r *http.Request) {
	key, ok := h.authenticate(w, r)
	if !ok {
		return
	}
	projectID := key.ProjectID
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
			writeJSONError(w, http.StatusRequestEntityTooLarge, "event too large")
			return
		}
		writeJSONError(w, http.StatusBadRequest, "bad body")
		return
	}
	pe, err := ParseEvent(raw)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "malformed event")
		return
	}
	h.pipeline.Enqueue(projectID, pe)
	writeJSON(w, http.StatusOK, map[string]string{"id": pe.EventID})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSONError(w http.ResponseWriter, status int, detail string) {
	writeJSON(w, status, map[string]string{"detail": detail})
}
