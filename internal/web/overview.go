package web

import (
	"net/http"
	"strconv"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/incidentgroup"
	"gitflic.ru/otezvikentiy/gotcha/internal/web/templates"
)

// overviewClosedGroupsLimit/overviewClosedOutOfGroupLimit — потолки ДВУХ
// независимых запросов секции «недавно решённые» (перенесены с прежней
// /incident-feed, R4/W7/W8 без изменений: подпись у заголовка секции
// обещала «не больше 50», и фактический потолок экрана был 50 (групп) + 50
// (внегрупповых) = до 100 строк — одно число на двоих обещало не то, что
// делал код; разведены на два имени и две подписи).
const (
	overviewClosedGroupsLimit     = 50
	overviewClosedOutOfGroupLimit = 50
)

// overviewRangeWindows — пресеты окна секции «недавно решённые»: 24 часа —
// тот же горизонт, что был жёстко зашит на прежней /incident-feed, 7 дней —
// расширение на случай, когда шторм не улёгся за сутки (см. докблок
// overview ниже, инвентаризация §7 спеки).
var overviewRangeWindows = map[string]time.Duration{
	"24h": 24 * time.Hour,
	"7d":  7 * 24 * time.Hour,
}

// overviewRangeKey разбирает query-параметр ?range= в один из двух
// известных пресетов, откатываясь на "24h" (умолчание страницы, тот же
// горизонт, что был у прежней несменяемой ленты) при пустом или
// нераспознанном значении.
func overviewRangeKey(r *http.Request) string {
	key := r.URL.Query().Get("range")
	if _, ok := overviewRangeWindows[key]; ok {
		return key
	}
	return "24h"
}

// overviewPath — путь до «Обзора» проекта: цель редиректа со старого адреса
// /incident-feed (incidentFeedRedirect ниже) и запасной выход index()/
// ProjectSwitchHref (см. web.go, nav.ProjectSwitchHref) для запомненного
// проекта.
func overviewPath(projectID int64) string {
	return "/projects/" + strconv.FormatInt(projectID, 10) + "/overview"
}

// overview — GET /projects/{id}/overview (задача 6 nav-ia): первый экран
// проекта, шкала инцидентов; index() и /incident-feed теперь ведут сюда.
//
// Инвентаризация прежней /incident-feed перед вёрсткой (§7 спеки,
// обязательный пункт): у неё не было ни фильтров, ни пагинации — три секции
// с фиксированными потолками (открытые группы/внегрупповые —
// incidentgroup.MaxOpenGroups/MaxOpenOutOfGroup, закрытые —
// overviewClosed*Limit) и жёстко зашитым суточным окном «недавно решённые».
// Решение: всё содержимое переносится на «Обзор» БЕЗ урезания — открытые
// секции остаются без временного окна (они открыты сейчас, обрезать
// нечего), у «недавно решённых» окно становится выбираемым (rangeKey —
// "24h"/"7d", 24ч по умолчанию — тот же горизонт, что был раньше). Отдельной
// страницы «вся лента» не остаётся: /incident-feed целиком редиректит на
// «Обзор» (incidentFeedRedirect), а не на урезанную версию — экран не беднее
// прежнего ни при каком выборе диапазона.
//
// h.IncidentGroups == nil (стенд/инстанс без подсистемы D3) НЕ отдаёт 404,
// в отличие от прежней /incident-feed: «Обзор» — теперь дверь по умолчанию
// (index() ведёт сюда), и 404 на голом входе в приложение читался бы как
// поломка, а не как отсутствующая опциональная фича. Вместо этого страница
// рендерится с пустыми выборками — совсем пустой проект получает
// приглашение подключить SDK (см. templates.Overview), а не ошибку.
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
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	if !canAccess {
		h.notFound(w, r)
		return
	}
	// canOperate (W9, перенесено без изменений): лента сама остаётся на
	// lvlAccess — свести её к lvlOperator означало бы прятать metric/slo-
	// инциденты от рядового участника проекта целиком. canOperate гейтит
	// только ссылку на родную страницу metric/slo-инцидента (см.
	// templates.feedItemLinkable).
	canOperate, err := h.canOperateProject(r.Context(), projectID, uid)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}

	rangeKey := overviewRangeKey(r)
	since := time.Now().Add(-overviewRangeWindows[rangeKey])

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
			h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
			return
		}
		closedGroups, err := h.IncidentGroups.ClosedGroupsSince(r.Context(), projectID, since, overviewClosedGroupsLimit)
		if err != nil {
			h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
			return
		}

		// Состав ВСЕХ карточек (открытых + закрытых) — одним запросом (W7):
		// см. докблок Compositions в incidentgroup — десятки round-trip на
		// одну отрисовку страницы, доступной любому участнику проекта, иначе
		// уходили на Composition по группе.
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
		openCards, closedCards = groups[:len(open)], groups[len(open):]

		outOfGroup, err = h.IncidentGroups.OpenOutOfGroup(r.Context(), projectID)
		if err != nil {
			h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
			return
		}
		closed, err = h.IncidentGroups.ClosedSince(r.Context(), projectID, since, overviewClosedOutOfGroupLimit)
		if err != nil {
			h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
			return
		}
	}

	_ = templates.Overview(projectID, rangeKey, openCards, outOfGroup, closedCards, closed, caps, canOperate, h.currentEmail(r)).Render(r.Context(), w)
}

// incidentFeedRedirect — GET /projects/{id}/incident-feed: старый адрес
// сводной ленты (D3, §6.1) теперь целиком редиректит на «Обзор» (задача 6
// nav-ia) — та же выборка данных, без урезания (см. докблок overview выше),
// плюс окно «недавно решённые» стало переключаемым. Редирект, а не
// alias-хендлер: внешние ссылки/закладки должны увидеть канонический адрес в
// адресной строке, а не отрендеренную под старым URL страницу. Доступ не
// проверяется здесь отдельно — overview делает ту же проверку
// (CanAccessProject) на целевом адресе; проверка на самом редиректе была бы
// задвоением, различимым только по тому, куда в итоге ведёт браузер (302 на
// /login либо честный 404 overview — тот же результат, что и раньше).
func (h *Handler) incidentFeedRedirect(w http.ResponseWriter, r *http.Request) {
	projectID, ok := h.parsePathProjectID(w, r)
	if !ok {
		return
	}
	http.Redirect(w, r, overviewPath(projectID), http.StatusMovedPermanently)
}
