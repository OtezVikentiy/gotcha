package web

import (
	"net/http"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/web/templates"
)

// feedClosedWindow/feedClosedLimit — окно и потолок секции «недавно
// закрытые» (§6.1: сутки, LIMIT 50 с подписью; пагинации в v1 нет —
// открытых инцидентов единицы-десятки).
const (
	feedClosedWindow = 24 * time.Hour
	feedClosedLimit  = 50
)

// incidentFeed — GET /projects/{id}/incident-feed: сводная лента (D3, §6.1):
// открытые группы с раскрываемым составом, внегрупповые открытые инциденты
// 6 источников, недавно закрытые. Доступ — CanAccessProject (lvlAccess),
// тот же принцип, что incidentsList.
func (h *Handler) incidentFeed(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	projectID, ok := h.parsePathProjectID(w, r)
	if !ok {
		return
	}
	if h.IncidentGroups == nil {
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

	open, err := h.IncidentGroups.OpenGroups(r.Context(), projectID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	since := time.Now().Add(-feedClosedWindow)
	closedGroups, err := h.IncidentGroups.ClosedGroupsSince(r.Context(), projectID, since, feedClosedLimit)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	groups := make([]templates.GroupCard, 0, len(open)+len(closedGroups))
	for _, g := range append(open, closedGroups...) {
		members, err := h.IncidentGroups.Composition(r.Context(), g.ID)
		if err != nil {
			h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
			return
		}
		groups = append(groups, templates.NewGroupCard(g, members))
	}
	openCards, closedCards := groups[:len(open)], groups[len(open):]

	outOfGroup, err := h.IncidentGroups.OpenOutOfGroup(r.Context(), projectID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	closed, err := h.IncidentGroups.ClosedSince(r.Context(), projectID, since, feedClosedLimit)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}

	_ = templates.IncidentFeed(projectID, openCards, outOfGroup, closedCards, closed, h.currentEmail(r)).Render(r.Context(), w)
}
