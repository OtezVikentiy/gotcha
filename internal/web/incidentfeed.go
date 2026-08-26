package web

import (
	"net/http"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/incidentgroup"
	"gitflic.ru/otezvikentiy/gotcha/internal/web/templates"
)

// feedClosedWindow — окно секции «недавно закрытые» (§6.1: сутки), общее
// для закрытых групп и закрытых внегрупповых инцидентов.
//
// feedClosedGroupsLimit/feedClosedOutOfGroupLimit — потолки ДВУХ независимых
// запросов этой секции (R4, W8). Раньше был один feedClosedLimit на оба —
// подпись у заголовка секции обещала «не больше 50», а фактический потолок
// экрана был 50 (групп) + 50 (внегрупповых) = до 100 строк: одно число на
// двоих обещало не то, что делал код. Разведены на два имени и две подписи
// (см. IncidentFeed/feed.closed.cap) — значения пока совпадают (50/50, тот
// же ориентир, что у пагинации /incidents, incidentsPerPage), но это два
// независимых крана, и совпадение значений не обязано сохраняться.
const (
	feedClosedWindow          = 24 * time.Hour
	feedClosedGroupsLimit     = 50
	feedClosedOutOfGroupLimit = 50
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
	// canOperate (W9): лента сама остаётся на lvlAccess — свести её к
	// lvlOperator означало бы прятать metric/slo-инциденты от рядового
	// участника проекта целиком, а весь смысл ленты (D3) — единая картина
	// по всем 6 источникам сразу; «Хост недоступен» и «Монитор недоступен»
	// рядовому участнику видны и сегодня (§6.1), урезание по источнику было
	// бы шагом назад именно там, где корреляция полезнее всего (шторм чаще
	// всего смешивает источники). Вместо этого — по образцу CanOperate у
	// HostDetailVM (hosts.go/hostdetail.templ): имя, severity и время
	// metric/slo-инцидента видны всем с доступом к проекту, а ссылка на его
	// родную страницу (/projects/{id}/metrics/alerts, /projects/{id}/slos —
	// обе lvlOperator) рисуется только оператору, чтобы не подсовывать
	// рядовому участнику ссылку, которую CanAccessProject != CanOperate
	// закроет 404. Сегодня canOperateProject предикатно совпадает с
	// CanAccessProject (operate.go, осознанно временно, до разъезда ролей
	// из спеки access-model-rework) — условие ничего не меняет прямо
	// сейчас, но перестаёт требовать доработки ленты в день, когда роли
	// разъедутся. См. также комментарий у lvlOperator-записей
	// metrics/alerts и slos в authz_map_test.go.
	canOperate, err := h.canOperateProject(r.Context(), projectID, uid)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}

	open, err := h.IncidentGroups.OpenGroups(r.Context(), projectID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	since := time.Now().Add(-feedClosedWindow)
	closedGroups, err := h.IncidentGroups.ClosedGroupsSince(r.Context(), projectID, since, feedClosedGroupsLimit)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}

	// Состав ВСЕХ карточек (открытых + закрытых) — одним запросом (W7):
	// раньше на каждую карточку уходил отдельный Composition (четырёхветвевой
	// UNION), десятки round-trip на одну отрисовку страницы, доступной
	// любому участнику проекта. Compositions группирует по group_id в Go.
	allGroups := append(open, closedGroups...) // len(open) читается ниже (openCards/closedCards), сам срез open — нет, поэтому безопасно дописывать в его буфер
	groupIDs := make([]int64, len(allGroups))
	for i, g := range allGroups {
		groupIDs[i] = g.ID
	}
	members, err := h.IncidentGroups.Compositions(r.Context(), projectID, groupIDs)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	groups := make([]templates.GroupCard, len(allGroups))
	for i, g := range allGroups {
		groups[i] = templates.NewGroupCard(g, members[g.ID])
	}
	openCards, closedCards := groups[:len(open)], groups[len(open):]

	outOfGroup, err := h.IncidentGroups.OpenOutOfGroup(r.Context(), projectID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	closed, err := h.IncidentGroups.ClosedSince(r.Context(), projectID, since, feedClosedOutOfGroupLimit)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}

	caps := templates.FeedCaps{
		OpenGroups:   incidentgroup.MaxOpenGroups,
		OutOfGroup:   incidentgroup.MaxOpenOutOfGroup,
		ClosedGroups: feedClosedGroupsLimit,
		ClosedItems:  feedClosedOutOfGroupLimit,
	}
	_ = templates.IncidentFeed(projectID, openCards, outOfGroup, closedCards, closed, caps, canOperate, h.currentEmail(r)).Render(r.Context(), w)
}
