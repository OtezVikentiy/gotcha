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

func alertSuppressionPath(projectID int64) string {
	return templates.AlertSuppressionPath(projectID)
}

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

// доступ — оператор проекта, как у escalations/slos/metric-alerts.
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
	// AlertDeps может быть nil в узких тестовых стендах — guard вместо паники.
	if h.AlertDeps == nil {
		h.notFound(w, r)
		return
	}
	if _, ok := h.requireProjectOperator(w, r, projectID, uid); !ok {
		return
	}
	h.renderAlertSuppression(w, r, http.StatusOK, projectID, nil, "")
}

// form — введённые значения при ошибке валидации с пометкой, какую модалку
// переоткрыть; nil на GET — модалки закрыты, поля со своими fallback'ами.
func (h *Handler) renderAlertSuppression(w http.ResponseWriter, r *http.Request, status int, projectID int64, form templates.FormState, errMsg string) {
	edges, err := h.AlertDeps.List(r.Context(), projectID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "")
		return
	}

	hosts, monitors, err := h.suppressionNodes(r.Context(), projectID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "")
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
			Defaults:    suppressionEdgeFormDefaults(e),
		}
	}

	preview := suppressionPreviewRows(r.Context(), edges, hostNames, monitorNames, hosts, monitors)

	w.WriteHeader(status)
	_ = templates.AlertSuppression(projectID, rows, hostOptions, monitorOptions, preview, h.SuppressionGrace, form, errMsg, h.currentEmail(r)).Render(r.Context(), w)
}

// та же плоская карта имён, что читает alertSuppressionEdgeFromForm — иначе
// общий фрагмент полей не подставит значения при правке.
func suppressionEdgeFormDefaults(e depsuppress.Edge) templates.FormState {
	f := templates.FormState{}
	switch {
	case e.ParentHostID != nil:
		f["parent_kind"] = "host"
		f["parent_host_id"] = strconv.FormatInt(*e.ParentHostID, 10)
	case e.ParentMonitorID != nil:
		f["parent_kind"] = "monitor"
		f["parent_monitor_id"] = strconv.FormatInt(*e.ParentMonitorID, 10)
	}
	switch {
	case e.ChildHostID != nil:
		f["child_kind"] = "host"
		f["child_host_id"] = strconv.FormatInt(*e.ChildHostID, 10)
	case e.ChildMonitorID != nil:
		f["child_kind"] = "monitor"
		f["child_monitor_id"] = strconv.FormatInt(*e.ChildMonitorID, 10)
	case e.ChildLabelScope != nil && e.ChildLabelValue != nil:
		f["child_kind"] = "label"
		f["child_label_scope"] = *e.ChildLabelScope
		f["child_label_value"] = *e.ChildLabelValue
	}
	return f
}

// собирается поимённо из известных полей, не копированием r.Form целиком —
// см. инвариант formModalKey.
func suppressionFormState(r *http.Request) templates.FormState {
	form := templates.FormState{}
	for _, name := range []string{
		"parent_kind", "parent_host_id", "parent_monitor_id",
		"child_kind", "child_host_id", "child_monitor_id",
		"child_label_scope", "child_label_value",
	} {
		form[name] = r.FormValue(name)
	}
	return form
}

// первое вхождение родителя в edges занимает позицию строки;
// PreviewSuppression — чистая функция без БД, здесь только сборка входа/вывода.
func suppressionPreviewRows(ctx context.Context, edges []depsuppress.Edge, hostNames, monitorNames map[int64]string, hosts []host.Host, monitors []uptime.Monitor) []templates.SuppressionPreviewView {
	hostLites := make([]depsuppress.HostLite, len(hosts))
	for i, hh := range hosts {
		hostLites[i] = depsuppress.HostLite{ID: hh.ID, Name: hh.Name, Environment: hh.Environment, Role: hh.Role}
	}
	monitorRefs := make([]depsuppress.NodeRef, len(monitors))
	for i, m := range monitors {
		monitorRefs[i] = depsuppress.NodeRef{Kind: "monitor", ID: m.ID, Name: m.Name}
	}

	preview := depsuppress.PreviewSuppression(edges, hostLites, monitorRefs)

	var rows []templates.SuppressionPreviewView
	seenParent := map[depsuppress.NodeRef]bool{}
	for _, e := range edges {
		parent, ok := suppressionParentRef(e, hostNames, monitorNames)
		if !ok || seenParent[parent] {
			continue
		}
		children := preview[parent]
		if len(children) == 0 {
			continue
		}
		seenParent[parent] = true
		childLabels := make([]string, len(children))
		for i, c := range children {
			childLabels[i] = suppressionNodeRefLabel(ctx, c)
		}
		rows = append(rows, templates.SuppressionPreviewView{
			ParentLabel: suppressionNodeRefLabel(ctx, parent),
			Children:    childLabels,
		})
	}
	return rows
}

