package ingest

import (
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/klauspost/compress/zstd"

	"gitflic.ru/otezvikentiy/gotcha/internal/org"
)

// Handler — HTTP-слой Sentry-протокола.
type Handler struct {
	keys     *KeyCache
	pipeline *Pipeline
	maxBytes int64
}

func NewHandler(keys *KeyCache, pipeline *Pipeline, maxEventBytes int64) *Handler {
	return &Handler{keys: keys, pipeline: pipeline, maxBytes: maxEventBytes}
}

func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/{project}/envelope/{$}", h.envelope)
	mux.HandleFunc("POST /api/{project}/store/{$}", h.store)
}

// authenticate возвращает id проекта или пишет ошибку в w и возвращает -1.
func (h *Handler) authenticate(w http.ResponseWriter, r *http.Request) int64 {
	projectID, err := strconv.ParseInt(r.PathValue("project"), 10, 64)
	if err != nil {
		writeJSONError(w, http.StatusNotFound, "unknown project")
		return -1
	}
	pub := PublicKeyFromRequest(r)
	if pub == "" {
		writeJSONError(w, http.StatusUnauthorized, "missing sentry_key")
		return -1
	}
	key, err := h.keys.Resolve(r.Context(), pub)
	switch {
	case errors.Is(err, org.ErrNotFound):
		writeJSONError(w, http.StatusForbidden, "invalid sentry_key")
		return -1
	case err != nil:
		writeJSONError(w, http.StatusServiceUnavailable, "key lookup failed")
		return -1
	case key.ProjectID != projectID:
		writeJSONError(w, http.StatusForbidden, "sentry_key does not match project")
		return -1
	}
	return projectID
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
	projectID := h.authenticate(w, r)
	if projectID < 0 {
		return
	}
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
	projectID := h.authenticate(w, r)
	if projectID < 0 {
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
