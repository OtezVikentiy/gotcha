package web

import (
	"log/slog"
	"net/http"
	"net/url"
	"strconv"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/web/templates"
)

func profilesPath(projectID int64) string {
	return "/projects/" + strconv.FormatInt(projectID, 10) + "/profiles"
}

func (h *Handler) profilesList(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	projectID, ok := h.parsePathProjectID(w, r)
	if !ok {
		return
	}
	if h.Profiles == nil {
		h.notFound(w, r)
		return
	}
	canAccess, err := h.Org.CanAccessProject(r.Context(), uid, projectID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "")
		return
	}
	if !canAccess {
		h.notFound(w, r)
		return
	}
	tr := h.resolveTimeRange(w, r, "24h")
	environment := r.URL.Query().Get("environment")
	services, err := h.Profiles.ListServices(r.Context(), projectID, environment, tr.From, tr.To)
	// Отказ ClickHouse — не 500: список рендерится с loadFailed вместо страницы ошибки.
	loadFailed := err != nil
	if loadFailed {
		slog.Warn("profiles: list failed", "project_id", projectID, "err", err)
		services = nil
	}
	_ = templates.ProfilesList(projectID, services, timeRangeVM(tr), environment, h.currentEmail(r), loadFailed).Render(r.Context(), w)
}

func (h *Handler) profileFlame(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	projectID, ok := h.parsePathProjectID(w, r)
	if !ok {
		return
	}
	if h.Profiles == nil {
		h.notFound(w, r)
		return
	}
	canAccess, err := h.Org.CanAccessProject(r.Context(), uid, projectID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "")
		return
	}
	if !canAccess {
		h.notFound(w, r)
		return
	}
	q := r.URL.Query()
	tr := h.resolveTimeRange(w, r, "24h")
	service := q.Get("service")
	profileType := q.Get("type")
	environment := q.Get("environment")
	transaction := q.Get("transaction")
	root, err := h.Profiles.Flame(r.Context(), projectID, service, environment, profileType, transaction, tr.From, tr.To)
	loadFailed := err != nil
	if loadFailed {
		slog.Warn("profiles: flame failed", "project_id", projectID, "service", service, "err", err)
		root = nil
	}
	vm := templates.ProfileFlameVM{
		ProjectID:   projectID,
		Service:     service,
		Type:        profileType,
		Transaction: transaction,
		Environment: environment,
		Range:       timeRangeVM(tr),
		Chart:       flamegraphSVG(r.Context(), root, q["focus"], 960, flameLink(r)),
		HasData:     flameHasData(root),
		LoadFailed:  loadFailed,
	}
	_ = templates.ProfileFlame(vm, h.currentEmail(r)).Render(r.Context(), w)
}

func flameLink(r *http.Request) func(path []string) string {
	// EscapedPath, а не Path: trace_id со спецсимволами (?, #, %) в сыром виде
	// сломал бы ссылку — браузер разобрал бы его как начало query/фрагмента.
	base := r.URL.EscapedPath()
	q := r.URL.Query()
	q.Del("focus")
	return func(path []string) string {
		v := make(url.Values, len(q)+1)
		for k, vals := range q {
			v[k] = vals
		}
		if len(path) > 0 {
			v["focus"] = path
		}
		enc := v.Encode()
		if enc == "" {
			return base
		}
		return base + "?" + enc
	}
}
