package web

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/incidentgroup"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
	"gitflic.ru/otezvikentiy/gotcha/internal/web/templates"
)

// Ниже, чем deploymentsListLimit — деплои тут вспомогательный контекст, не отдельный список.
const overviewDeployMarkersLimit = 20

// Всегда сутки, не переключается вместе с rangeKey — это разный вопрос от «недавно решённых».
const overviewNewIssuesWindow = 24 * time.Hour

// Источники — необязательные поля Handler, nil-safe: без подсистемы плитка/счётчик
// показывают «нет данных»/0, а не 404 или панику.
func (h *Handler) overviewStatusLine(ctx context.Context, projectID int64, rangeSince, rangeTo time.Time) (templates.StatusLine, error) {
	var sl templates.StatusLine

	if h.Uptime != nil && h.UptimeQuery != nil {
		monitors, err := h.Uptime.List(ctx, projectID)
		if err != nil {
			return templates.StatusLine{}, err
		}
		if len(monitors) > 0 {
			ids := make([]int64, len(monitors))
			for i, m := range monitors {
				ids[i] = m.ID
			}
			// Без исключения окон обслуживания — точность как у сырой колонки списка
			// мониторов, не страницы монитора: вычитать maintenance windows тут не по карману.
			batch, err := h.UptimeQuery.UptimeBatch(ctx, ids, rangeSince, rangeTo)
			if err != nil {
				// Отказ ClickHouse не роняет обзор: плитка честно говорит «недоступно».
				slog.Warn("web: overview uptime failed", "project_id", projectID, "error", err)
				sl.UptimeUnavailable = true
			}
			var sum uptime.UptimeStat
			for _, st := range batch {
				sum.Total += st.Total
				sum.OK += st.OK
			}
			sl.Uptime = sum
		}
	}

	if h.HostIncidents != nil {
		incidents, err := h.HostIncidents.ListOpenByProject(ctx, projectID)
		if err != nil {
			return templates.StatusLine{}, err
		}
		hosts := make(map[int64]bool, len(incidents))
		for _, in := range incidents {
			hosts[in.HostID] = true
		}
		sl.HostsOverThreshold = len(hosts)
	}

	if h.Issues != nil {
		n, err := h.Issues.CountNewSince(ctx, projectID, time.Now().Add(-overviewNewIssuesWindow))
		if err != nil {
			return templates.StatusLine{}, err
		}
		sl.NewIssues24h = n
	}

	return sl, nil
}

// h.Deploy == nil (стенд без подсистемы) — nil-safe, пустой срез, не 404.
func (h *Handler) overviewDeployMarkers(ctx context.Context, projectID int64, since, now time.Time) ([]templates.DeploymentRow, error) {
	if h.Deploy == nil {
		return nil, nil
	}
	deps, err := h.Deploy.List(ctx, projectID, since, now, overviewDeployMarkersLimit)
	if err != nil {
		return nil, err
	}
	rows := make([]templates.DeploymentRow, len(deps))
	for i, d := range deps {
		rows[i] = templates.DeploymentRow{
			Version:     d.Version,
			Environment: d.Environment,
			DeployedAt:  d.DeployedAt,
		}
	}
	return rows, nil
}

// Раздельные потолки для групп и внегрупповых пунктов: слитое число обещало бы
// не то, что делал код (50 групп + 50 внегрупповых = до 100 строк).
const (
	overviewClosedGroupsLimit     = 50
	overviewClosedOutOfGroupLimit = 50
)

var overviewRangeWindows = map[string]time.Duration{
	"24h": 24 * time.Hour,
	"7d":  7 * 24 * time.Hour,
}

func overviewRangeKey(r *http.Request) string {
	key := r.URL.Query().Get("range")
	if _, ok := overviewRangeWindows[key]; ok {
		return key
	}
	return "24h"
}

func overviewPath(projectID int64) string {
	return "/projects/" + strconv.FormatInt(projectID, 10) + "/overview"
}

// h.IncidentGroups == nil не отдаёт 404 — страница рендерится с пустыми выборками
// (пустой проект видит приглашение подключить SDK, не ошибку).
func (h *Handler) overview(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	projectID, ok := h.parsePathProjectID(w, r)
	if !ok {
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
	// Лента остаётся на lvlAccess; canOperate гейтит только ссылку на страницу
	// metric/slo-инцидента, не саму ленту.
	canOperate, err := h.canOperateProject(r.Context(), projectID, uid)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "")
		return
	}

	rangeKey := overviewRangeKey(r)
	now := time.Now()
	since := now.Add(-overviewRangeWindows[rangeKey])

	caps := templates.FeedCaps{
		OpenGroups:   incidentgroup.MaxOpenGroups,
		OutOfGroup:   incidentgroup.MaxOpenOutOfGroup,
		ClosedGroups: overviewClosedGroupsLimit,
		ClosedItems:  overviewClosedOutOfGroupLimit,
	}

	var openCards, closedCards []templates.GroupCard
	var outOfGroup, closed []incidentgroup.FeedItem
	if h.IncidentGroups != nil {
		open, err := h.IncidentGroups.OpenGroups(r.Context(), projectID)
		if err != nil {
			h.renderError(w, r, http.StatusInternalServerError, "")
			return
		}
		closedGroups, err := h.IncidentGroups.ClosedGroupsSince(r.Context(), projectID, since, overviewClosedGroupsLimit)
		if err != nil {
			h.renderError(w, r, http.StatusInternalServerError, "")
			return
		}

		// Один запрос на все карточки: иначе десятки round-trip по Composition на каждую группу.
		// len(open) читается ниже, сам срез open — нет, поэтому безопасно дописывать в его буфер.
		allGroups := append(open, closedGroups...)
		groupIDs := make([]int64, len(allGroups))
		for i, g := range allGroups {
			groupIDs[i] = g.ID
		}
		members, err := h.IncidentGroups.Compositions(r.Context(), projectID, groupIDs)
		if err != nil {
			h.renderError(w, r, http.StatusInternalServerError, "")
			return
		}
		groups := make([]templates.GroupCard, len(allGroups))
		for i, g := range allGroups {
			groups[i] = templates.NewGroupCard(g, members[g.ID])
		}
		openCards, closedCards = groups[:len(open)], groups[len(open):]

		outOfGroup, err = h.IncidentGroups.OpenOutOfGroup(r.Context(), projectID)
		if err != nil {
			h.renderError(w, r, http.StatusInternalServerError, "")
			return
		}
		closed, err = h.IncidentGroups.ClosedSince(r.Context(), projectID, since, overviewClosedOutOfGroupLimit)
		if err != nil {
			h.renderError(w, r, http.StatusInternalServerError, "")
			return
		}
	}

	statusLine, err := h.overviewStatusLine(r.Context(), projectID, since, now)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "")
		return
	}
	deploys, err := h.overviewDeployMarkers(r.Context(), projectID, since, now)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "")
		return
	}

	_ = templates.Overview(projectID, rangeKey, openCards, outOfGroup, closedCards, closed, caps, canOperate, statusLine, deploys, h.currentEmail(r)).Render(r.Context(), w)
}

// Доступ проверяется здесь же, а не оставлен на overview: parsePathProjectID не ходит
// в БД, и без этой проверки чужак получил бы неотличимый от чужого 301.
func (h *Handler) incidentFeedRedirect(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	projectID, ok := h.parsePathProjectID(w, r)
	if !ok {
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
	http.Redirect(w, r, overviewPath(projectID), http.StatusMovedPermanently)
}
