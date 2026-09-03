package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/depsuppress"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
)

func ruTestCtx() context.Context {
	return i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"})
}

// TestAlertSuppressionErrorMessage — каждая доменная ошибка depsuppress.Store
// переводится в свой собственный i18n-текст, неизвестная — в общий
// error.action_failed, а не пустую строку/панику на errors.Is(nil,...).
func TestAlertSuppressionErrorMessage(t *testing.T) {
	ctx := ruTestCtx()
	cases := []struct {
		err  error
		want string
	}{
		{depsuppress.ErrForeignNode, i18n.T(ctx, "err.alert_suppression.foreign_node")},
		{depsuppress.ErrSelfLoop, i18n.T(ctx, "err.alert_suppression.self_loop")},
		{depsuppress.ErrSelfMatch, i18n.T(ctx, "err.alert_suppression.self_match")},
		{depsuppress.ErrDuplicate, i18n.T(ctx, "err.alert_suppression.duplicate")},
		{depsuppress.ErrCycle, i18n.T(ctx, "err.alert_suppression.cycle")},
		{depsuppress.ErrInvalidEdge, i18n.T(ctx, "err.alert_suppression.invalid")},
		{errors.New("unrelated"), i18n.T(ctx, "error.action_failed")},
	}
	for _, c := range cases {
		if got := alertSuppressionErrorMessage(ctx, c.err); got != c.want {
			t.Errorf("alertSuppressionErrorMessage(%v) = %q, want %q", c.err, got, c.want)
		}
	}
}

// TestSuppressionParentRef — родитель-хост/монитор резолвится в NodeRef по
// картам имён; отсутствие в карте (узел удалён из проекта) — ok=false, а не
// NodeRef с пустым именем.
func TestSuppressionParentRef(t *testing.T) {
	hostNames := map[int64]string{1: "gw"}
	monNames := map[int64]string{2: "ping"}

	hostID := int64(1)
	ref, ok := suppressionParentRef(depsuppress.Edge{ParentHostID: &hostID}, hostNames, monNames)
	if !ok || ref != (depsuppress.NodeRef{Kind: "host", ID: 1, Name: "gw"}) {
		t.Errorf("host parent = %+v/%v, want host/1/gw true", ref, ok)
	}

	monID := int64(2)
	ref, ok = suppressionParentRef(depsuppress.Edge{ParentMonitorID: &monID}, hostNames, monNames)
	if !ok || ref != (depsuppress.NodeRef{Kind: "monitor", ID: 2, Name: "ping"}) {
		t.Errorf("monitor parent = %+v/%v, want monitor/2/ping true", ref, ok)
	}

	ghostHost := int64(99)
	if _, ok := suppressionParentRef(depsuppress.Edge{ParentHostID: &ghostHost}, hostNames, monNames); ok {
		t.Error("deleted host parent must resolve ok=false")
	}
	ghostMon := int64(98)
	if _, ok := suppressionParentRef(depsuppress.Edge{ParentMonitorID: &ghostMon}, hostNames, monNames); ok {
		t.Error("deleted monitor parent must resolve ok=false")
	}
	if _, ok := suppressionParentRef(depsuppress.Edge{}, hostNames, monNames); ok {
		t.Error("edge with neither parent must resolve ok=false")
	}
}

// TestSuppressionNodeRefLabel — "Monitor: x" для kind=monitor, "Host: x" по
// умолчанию (в т.ч. kind=host).
func TestSuppressionNodeRefLabel(t *testing.T) {
	ctx := ruTestCtx()
	if got, want := suppressionNodeRefLabel(ctx, depsuppress.NodeRef{Kind: "monitor", Name: "ping"}),
		"Монитор: ping"; got != want {
		t.Errorf("monitor label = %q, want %q", got, want)
	}
	if got, want := suppressionNodeRefLabel(ctx, depsuppress.NodeRef{Kind: "host", Name: "gw"}),
		"Хост: gw"; got != want {
		t.Errorf("host label = %q, want %q", got, want)
	}
}

// TestSuppressionHostMonitorLabel — известный id даёт "Kind: имя", удалённый
// (нет в карте) — "Kind #id (удалён)".
func TestSuppressionHostMonitorLabel(t *testing.T) {
	ctx := ruTestCtx()
	names := map[int64]string{1: "gw"}

	if got, want := suppressionHostLabel(ctx, 1, names), "Хост: gw"; got != want {
		t.Errorf("known host = %q, want %q", got, want)
	}
	if got, want := suppressionHostLabel(ctx, 99, names), "Хост #99 (удалён)"; got != want {
		t.Errorf("deleted host = %q, want %q", got, want)
	}
	if got, want := suppressionMonitorLabel(ctx, 1, names), "Монитор: gw"; got != want {
		t.Errorf("known monitor = %q, want %q", got, want)
	}
	if got, want := suppressionMonitorLabel(ctx, 99, names), "Монитор #99 (удалён)"; got != want {
		t.Errorf("deleted monitor = %q, want %q", got, want)
	}
}

