package web

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/incidentgroup"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
	"gitflic.ru/otezvikentiy/gotcha/internal/web/templates"
)

// overviewDeployMarkersLimit — потолок числа деплоев, показываемых на
// шкале обзора (задача 7 nav-ia): деплои внутри окна — вспомогательный
// контекст для инцидентов, а не отдельный список (за полным списком — на
// /deployments), поэтому потолок ниже, чем у deploymentsListLimit.
const overviewDeployMarkersLimit = 20

// overviewNewIssuesWindow — окно «новых проблем» строки состояния: всегда
// сутки, НЕ переключается вместе с rangeKey «недавно решённых» ниже — это
// разные вопросы («сколько новых проблем завелось со вчера» вне зависимости
// от того, какое окно выбрано для просмотра резолвнутых).
const overviewNewIssuesWindow = 24 * time.Hour

// overviewStatusLine считает три числа строки состояния (§ шаг 3 брифа
// задачи 7): аптайм за то же окно, что и «недавно решённые» (rangeKey),
// хосты, у которых прямо сейчас открыт инцидент по порогу, и новые проблемы
// за последние сутки (фиксированное окно, см. overviewNewIssuesWindow). Все
// три источника — необязательные поля Handler (Uptime/UptimeQuery,
// HostIncidents, Issues) и nil-safe: стенд/инстанс без соответствующей
// подсистемы получает нулевой uptime.UptimeStat{} (плитка покажет «нет
// данных», тот же uptimeStatTextCtx, что и на странице монитора) либо
// честный 0 по хостам/проблемам — не 404/панику. Строка состояния должна
// быть на «Обзоре» по умолчанию (см. докблок overview ниже, тот же принцип,
// что и у IncidentGroups==nil).
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
			// UptimeBatch без исключения окон обслуживания — та же точность,
			// что у сырой колонки списка мониторов (monitors.templ), не
			// точность страницы монитора (та вычитает maintenance windows
			// отдельным запросом): строке состояния «Обзора» это не по
			// карману на каждый заход.
			batch, err := h.UptimeQuery.UptimeBatch(ctx, ids, rangeSince, rangeTo)
			if err != nil {
				return templates.StatusLine{}, err
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

// overviewDeployMarkers — деплои проекта в том же временном окне, что и
// секция «недавно решённые» (rangeKey/since — тот же параметр, что заводит
// её ниже по overview()): деплои ложатся на ту же временную ось, что и
// инциденты, чтобы вопрос «после выкатки или само» решался на одном экране
// (§ шаг 3 брифа задачи 7). h.Deploy == nil (стенд/инстанс без подсистемы
// C5) — nil-safe, пустой срез, тот же принцип, что и у IncidentGroups/Hosts
// выше: «Обзор» не 404-ит на отсутствующей опциональной фиче.
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

	statusLine, err := h.overviewStatusLine(r.Context(), projectID, since, now)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	deploys, err := h.overviewDeployMarkers(r.Context(), projectID, since, now)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}

	_ = templates.Overview(projectID, rangeKey, openCards, outOfGroup, closedCards, closed, caps, canOperate, statusLine, deploys, h.currentEmail(r)).Render(r.Context(), w)
}

// incidentFeedRedirect — GET /projects/{id}/incident-feed: старый адрес
// сводной ленты (D3, §6.1) теперь целиком редиректит на «Обзор» (задача 6
// nav-ia) — та же выборка данных, без урезания (см. докблок overview выше),
// плюс окно «недавно решённые» стало переключаемым. Редирект, а не
// alias-хендлер: внешние ссылки/закладки должны увидеть канонический адрес в
// адресной строке, а не отрендеренную под старым URL страницу.
//
// Доступ ПРОВЕРЯЕТСЯ здесь, той же CanAccessProject, что и у overview:
// «редирект просто ведёт на overview, а там та же проверка» неверно уже на
// первом прыжке — parsePathProjectID не ходит в БД (только парсит число из
// пути), поэтому чужак и вовсе несуществующий id получали бы неотличимый
// 301 (см. TestAuthzBehaviorStrangerRejectedOnScopedRoutes) — единый
// existence-oracle (h.notFound и для отсутствия, и для отказа), которого
// держится весь остальной сайт, был бы пробит.
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
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	if !canAccess {
		h.notFound(w, r)
		return
	}
	http.Redirect(w, r, overviewPath(projectID), http.StatusMovedPermanently)
}
