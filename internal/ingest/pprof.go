package ingest

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/profile"
)

func (h *Handler) pprofIngest(w http.ResponseWriter, r *http.Request) {
	key, ok := h.otlpAuthenticate(w, r, SignalProfile)
	if !ok {
		return
	}
	if h.Profiles == nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if h.rateLimited(w, key.OrgID, key.ProjectID, SignalProfile) {
		return
	}
	if h.overloaded(w, key.OrgID, key.ProjectID, SignalProfile, saturationOf(h.Profiles)) {
		return
	}
	if granted, _ := h.grant(r.Context(), h.ProfileQuota, key.OrgID, "profile", 1); granted == 0 {
		h.countDrop(r.Context(), dropProfile, key.OrgID, 1)
		h.writeQuotaExceeded(w, SignalProfile, "profile quota exceeded")
		return
	}
	body, closeBody, err := h.body(w, r)
	if err != nil {
		h.countRejected(RejectMalformed, SignalProfile)
		writeJSONError(w, http.StatusBadRequest, "bad body encoding")
		return
	}
	defer closeBody()
	raw, err := io.ReadAll(body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.Is(err, ErrTooLarge) || errors.As(err, &maxErr) {
			h.countRejected(RejectTooLarge, SignalProfile)
			writeJSONError(w, http.StatusRequestEntityTooLarge, "profile too large")
			return
		}
		h.countRejected(RejectMalformed, SignalProfile)
		writeJSONError(w, http.StatusBadRequest, "bad body")
		return
	}
	// pprof шлют gzip внутри тела без Content-Encoding — h.body его не разжал,
	// разжимаем сами с тем же лимитом, иначе ParsePprof разжал бы «бомбу» без предела.
	raw, err = gunzipLimited(raw, h.maxBytes*10)
	if err != nil {
		if errors.Is(err, ErrTooLarge) {
			h.countRejected(RejectTooLarge, SignalProfile)
			writeJSONError(w, http.StatusRequestEntityTooLarge, "profile too large")
			return
		}
		slog.Warn("ingest: bad pprof gzip", "error", err)
		h.countRejected(RejectMalformed, SignalProfile)
		writeJSONError(w, http.StatusBadRequest, "malformed pprof")
		return
	}
	q := r.URL.Query()
	prof, err := profile.ParsePprof(raw, q.Get("type"), time.Now().UTC())
	if err != nil {
		slog.Warn("ingest: bad pprof profile", "error", err)
		h.countRejected(RejectMalformed, SignalProfile)
		writeJSONError(w, http.StatusBadRequest, "malformed pprof")
		return
	}
	// метаданные из query недоверенные — каппим, иначе гигантский ?service=...
	// раздул бы колонки без ограничений.
	prof.Service = capRunes(q.Get("service"), 200)
	prof.Transaction = capRunes(q.Get("transaction"), 200)
	prof.Environment = capRunes(q.Get("environment"), 200)
	prof.TraceID = capRunes(q.Get("trace_id"), 200)
	h.scrubProfile(&prof)
	h.limitProfileCardinality(key.ProjectID, &prof)
	h.Profiles.Add(key.ProjectID, prof)
	w.WriteHeader(http.StatusAccepted)
}