// false — родитель с тех пор удалён из проекта, строка dry-run для него не строится.
func suppressionParentRef(e depsuppress.Edge, hostNames, monitorNames map[int64]string) (depsuppress.NodeRef, bool) {
	if e.ParentHostID != nil {
		name, ok := hostNames[*e.ParentHostID]
		if !ok {
			return depsuppress.NodeRef{}, false
		}
		return depsuppress.NodeRef{Kind: "host", ID: *e.ParentHostID, Name: name}, true
	}
	if e.ParentMonitorID != nil {
		name, ok := monitorNames[*e.ParentMonitorID]
		if !ok {
			return depsuppress.NodeRef{}, false
		}
		return depsuppress.NodeRef{Kind: "monitor", ID: *e.ParentMonitorID, Name: name}, true
	}
	return depsuppress.NodeRef{}, false
}

// имя уже резолвлено в NodeRef.Name — используем те же i18n-ключи, что список рёбер.
func suppressionNodeRefLabel(ctx context.Context, n depsuppress.NodeRef) string {
	switch n.Kind {
	case "monitor":
		return i18n.Tf(ctx, "alert_suppression.node.monitor", "name", n.Name)
	default:
		return i18n.Tf(ctx, "alert_suppression.node.host", "name", n.Name)
	}
}

// nil-safe: h.Hosts/h.Uptime могут быть не заведены в узких тестовых стендах —
// тогда список остаётся пустым, а не паникует.
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

// удалённый узел — метка "unknown" с id (как у escalationChannelLabels).
// два явных ключа, не конкатенация — сканер i18n_keys_test.go видит только литеральные.
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

// родитель ребра — всегда явный узел (host или monitor):
// Store.validateShape требует ровно один из двух.
func suppressionParentLabel(ctx context.Context, e depsuppress.Edge, hostNames, monitorNames map[int64]string) string {
	if e.ParentHostID != nil {
		return suppressionHostLabel(ctx, *e.ParentHostID, hostNames)
	}
	if e.ParentMonitorID != nil {
		return suppressionMonitorLabel(ctx, *e.ParentMonitorID, monitorNames)
	}
	return ""
}

// ребёнок ребра — явный узел ЛИБО label-селектор (env/role):
// Store.validateShape требует ровно один из трёх способов.
func suppressionChildLabel(ctx context.Context, e depsuppress.Edge, hostNames, monitorNames map[int64]string) string {
	switch {
	case e.ChildHostID != nil:
		return suppressionHostLabel(ctx, *e.ChildHostID, hostNames)
	case e.ChildMonitorID != nil:
		return suppressionMonitorLabel(ctx, *e.ChildMonitorID, monitorNames)
	case e.ChildLabelScope != nil && e.ChildLabelValue != nil:
		return i18n.Tf(ctx, "alert_suppression.node.label", "scope", suppressionScopeLabel(ctx, *e.ChildLabelScope), "value", *e.ChildLabelValue)
	default:
		return ""
	}
}

// явный switch, не конкатенация: сканер i18n_keys_test.go видит только буквальные вызовы i18n.T.
// неизвестный scope невозможен (Store.validateShape), default — просто подстраховка.
func suppressionScopeLabel(ctx context.Context, scope string) string {
	switch scope {
	case "env":
		return i18n.T(ctx, "alert_suppression.scope.env")
	case "role":
		return i18n.T(ctx, "alert_suppression.scope.role")
	default:
		return scope
	}
}

// parent_kind/child_kind — radio: ровно один родитель, ровно один способ задать ребёнка.
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

// отсутствующее/битое значение — (0, false), не паника и не молчаливый 0,
// который прошёл бы дальше как валидный id.
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

