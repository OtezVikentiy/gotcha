package web

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/depsuppress"
	"gitflic.ru/otezvikentiy/gotcha/internal/host"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
	"gitflic.ru/otezvikentiy/gotcha/internal/web/templates"
)

// alertSuppressionPath — адрес редактора рёбер зависимостей (B5, задача 9).
func alertSuppressionPath(projectID int64) string {
	return templates.AlertSuppressionPath(projectID)
}

// alertSuppressionErrorMessage переводит доменные ошибки depsuppress.Store.Create
// в человекочитаемое сообщение для 422-страницы — тот же приём, что и
// escalationsErrorMessage/alertsErrorMessage.
func alertSuppressionErrorMessage(ctx context.Context, err error) string {
	switch {
	case errors.Is(err, depsuppress.ErrForeignNode):
		return i18n.T(ctx, "err.alert_suppression.foreign_node")
	case errors.Is(err, depsuppress.ErrSelfLoop):
		return i18n.T(ctx, "err.alert_suppression.self_loop")
	case errors.Is(err, depsuppress.ErrSelfMatch):
		return i18n.T(ctx, "err.alert_suppression.self_match")
	case errors.Is(err, depsuppress.ErrDuplicate):
		return i18n.T(ctx, "err.alert_suppression.duplicate")
	case errors.Is(err, depsuppress.ErrCycle):
		return i18n.T(ctx, "err.alert_suppression.cycle")
	case errors.Is(err, depsuppress.ErrInvalidEdge):
		return i18n.T(ctx, "err.alert_suppression.invalid")
	default:
		return i18n.T(ctx, "error.action_failed")
	}
}

// alertSuppressionPage — GET /projects/{id}/alert-suppression: список рёбер
// зависимостей проекта + форма добавления. Доступ — оператор проекта
// (requireProjectOperator), как escalations/slos/metric-alerts.
func (h *Handler) alertSuppressionPage(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	projectID, ok := h.parsePathProjectID(w, r)
	if !ok {
		return
	}
	// h.AlertDeps может быть nil в узких тестовых стендах — тот же nil-guard,
	// что у escalationsPage/slosPage, а не паника при разыменовании.
	if h.AlertDeps == nil {
		h.notFound(w, r)
		return
	}
	if _, ok := h.requireProjectOperator(w, r, projectID, uid); !ok {
		return
	}
	h.renderAlertSuppression(w, r, http.StatusOK, projectID, "")
}

// renderAlertSuppression — общий рендер: GET и POST-обработчик на 422 (тот
// же принцип, что renderEscalations/renderAlerts). Список рёбер и узлы для
// селектов формы читаются заново на каждый рендер — введённое в форме при
// ошибке не перерисовывается буквально (тот же упрощённый принцип, что и у
// alertsChannelCreate: форма создания короткая, повторный ввод не в тягость).
func (h *Handler) renderAlertSuppression(w http.ResponseWriter, r *http.Request, status int, projectID int64, errMsg string) {
	edges, err := h.AlertDeps.List(r.Context(), projectID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}

	hosts, monitors, err := h.suppressionNodes(r.Context(), projectID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}

	hostNames := make(map[int64]string, len(hosts))
	hostOptions := make([]templates.SuppressionNodeOption, len(hosts))
	for i, hh := range hosts {
		hostNames[hh.ID] = hh.Name
		hostOptions[i] = templates.SuppressionNodeOption{ID: hh.ID, Name: hh.Name}
	}
	monitorNames := make(map[int64]string, len(monitors))
	monitorOptions := make([]templates.SuppressionNodeOption, len(monitors))
	for i, m := range monitors {
		monitorNames[m.ID] = m.Name
		monitorOptions[i] = templates.SuppressionNodeOption{ID: m.ID, Name: m.Name}
	}

	rows := make([]templates.SuppressionEdgeView, len(edges))
	for i, e := range edges {
		rows[i] = templates.SuppressionEdgeView{
			ID:          e.ID,
			ParentLabel: suppressionParentLabel(r.Context(), e, hostNames, monitorNames),
			ChildLabel:  suppressionChildLabel(r.Context(), e, hostNames, monitorNames),
		}
	}

	w.WriteHeader(status)
	_ = templates.AlertSuppression(projectID, rows, hostOptions, monitorOptions, errMsg, h.currentEmail(r)).Render(r.Context(), w)
}

