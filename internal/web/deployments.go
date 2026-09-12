package web

import (
	"net/http"
	"net/url"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/web/templates"
)

const deploymentsListLimit = 100

// Пускаем ссылкой только http/https; иначе ok=false — шаблон покажет URL текстом.
// templ.URL тут не годится: для плохой схемы он всё равно даст <a href="about:invalid">.
func safeExternalHref(raw string) (string, bool) {
	if raw == "" {
		return "", false
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", false
	}
	switch u.Scheme {
	case "http", "https":
		return raw, true
	}
	return "", false
}

func (h *Handler) deployments(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	projectID, ok := h.parsePathProjectID(w, r)
	if !ok {
		return
	}
	if h.Deploy == nil {
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

	deps, err := h.Deploy.Recent(r.Context(), projectID, deploymentsListLimit)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}

	rows := make([]templates.DeploymentRow, 0, len(deps))
	for _, d := range deps {
		href, isLink := safeExternalHref(d.URL)
		rows = append(rows, templates.DeploymentRow{
			Version:     d.Version,
			Environment: d.Environment,
			URL:         d.URL,
			LinkURL:     href,
			IsLink:      isLink,
			Changelog:   d.Changelog,
			DeployedAt:  d.DeployedAt,
		})
	}

	_ = templates.DeploymentsScreen(projectID, rows, h.currentEmail(r)).Render(r.Context(), w)
}
