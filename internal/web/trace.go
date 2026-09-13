package web

import (
	"context"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/web/templates"
)

func (h *Handler) traceWaterfall(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	// trace_id в БД всегда канонично лоуркейснут — без нормализации значение
	// из URL в другом регистре даёт ложный 404 для существующего трейса.
	traceID := strings.ToLower(strings.TrimSpace(r.PathValue("trace_id")))
	if traceID == "" {
		h.notFound(w, r)
		return
	}

	if h.Trace == nil {
		h.notFound(w, r)
		return
	}

	origin, originID, originTransaction := traceOrigin(r)

	// found=false — 404 ниже; err — ClickHouse недоступен, деградация без 404.
	projectID, found, err := h.Trace.ProjectForTrace(r.Context(), traceID)
	if err != nil {
		h.renderTraceUnavailable(w, r, 0, traceID, origin, originID, originTransaction, err)
		return
	}
	if !found {
		h.notFound(w, r)
		return
	}

	canAccess, err := h.Org.CanAccessProject(r.Context(), uid, projectID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	if !canAccess {
		h.notFound(w, r)
		return
	}

	root, spans, err := h.Trace.Trace(r.Context(), projectID, traceID)
	if err != nil {
		h.renderTraceUnavailable(w, r, projectID, traceID, origin, originID, originTransaction, err)
		return
	}
	if len(spans) == 0 {
		// h.SpanRetentionDays (не trace.SpanRetentionDays — тот лишь дефолт
		// первой установки) — актуальный TTL; 0 значит «вечно».
		data := templates.TraceExpiredData{
			ProjectID:       projectID,
			TraceID:         traceID,
			RetentionDays:   h.SpanRetentionDays,
			Dropped:         h.spansLookLost(r.Context(), projectID, traceID),
			From:            origin,
			FromID:          originID,
			FromTransaction: originTransaction,
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusNotFound)
		_ = templates.TraceExpired(data, h.currentEmail(r)).Render(r.Context(), w)
		return
	}

	// h.Events может быть nil в стендах без него — тогда маркеров просто нет.
	errIssues := map[string]int64{}
	if h.Events != nil {
		errs, err := h.Events.ByTraceID(r.Context(), projectID, traceID)
		if err != nil {
			h.renderTraceUnavailable(w, r, projectID, traceID, origin, originID, originTransaction, err)
			return
		}
		for _, e := range errs {
			if e.SpanID != "" {
				errIssues[e.SpanID] = e.IssueID
			}
		}
	}

	// Конец последнего спана, не длительность корня: дочерний спан может
	// закончиться позже корня. Считаем в uint64 и насыщаем на UInt32.
	var maxEnd uint64
	for _, s := range spans {
		end := uint64(s.StartUS) + uint64(s.DurationUS)
		if end > maxEnd {
			maxEnd = end
		}
	}
	if maxEnd > math.MaxUint32 {
		maxEnd = math.MaxUint32
	}
	totalUS := uint32(maxEnd)

	transaction := traceID
	for _, s := range spans {
		if s.ParentSpanID == "" {
			transaction = s.Description
			break
		}
	}

	shown := len(spans)
	if shown > waterfallMaxRows {
		shown = waterfallMaxRows
	}

	// Best-effort: ошибка проверки профиля не роняет waterfall, просто прячет ссылку.
	hasProfile := false
	if h.Profiles != nil {
		if ok, err := h.Profiles.HasProfileForTrace(r.Context(), projectID, traceID); err == nil {
			hasProfile = ok
		}
	}

	data := templates.TraceWaterfallData{
		ProjectID:   projectID,
		TraceID:     traceID,
		Transaction: transaction,
		TotalUS:     totalUS,
		Timestamp:   root.Timestamp,
		Waterfall:   waterfallSVG(r.Context(), spans, errIssues, totalUS, waterfallWidth),
		ShownRows:   shown,
		TotalRows:   len(spans),
		HasProfile:  hasProfile,
	}
	data.From, data.FromID, data.FromTransaction = origin, originID, originTransaction
	_ = templates.TraceWaterfall(data, h.currentEmail(r)).Render(r.Context(), w)
}

// RetentionDays<=0 сюда не попадает (вечное хранение — body_purged). Иначе
// default true: без доказательства реального возраста нечестно винить срок хранения.
func (h *Handler) spansLookLost(ctx context.Context, projectID int64, traceID string) bool {
	if h.SpanRetentionDays <= 0 {
		return false
	}
	ts, found, err := h.Trace.TransactionTimestamp(ctx, projectID, traceID)
	if err != nil {
		slog.Warn("trace: transaction timestamp lookup failed, assuming buffer loss over expiry",
			"project_id", projectID, "trace_id", traceID, "err", err)
		return true
	}
	if !found {
		return true
	}
	cutoff := time.Now().Add(-time.Duration(h.SpanRetentionDays) * 24 * time.Hour)
	return !ts.Before(cutoff)
}

// 200, а не 500: единый приём для CH-страниц, 404 остаётся за «трейса нет».
func (h *Handler) renderTraceUnavailable(w http.ResponseWriter, r *http.Request, projectID int64, traceID, origin string, originID int64, originTransaction string, err error) {
	slog.Warn("trace: waterfall failed", "project_id", projectID, "trace_id", traceID, "err", err)
	data := templates.TraceExpiredData{
		ProjectID:       projectID,
		TraceID:         traceID,
		From:            origin,
		FromID:          originID,
		FromTransaction: originTransaction,
	}
	_ = templates.TraceUnavailable(data, h.currentEmail(r)).Render(r.Context(), w)
}

func (h *Handler) traceFlame(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	if h.Trace == nil || h.Profiles == nil {
		h.notFound(w, r)
		return
	}
	traceID := strings.ToLower(strings.TrimSpace(r.PathValue("trace_id")))
	if traceID == "" {
		h.notFound(w, r)
		return
	}
	// err (не found=false) — ClickHouse недоступен: оболочка на месте,
	// вместо флеймграфа «данные временно недоступны».
	projectID, found, err := h.Trace.ProjectForTrace(r.Context(), traceID)
	if err != nil {
		h.renderTraceFlameUnavailable(w, r, 0, traceID, err)
		return
	}
	if !found {
		h.notFound(w, r)
		return
	}
	canAccess, err := h.Org.CanAccessProject(r.Context(), uid, projectID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	if !canAccess {
		h.notFound(w, r)
		return
	}
	root, err := h.Profiles.FlameForTrace(r.Context(), projectID, traceID)
	if err != nil {
		h.renderTraceFlameUnavailable(w, r, projectID, traceID, err)
		return
	}
	data := templates.TraceFlameData{
		TraceID: traceID,
		Chart:   flamegraphSVG(r.Context(), root, r.URL.Query()["focus"], 960, flameLink(r)),
		HasData: flameHasData(root),
	}
	_ = templates.TraceFlame(data, h.currentEmail(r)).Render(r.Context(), w)
}

func (h *Handler) renderTraceFlameUnavailable(w http.ResponseWriter, r *http.Request, projectID int64, traceID string, err error) {
	slog.Warn("trace: flame failed", "project_id", projectID, "trace_id", traceID, "err", err)
	data := templates.TraceFlameData{TraceID: traceID, LoadFailed: true}
	_ = templates.TraceFlame(data, h.currentEmail(r)).Render(r.Context(), w)
}

// Неизвестный источник игнорируется — значение пришло из URL и не должно
// влиять на навигацию.
func traceOrigin(r *http.Request) (origin string, id int64, transaction string) {
	q := r.URL.Query()
	switch from := q.Get("from"); from {
	case "perf-issue", "issue":
		v, err := strconv.ParseInt(q.Get("from_id"), 10, 64)
		if err != nil || v <= 0 {
			return "", 0, ""
		}
		return from, v, ""
	case "endpoint":
		name := q.Get("from_id")
		if name == "" {
			return "", 0, ""
		}
		return from, 0, name
	default:
		return "", 0, ""
	}
}