// suppressionNodes читает хосты и мониторы проекта для селектов формы и
// резолва имён в списке. nil-safe: h.Hosts/h.Uptime могут не быть проведены
// в узких тестовых стендах (main.go в режимах "web"/"all" заводит их всегда
// вместе) — тогда соответствующий список остаётся пустым, а не паникует.
func (h *Handler) suppressionNodes(ctx context.Context, projectID int64) ([]host.Host, []uptime.Monitor, error) {
	var hosts []host.Host
	if h.Hosts != nil {
		var err error
		hosts, err = h.Hosts.List(ctx, projectID, 0)
		if err != nil {
			return nil, nil, err
		}
	}
	var monitors []uptime.Monitor
	if h.Uptime != nil {
		var err error
		monitors, err = h.Uptime.List(ctx, projectID)
		if err != nil {
			return nil, nil, err
		}
	}
	return hosts, monitors, nil
}

// suppressionHostLabel/suppressionMonitorLabel — «Host: web1» / «Monitor:
// ping-google», либо, если узел с тех пор удалён из проекта, метка "unknown"
// с его числовым id — то же поведение, что у escalationChannelLabels для
// удалённого канала: id ребра остаётся валидным, а имя молча выпадает из
// отображения. Два явных ключа (а не один, собранный конкатенацией "node."+
// kind) — тем же приёмом, что у остального дерева: сканер i18n_keys_test.go
// видит только литеральные ключи, конкатенация требовала бы отдельной записи
// в TestDynamicKeysResolve ради двух вариантов, что дороже двух функций.
func suppressionHostLabel(ctx context.Context, id int64, names map[int64]string) string {
	if name, ok := names[id]; ok {
		return i18n.Tf(ctx, "alert_suppression.node.host", "name", name)
	}
	return i18n.Tf(ctx, "alert_suppression.node.unknown", "kind", i18n.T(ctx, "alert_suppression.kind.host"), "id", strconv.FormatInt(id, 10))
}

func suppressionMonitorLabel(ctx context.Context, id int64, names map[int64]string) string {
	if name, ok := names[id]; ok {
		return i18n.Tf(ctx, "alert_suppression.node.monitor", "name", name)
	}
	return i18n.Tf(ctx, "alert_suppression.node.unknown", "kind", i18n.T(ctx, "alert_suppression.kind.monitor"), "id", strconv.FormatInt(id, 10))
}

// suppressionParentLabel — родитель ребра: всегда явный узел (host или
// monitor), Store.validateShape требует ровно один из двух.
func suppressionParentLabel(ctx context.Context, e depsuppress.Edge, hostNames, monitorNames map[int64]string) string {
	if e.ParentHostID != nil {
		return suppressionHostLabel(ctx, *e.ParentHostID, hostNames)
	}
	if e.ParentMonitorID != nil {
		return suppressionMonitorLabel(ctx, *e.ParentMonitorID, monitorNames)
	}
	return ""
}

// suppressionChildLabel — ребёнок ребра: явный узел ЛИБО label-селектор
// (env/role), Store.validateShape требует ровно один из трёх способов.
func suppressionChildLabel(ctx context.Context, e depsuppress.Edge, hostNames, monitorNames map[int64]string) string {
	switch {
	case e.ChildHostID != nil:
		return suppressionHostLabel(ctx, *e.ChildHostID, hostNames)
	case e.ChildMonitorID != nil:
		return suppressionMonitorLabel(ctx, *e.ChildMonitorID, monitorNames)
	case e.ChildLabelScope != nil && e.ChildLabelValue != nil:
		return i18n.Tf(ctx, "alert_suppression.node.label", "scope", *e.ChildLabelScope, "value", *e.ChildLabelValue)
	default:
		return ""
	}
}