// TestSuppressionParentLabel — делегирует на host/monitor-версию по тому,
// какой из указателей заполнен; ни один — пустая строка (не паника).
func TestSuppressionParentLabel(t *testing.T) {
	ctx := ruTestCtx()
	hostNames := map[int64]string{1: "gw"}
	monNames := map[int64]string{2: "ping"}
	hostID, monID := int64(1), int64(2)

	if got, want := suppressionParentLabel(ctx, depsuppress.Edge{ParentHostID: &hostID}, hostNames, monNames),
		"Хост: gw"; got != want {
		t.Errorf("host parent label = %q, want %q", got, want)
	}
	if got, want := suppressionParentLabel(ctx, depsuppress.Edge{ParentMonitorID: &monID}, hostNames, monNames),
		"Монитор: ping"; got != want {
		t.Errorf("monitor parent label = %q, want %q", got, want)
	}
	if got := suppressionParentLabel(ctx, depsuppress.Edge{}, hostNames, monNames); got != "" {
		t.Errorf("edge with no parent set = %q, want empty", got)
	}
}

// TestSuppressionChildLabel — host/monitor/label-селектор ветки switch'а, и
// default (ни один способ не задан) — пустая строка.
func TestSuppressionChildLabel(t *testing.T) {
	ctx := ruTestCtx()
	hostNames := map[int64]string{1: "web1"}
	monNames := map[int64]string{2: "ping"}
	hostID, monID := int64(1), int64(2)
	scope, value := "env", "prod"

	if got, want := suppressionChildLabel(ctx, depsuppress.Edge{ChildHostID: &hostID}, hostNames, monNames),
		"Хост: web1"; got != want {
		t.Errorf("host child = %q, want %q", got, want)
	}
	if got, want := suppressionChildLabel(ctx, depsuppress.Edge{ChildMonitorID: &monID}, hostNames, monNames),
		"Монитор: ping"; got != want {
		t.Errorf("monitor child = %q, want %q", got, want)
	}
	if got, want := suppressionChildLabel(ctx, depsuppress.Edge{ChildLabelScope: &scope, ChildLabelValue: &value}, hostNames, monNames),
		"Окружение = prod"; got != want {
		t.Errorf("label child = %q, want %q", got, want)
	}
	if got := suppressionChildLabel(ctx, depsuppress.Edge{}, hostNames, monNames); got != "" {
		t.Errorf("edge with no child set = %q, want empty", got)
	}
	// Store.validateShape требует ОБА указателя сразу — но suppressionChildLabel
	// не должен доверять этому и разыменовывать ChildLabelValue без проверки:
	// одинокий ChildLabelScope (без value) обязан упасть в default, а не
	// паниковать на nil-указателе.
	if got := suppressionChildLabel(ctx, depsuppress.Edge{ChildLabelScope: &scope}, hostNames, monNames); got != "" {
		t.Errorf("edge with ChildLabelScope but no ChildLabelValue = %q, want empty (defensive default)", got)
	}
}

// TestSuppressionScopeLabel — env/role — локализованные подписи, неизвестный
// scope (defensive) — возвращается как есть.
func TestSuppressionScopeLabel(t *testing.T) {
	ctx := ruTestCtx()
	if got, want := suppressionScopeLabel(ctx, "env"), "Окружение"; got != want {
		t.Errorf("env = %q, want %q", got, want)
	}
	if got, want := suppressionScopeLabel(ctx, "role"), "Роль"; got != want {
		t.Errorf("role = %q, want %q", got, want)
	}
	if got, want := suppressionScopeLabel(ctx, "bogus"), "bogus"; got != want {
		t.Errorf("unknown scope = %q, want passthrough %q", got, want)
	}
}

// formRequest — POST-запрос с урlencoded-телом для тестов formInt64/
// alertSuppressionEdgeFromForm; ParseForm вызывается тут же, как в реальных
// обработчиках.
func formRequest(t *testing.T, values url.Values) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(values.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if err := r.ParseForm(); err != nil {
		t.Fatalf("ParseForm: %v", err)
	}
	return r
}

// TestFormInt64 — валидное число парсится, отсутствующее/пустое/нечисловое
// поле — (0,false), а не паника/молчаливый 0.
func TestFormInt64(t *testing.T) {
	r := formRequest(t, url.Values{"id": {"42"}, "blank": {""}, "garbage": {"abc"}})
	if id, ok := formInt64(r, "id"); !ok || id != 42 {
		t.Errorf("formInt64(id) = %d/%v, want 42/true", id, ok)
	}
	if id, ok := formInt64(r, "blank"); ok || id != 0 {
		t.Errorf("formInt64(blank) = %d/%v, want 0/false", id, ok)
	}
	if id, ok := formInt64(r, "garbage"); ok || id != 0 {
		t.Errorf("formInt64(garbage) = %d/%v, want 0/false", id, ok)
	}
	if id, ok := formInt64(r, "missing"); ok || id != 0 {
		t.Errorf("formInt64(missing) = %d/%v, want 0/false", id, ok)
	}
}