// cross-tenant защита — не отдельным предфильтром, а транзакционной проверкой
// внутри Store.Create (ErrForeignNode); отдельный запрос здесь дублировал бы её без пользы.
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
	if !h.parseForm(w, r) {
		return
	}

	edge := alertSuppressionEdgeFromForm(r, projectID)
	if _, err := h.AlertDeps.Create(r.Context(), edge); err != nil {
		form := suppressionFormState(r).Open(templates.SuppressionCreateModalID)
		h.renderAlertSuppression(w, r, http.StatusUnprocessableEntity, projectID, form, alertSuppressionErrorMessage(r.Context(), err))
		return
	}
	h.flashOK(w, "flash.saved", 0)
	http.Redirect(w, r, alertSuppressionPath(projectID), http.StatusSeeOther)
}

// чужой depID не в скоупе Store.Update (ErrNotFound) — 404 без утечки существования.
// suppressed_by_dep одноразовый: на уже открытые инциденты правка не действует.
func (h *Handler) alertSuppressionUpdate(w http.ResponseWriter, r *http.Request) {
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
		h.renderError(w, r, http.StatusBadRequest, i18n.T(r.Context(), "error.bad_request"))
		return
	}
	if !h.parseForm(w, r) {
		return
	}

	edge := alertSuppressionEdgeFromForm(r, projectID)
	edge.ID = depID
	if err := h.AlertDeps.Update(r.Context(), edge); err != nil {
		if errors.Is(err, depsuppress.ErrNotFound) {
			h.notFound(w, r)
			return
		}
		form := suppressionFormState(r).Open(templates.EditSuppressionEdgeModalID(depID))
		h.renderAlertSuppression(w, r, http.StatusUnprocessableEntity, projectID, form, alertSuppressionErrorMessage(r.Context(), err))
		return
	}
	h.flashOK(w, "flash.saved", 0)
	http.Redirect(w, r, alertSuppressionPath(projectID), http.StatusSeeOther)
}

// Delete идемпотентно скоупит по project_id — чужой depID молча ничего не
// удаляет, без утечки существования.
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
		h.renderError(w, r, http.StatusBadRequest, i18n.T(r.Context(), "error.bad_request"))
		return
	}
	if !h.parseForm(w, r) {
		return
	}
	// двухшаговое подтверждение — CSP (default-src 'self', без unsafe-inline) не
	// исполняет inline confirm(); чужой/несуществующий depID здесь тоже 404.
	if r.FormValue("confirmed") != "yes" {
		parent, child, ok, err := h.suppressionEdgeLabels(r.Context(), projectID, depID)
		if err != nil {
			h.renderError(w, r, http.StatusInternalServerError, "")
			return
		}
		if !ok {
			h.notFound(w, r)
			return
		}
		h.renderConfirmf(w, r, "confirm.title", "confirm.suppression_edge_delete.message", "confirm.delete",
			alertSuppressionPath(projectID), alertSuppressionPath(projectID)+"/"+strconv.FormatInt(depID, 10)+"/delete", nil,
			"parent", parent, "child", child)
		return
	}
	if err := h.AlertDeps.Delete(r.Context(), projectID, depID); err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "")
		return
	}
	h.flashOK(w, "flash.deleted", 0)
	http.Redirect(w, r, alertSuppressionPath(projectID), http.StatusSeeOther)
}

// те же suppressionParentLabel/suppressionChildLabel, что у строк таблицы;
// ok=false — ребра с таким id в проекте нет.
func (h *Handler) suppressionEdgeLabels(ctx context.Context, projectID, depID int64) (parent, child string, ok bool, err error) {
	edges, err := h.AlertDeps.List(ctx, projectID)
	if err != nil {
		return "", "", false, err
	}
	var edge depsuppress.Edge
	for _, e := range edges {
		if e.ID == depID {
			edge, ok = e, true
			break
		}
	}
	if !ok {
		return "", "", false, nil
	}
	hosts, monitors, err := h.suppressionNodes(ctx, projectID)
	if err != nil {
		return "", "", false, err
	}
	hostNames := make(map[int64]string, len(hosts))
	for _, hh := range hosts {
		hostNames[hh.ID] = hh.Name
	}
	monitorNames := make(map[int64]string, len(monitors))
	for _, m := range monitors {
		monitorNames[m.ID] = m.Name
	}
	return suppressionParentLabel(ctx, edge, hostNames, monitorNames), suppressionChildLabel(ctx, edge, hostNames, monitorNames), true, nil
}