// alertSuppressionEdgeFromForm собирает depsuppress.Edge из уже
// распарсенной формы (r.ParseForm должен быть вызван вызывающей стороной).
// parent_kind/child_kind — radio, выбирающие, какое из параллельных полей
// формы читать (та же форма, что и у полей depsuppress.Edge: ровно один
// родитель, ровно один способ задать ребёнка).
func alertSuppressionEdgeFromForm(r *http.Request, projectID int64) depsuppress.Edge {
	e := depsuppress.Edge{ProjectID: projectID}
	switch r.FormValue("parent_kind") {
	case "host":
		if id, ok := formInt64(r, "parent_host_id"); ok {
			e.ParentHostID = &id
		}
	case "monitor":
		if id, ok := formInt64(r, "parent_monitor_id"); ok {
			e.ParentMonitorID = &id
		}
	}
	switch r.FormValue("child_kind") {
	case "host":
		if id, ok := formInt64(r, "child_host_id"); ok {
			e.ChildHostID = &id
		}
	case "monitor":
		if id, ok := formInt64(r, "child_monitor_id"); ok {
			e.ChildMonitorID = &id
		}
	case "label":
		scope := r.FormValue("child_label_scope")
		value := strings.TrimSpace(r.FormValue("child_label_value"))
		if scope != "" && value != "" {
			e.ChildLabelScope = &scope
			e.ChildLabelValue = &value
		}
	}
	return e
}

// formInt64 разбирает form-поле как int64; отсутствующее/битое значение —
// (0, false), а не паника или молчаливый 0, который прошёл бы дальше как
// валидный id.
func formInt64(r *http.Request, name string) (int64, bool) {
	v := strings.TrimSpace(r.FormValue(name))
	if v == "" {
		return 0, false
	}
	id, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, false
	}
	return id, true
}

// alertSuppressionSave — POST /projects/{id}/alert-suppression: создаёт одно
// ребро зависимости. Cross-tenant защита (concern T2, как у escalations
// channel_id) — здесь не отдельным предфильтром, а через собственную
// транзакционную проверку depsuppress.Store.Create (checkNodesBelongToProject,
// ErrForeignNode): узел, не принадлежащий ЭТОМУ проекту, отвергается ДО
// вставки, тем же порядком, что и self-loop/self-match/duplicate/cycle —
// пре-фильтр отдельным запросом здесь дублировал бы уже существующую в
// сторе проверку без выигрыша в защите (в отличие от escalations, где
// PolicyStore.SetLadder сам ownership каналов не проверяет).
func (h *Handler) alertSuppressionSave(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		h.denyCrossOrigin(w, r)
		return
	}
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	projectID, ok := h.parsePathProjectID(w, r)
	if !ok {
		return
	}
	if h.AlertDeps == nil {
		h.notFound(w, r)
		return
	}
	if _, ok := h.requireProjectOperator(w, r, projectID, uid); !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	edge := alertSuppressionEdgeFromForm(r, projectID)
	if _, err := h.AlertDeps.Create(r.Context(), edge); err != nil {
		h.renderAlertSuppression(w, r, http.StatusUnprocessableEntity, projectID, alertSuppressionErrorMessage(r.Context(), err))
		return
	}
	h.flashOK(w, "flash.saved", 0)
	http.Redirect(w, r, alertSuppressionPath(projectID), http.StatusSeeOther)
}

// alertSuppressionDelete — POST /projects/{id}/alert-suppression/{depID}/delete.
// Delete идемпотентно скоупит удаление на project_id (см. depsuppress.Store.Delete)
// — чужой depID молча ничего не удаляет, а не 403/404 с утечкой существования.
func (h *Handler) alertSuppressionDelete(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		h.denyCrossOrigin(w, r)
		return
	}
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	projectID, ok := h.parsePathProjectID(w, r)
	if !ok {
		return
	}
	if h.AlertDeps == nil {
		h.notFound(w, r)
		return
	}
	if _, ok := h.requireProjectOperator(w, r, projectID, uid); !ok {
		return
	}
	depID, err := strconv.ParseInt(r.PathValue("depID"), 10, 64)
	if err != nil {
		http.Error(w, "bad dependency id", http.StatusBadRequest)
		return
	}
	if err := h.AlertDeps.Delete(r.Context(), projectID, depID); err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	h.flashOK(w, "flash.deleted", 0)
	http.Redirect(w, r, alertSuppressionPath(projectID), http.StatusSeeOther)
}