// TestAlertSuppressionEdgeFromForm — все 6 комбинаций parent_kind x
// child_kind (host/monitor родитель, host/monitor/label ребёнок), плюс
// пустой label (scope/value не оба заполнены) не заполняет ChildLabel*.
func TestAlertSuppressionEdgeFromForm(t *testing.T) {
	const projectID = 7

	r := formRequest(t, url.Values{
		"parent_kind": {"host"}, "parent_host_id": {"1"},
		"child_kind": {"monitor"}, "child_monitor_id": {"2"},
	})
	e := alertSuppressionEdgeFromForm(r, projectID)
	if e.ProjectID != projectID || e.ParentHostID == nil || *e.ParentHostID != 1 || e.ChildMonitorID == nil || *e.ChildMonitorID != 2 {
		t.Errorf("host-parent/monitor-child edge = %+v", e)
	}

	r = formRequest(t, url.Values{
		"parent_kind": {"monitor"}, "parent_monitor_id": {"3"},
		"child_kind": {"host"}, "child_host_id": {"4"},
	})
	e = alertSuppressionEdgeFromForm(r, projectID)
	if e.ParentMonitorID == nil || *e.ParentMonitorID != 3 || e.ChildHostID == nil || *e.ChildHostID != 4 {
		t.Errorf("monitor-parent/host-child edge = %+v", e)
	}

	r = formRequest(t, url.Values{
		"parent_kind": {"host"}, "parent_host_id": {"1"},
		"child_kind": {"label"}, "child_label_scope": {"env"}, "child_label_value": {" prod "},
	})
	e = alertSuppressionEdgeFromForm(r, projectID)
	if e.ChildLabelScope == nil || *e.ChildLabelScope != "env" || e.ChildLabelValue == nil || *e.ChildLabelValue != "prod" {
		t.Errorf("label child edge = %+v, want scope=env value=prod (trimmed)", e)
	}

	// label с пустым value после TrimSpace — оба указателя должны остаться nil.
	r = formRequest(t, url.Values{
		"parent_kind": {"host"}, "parent_host_id": {"1"},
		"child_kind": {"label"}, "child_label_scope": {"env"}, "child_label_value": {"   "},
	})
	e = alertSuppressionEdgeFromForm(r, projectID)
	if e.ChildLabelScope != nil || e.ChildLabelValue != nil {
		t.Errorf("blank label value must leave ChildLabel* nil, got %+v", e)
	}

	// Неизвестный/пустой parent_kind — родитель не заполнен вовсе.
	r = formRequest(t, url.Values{"child_kind": {"host"}, "child_host_id": {"5"}})
	e = alertSuppressionEdgeFromForm(r, projectID)
	if e.ParentHostID != nil || e.ParentMonitorID != nil {
		t.Errorf("missing parent_kind must leave both parent fields nil, got %+v", e)
	}
}

// TestSuppressionEdgeFormDefaults — предзаполнение модалки правки из самого
// ребра: те же имена полей, что читает alertSuppressionEdgeFromForm, по одной
// ветке на каждую форму родителя/ребёнка; незадействованные ветки остаются
// без ключа (их поля берут пустые fallback'и).
func TestSuppressionEdgeFormDefaults(t *testing.T) {
	hostID, monID := int64(11), int64(22)
	scope, value := "role", "web"

	f := suppressionEdgeFormDefaults(depsuppress.Edge{ParentHostID: &hostID, ChildMonitorID: &monID})
	for k, want := range map[string]string{
		"parent_kind": "host", "parent_host_id": "11",
		"child_kind": "monitor", "child_monitor_id": "22",
	} {
		if got := f.Get(k, ""); got != want {
			t.Errorf("host->monitor defaults[%s] = %q, want %q", k, got, want)
		}
	}
	if _, ok := f["parent_monitor_id"]; ok {
		t.Errorf("host->monitor defaults must not set parent_monitor_id: %+v", f)
	}

	f = suppressionEdgeFormDefaults(depsuppress.Edge{ParentMonitorID: &monID, ChildHostID: &hostID})
	for k, want := range map[string]string{
		"parent_kind": "monitor", "parent_monitor_id": "22",
		"child_kind": "host", "child_host_id": "11",
	} {
		if got := f.Get(k, ""); got != want {
			t.Errorf("monitor->host defaults[%s] = %q, want %q", k, got, want)
		}
	}

	f = suppressionEdgeFormDefaults(depsuppress.Edge{ParentHostID: &hostID, ChildLabelScope: &scope, ChildLabelValue: &value})
	for k, want := range map[string]string{
		"child_kind": "label", "child_label_scope": "role", "child_label_value": "web",
	} {
		if got := f.Get(k, ""); got != want {
			t.Errorf("label-child defaults[%s] = %q, want %q", k, got, want)
		}
	}
}
