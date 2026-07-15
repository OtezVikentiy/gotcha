package ingest

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/profile"
)

// pprofIngest — POST /profiles/pprof: приём pprof-профиля (gzip-protobuf) с
// Bearer-DSN аутентификацией и метаданными из query (service/transaction/
// environment/type). Профили выключены (h.Profiles==nil) → 202 без записи.
func (h *Handler) pprofIngest(w http.ResponseWriter, r *http.Request) {
	key, ok := h.otlpAuthenticate(w, r)
	if !ok {
		return
	}
	if h.Profiles == nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if !h.allow(r.Context(), h.ProfileQuota, key.OrgID, "profile") {
		writeQuotaExceeded(w, "profile quota exceeded")
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
			writeJSONError(w, http.StatusRequestEntityTooLarge, "profile too large")
			return
		}
		writeJSONError(w, http.StatusBadRequest, "bad body")
		return
	}
	q := r.URL.Query()
	prof, err := profile.ParsePprof(raw, q.Get("type"), time.Now().UTC())
	if err != nil {
		slog.Warn("ingest: bad pprof profile", "error", err)
		writeJSONError(w, http.StatusBadRequest, "malformed pprof")
		return
	}
	prof.Service = q.Get("service")
	prof.Transaction = q.Get("transaction")
	prof.Environment = q.Get("environment")
	prof.TraceID = q.Get("trace_id")
	h.Profiles.Add(key.ProjectID, prof)
	w.WriteHeader(http.StatusAccepted)
}
