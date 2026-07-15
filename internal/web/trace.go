package web

import (
	"math"
	"net/http"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/web/templates"
)

// traceWaterfall — GET /traces/{trace_id}: waterfall трейса (дерево спанов с
// полосами по времени) плюс красные маркеры спанов с привязанными ошибками.
// Доступ — по проекту трейса: сначала резолвим trace_id → project_id
// (ProjectForTrace), затем CanAccessProject; неизвестный трейс и трейс чужого
// проекта дают одну и ту же 404 (не палим существование чужих trace_id, тот же
// принцип, что и у issue/monitor).
func (h *Handler) traceWaterfall(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	traceID := r.PathValue("trace_id")
	if traceID == "" {
		http.NotFound(w, r)
		return
	}

	projectID, found, err := h.Trace.ProjectForTrace(r.Context(), traceID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	if !found {
		http.NotFound(w, r)
		return
	}

	canAccess, err := h.Org.CanAccessProject(r.Context(), uid, projectID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	if !canAccess {
		http.NotFound(w, r)
		return
	}

	root, spans, err := h.Trace.Trace(r.Context(), projectID, traceID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	if len(spans) == 0 {
		// ProjectForTrace нашёл трейс в transactions, но спанов нет — трейс
		// без спанов рисовать нечем, 404 (тот же смысл «нет такой страницы»).
		http.NotFound(w, r)
		return
	}

	// Маркеры ошибок: события этого трейса (issue_id + span_id). Events может
	// быть nil в стендах, которым он не нужен, — тогда маркеров просто нет.
	errIssues := map[string]int64{}
	if h.Events != nil {
		errs, err := h.Events.ByTraceID(r.Context(), projectID, traceID)
		if err != nil {
			h.renderError(w, r, http.StatusInternalServerError, "internal error")
			return
		}
		for _, e := range errs {
			if e.SpanID != "" {
				errIssues[e.SpanID] = e.IssueID
			}
		}
	}

	// totalUS — правый край шкалы: максимальный конец спана (StartUS+Dur), а не
	// только длительность корня — дочерний спан теоретически может закончиться
	// позже корня, полоса не должна вылезти за viewBox. Считаем в uint64 и
	// насыщаем на UInt32.
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

	// Имя транзакции для заголовка — описание корневого спана (writer кладёт в
	// него имя транзакции). Нет корня — падаем на trace_id.
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

	// Profiling-in-context (этап 8): показываем ссылку на flamegraph, если для
	// этого трейса есть профиль. Best-effort — ошибка проверки не роняет
	// waterfall, просто прячет ссылку.
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
		Waterfall:   waterfallSVG(spans, errIssues, totalUS, waterfallWidth),
		ShownRows:   shown,
		TotalRows:   len(spans),
		HasProfile:  hasProfile,
	}
	_ = templates.TraceWaterfall(data, h.currentEmail(r)).Render(r.Context(), w)
}

// traceFlame — GET /traces/{trace_id}/flame: flamegraph профиля, снятого во
// время этого трейса (profiling-in-context, этап 8). Тот же контур доступа, что
// waterfall (ProjectForTrace → 404 чужим/неизвестным). Нет профиля → flamegraph
// с плейсхолдером «нет данных».
func (h *Handler) traceFlame(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	if h.Trace == nil || h.Profiles == nil {
		http.NotFound(w, r)
		return
	}
	traceID := r.PathValue("trace_id")
	if traceID == "" {
		http.NotFound(w, r)
		return
	}
	projectID, found, err := h.Trace.ProjectForTrace(r.Context(), traceID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	if !found {
		http.NotFound(w, r)
		return
	}
	canAccess, err := h.Org.CanAccessProject(r.Context(), uid, projectID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	if !canAccess {
		http.NotFound(w, r)
		return
	}
	root, err := h.Profiles.FlameForTrace(r.Context(), projectID, traceID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	data := templates.TraceFlameData{
		TraceID: traceID,
		Chart:   flamegraphSVG(root, 960),
	}
	_ = templates.TraceFlame(data, h.currentEmail(r)).Render(r.Context(), w)
}
