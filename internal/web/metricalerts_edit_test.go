package web_test

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/metric"
)

// metricRuleUpdateForm — валидная форма правки со всеми полями, как её шлёт
// браузер со СНЯТЫМ чекбоксом «Включено»: hidden enabled=off приходит всегда,
// "on" от чекбокса добавляет вызывающий (form.Add("enabled", "on")).
func metricRuleUpdateForm() url.Values {
	return url.Values{
		"metric_name": {"cpu.load"}, "aggregation": {"p95"}, "comparator": {"lt"},
		"threshold": {"42.5"}, "window_seconds": {"600"}, "environment": {"prod"},
		"label_key": {"az"}, "label_value": {"a1"}, "severity": {"critical"},
		"enabled": {"off"},
	}
}

func TestWebMetricAlertUpdate(t *testing.T) {
	s := newMetricAlertsStack(t, true)
	ctx := context.Background()
	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, "ma-upd-owner@example.com")
	o, err := s.org.CreateOrg(ctx, "ma-upd-co", "MA Upd Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	project, err := s.org.CreateProject(ctx, o.ID, "ma-upd-proj", "MA Upd Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	base := "/projects/" + strconv.FormatInt(project.ID, 10) + "/metrics/alerts"

	target, err := s.rules.Create(ctx, metric.Rule{ProjectID: project.ID, MetricName: "cpu", Aggregation: "avg", Comparator: "gt", Threshold: 90, WindowSeconds: 300, Enabled: true})
	if err != nil {
		t.Fatalf("create target: %v", err)
	}
	witness, err := s.rules.Create(ctx, metric.Rule{ProjectID: project.ID, MetricName: "mem", Aggregation: "max", Comparator: "gt", Threshold: 80, WindowSeconds: 60, Enabled: true})
	if err != nil {
		t.Fatalf("create witness: %v", err)
	}
	updPath := base + "/" + strconv.FormatInt(target.ID, 10)

	// До правки оба правила включены — бейджа «Выключено» на странице нет.
	resp := getWithCookie(t, s.srv, base, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || strings.Contains(string(body), "badge badge-neutral") {
		t.Fatalf("page before update (status %d) must not show disabled badge", resp.StatusCode)
	}

	// Правка без чекбокса enabled → 303: все поля обновлены, правило выключено.
	resp = postForm(t, s.srv, updPath, metricRuleUpdateForm(), s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("update status = %d, want 303", resp.StatusCode)
	}
	got, found, err := s.rules.Get(ctx, target.ID)
	if err != nil || !found {
		t.Fatalf("get target: (%v,%v)", found, err)
	}
	if got.MetricName != "cpu.load" || got.Aggregation != "p95" || got.Comparator != "lt" ||
		got.Threshold != 42.5 || got.WindowSeconds != 600 || got.Environment != "prod" ||
		got.LabelKey != "az" || got.LabelValue != "a1" || got.Enabled || got.Severity != "critical" {
		t.Fatalf("updated rule = %+v", got)
	}
	// Свидетель не тронут — правка задела ИМЕННО целевое правило.
	w2, found, err := s.rules.Get(ctx, witness.ID)
	if err != nil || !found {
		t.Fatalf("get witness: (%v,%v)", found, err)
	}
	if w2.MetricName != "mem" || w2.Threshold != 80 || !w2.Enabled {
		t.Fatalf("witness changed: %+v", w2)
	}

	// Выключение видно в бейдже.
	resp = getWithCookie(t, s.srv, base, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "badge badge-neutral") {
		t.Fatalf("disabled badge missing after update: %s", body)
	}

	// Включение обратно (чекбокс взведён: браузер шлёт и hidden "off", и
	// "on") — бейдж «Выключено» пропадает.
	form := metricRuleUpdateForm()
	form.Add("enabled", "on")
	resp = postForm(t, s.srv, updPath, form, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("re-enable status = %d, want 303", resp.StatusCode)
	}
	resp = getWithCookie(t, s.srv, base, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.Contains(string(body), "badge badge-neutral") {
		t.Fatalf("disabled badge must disappear after re-enable")
	}

	// Нечисловой ruleID → 400.
	resp = postForm(t, s.srv, base+"/abc", metricRuleUpdateForm(), s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad ruleID status = %d, want 400", resp.StatusCode)
	}

	// Без Origin → 403.
	resp = postForm(t, s.srv, updPath, metricRuleUpdateForm(), "", ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("no-origin status = %d, want 403", resp.StatusCode)
	}

	// Кросс-тенант, вариант 1: посторонний пользователь правит правило чужого
	// проекта → 404 (existence-oracle requireProjectOperator).
	_, foreignCookie := orgSettingsRegister(t, s.auth, "ma-upd-foreign@example.com")
	resp = postForm(t, s.srv, updPath, metricRuleUpdateForm(), s.srv.URL, foreignCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("foreign user update status = %d, want 404", resp.StatusCode)
	}

	// Кросс-тенант, вариант 2: владелец СВОЕГО проекта подставляет чужой
	// ruleID в путь своего проекта → 404 из скоупа RuleService.Update,
	// чужое правило не изменилось.
	attackerID, attackerCookie := orgSettingsRegister(t, s.auth, "ma-upd-attacker@example.com")
	ao, err := s.org.CreateOrg(ctx, "ma-upd-att-co", "MA Att Co", attackerID)
	if err != nil {
		t.Fatalf("create attacker org: %v", err)
	}
	ap, err := s.org.CreateProject(ctx, ao.ID, "ma-upd-att-proj", "MA Att Proj", "go")
	if err != nil {
		t.Fatalf("create attacker project: %v", err)
	}
	hijack := metricRuleUpdateForm()
	hijack.Set("metric_name", "hacked")
	attackerPath := "/projects/" + strconv.FormatInt(ap.ID, 10) + "/metrics/alerts/" + strconv.FormatInt(target.ID, 10)
	resp = postForm(t, s.srv, attackerPath, hijack, s.srv.URL, attackerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-project ruleID status = %d, want 404", resp.StatusCode)
	}
	if after, _, _ := s.rules.Get(ctx, target.ID); after.MetricName != "cpu.load" {
		t.Fatalf("cross-project update leaked: %+v", after)
	}
}

func TestWebMetricAlertUpdateValidationReopensModal(t *testing.T) {
	s := newMetricAlertsStack(t, true)
	ctx := context.Background()
	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, "ma-modal-owner@example.com")
	o, err := s.org.CreateOrg(ctx, "ma-modal-co", "MA Modal Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	project, err := s.org.CreateProject(ctx, o.ID, "ma-modal-proj", "MA Modal Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	base := "/projects/" + strconv.FormatInt(project.ID, 10) + "/metrics/alerts"

	r1, err := s.rules.Create(ctx, metric.Rule{ProjectID: project.ID, MetricName: "cpu", Aggregation: "avg", Comparator: "gt", Threshold: 90, WindowSeconds: 300, Enabled: true})
	if err != nil {
		t.Fatalf("create r1: %v", err)
	}
	r2, err := s.rules.Create(ctx, metric.Rule{ProjectID: project.ID, MetricName: "mem", Aggregation: "max", Comparator: "gt", Threshold: 80, WindowSeconds: 60, Enabled: true})
	if err != nil {
		t.Fatalf("create r2: %v", err)
	}

	// 422 правки: переоткрывается модалка ИМЕННО правила r2 (и только она),
	// введённое сохранено, снятый чекбокс enabled остаётся снятым.
	bad := metricRuleUpdateForm()
	bad.Set("metric_name", "typed.metric.value")
	bad.Set("threshold", "NaN")
	resp := postForm(t, s.srv, base+"/"+strconv.FormatInt(r2.ID, 10), bad, s.srv.URL, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("edit 422 status = %d", resp.StatusCode)
	}
	bodyStr := string(body)
	openID := "edit-metric-rule-" + strconv.FormatInt(r2.ID, 10)
	if !strings.Contains(bodyStr, `id="`+openID+`" class="modal modal--open"`) {
		t.Fatalf("422 must reopen edit modal %s: %s", openID, bodyStr)
	}
	if got := strings.Count(bodyStr, "modal--open"); got != 1 {
		t.Fatalf("want exactly 1 open modal (edit %s), got %d", openID, got)
	}
	closedID := "edit-metric-rule-" + strconv.FormatInt(r1.ID, 10)
	if strings.Contains(bodyStr, `id="`+closedID+`" class="modal modal--open"`) {
		t.Fatalf("modal of untouched rule %d must stay closed", r1.ID)
	}
	if !strings.Contains(bodyStr, `value="typed.metric.value"`) {
		t.Fatalf("typed value lost after 422: %s", bodyStr)
	}
	// Фрагмент открытой модалки: чекбокс enabled остался снятым (в POST
	// пришёл только hidden "off").
	start := strings.Index(bodyStr, `id="`+openID+`" class="modal modal--open"`)
	end := strings.Index(bodyStr[start:], "</form>")
	fragment := bodyStr[start : start+end]
	if strings.Contains(fragment, `value="on" checked`) {
		t.Fatalf("unchecked enabled must stay unchecked after 422: %s", fragment)
	}

	// 422 создания: переоткрывается модалка создания, а не правки.
	create := metricRuleUpdateForm()
	create.Set("metric_name", "created.metric.value")
	create.Set("threshold", "NaN")
	resp = postForm(t, s.srv, base, create, s.srv.URL, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("create 422 status = %d", resp.StatusCode)
	}
	bodyStr = string(body)
	if !strings.Contains(bodyStr, `id="new-metric-rule" class="modal modal--open"`) {
		t.Fatalf("create 422 must reopen create modal: %s", bodyStr)
	}
	if got := strings.Count(bodyStr, "modal--open"); got != 1 {
		t.Fatalf("create 422: want exactly 1 open modal, got %d", got)
	}
	if !strings.Contains(bodyStr, `value="created.metric.value"`) {
		t.Fatalf("typed value lost after create 422")
	}

	// Ошибка валидации из СЕРВИСА (форма её не ловит: aggregation проверяет
	// только RuleService.validateRule) — тоже 422 с переоткрытием модалки
	// именно правимого правила, а не 500.
	svcBad := metricRuleUpdateForm()
	svcBad.Set("aggregation", "bogus")
	resp = postForm(t, s.srv, base+"/"+strconv.FormatInt(r2.ID, 10), svcBad, s.srv.URL, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("service-invalid 422 status = %d", resp.StatusCode)
	}
	if !strings.Contains(string(body), `id="`+openID+`" class="modal modal--open"`) {
		t.Fatalf("service-invalid 422 must reopen edit modal %s", openID)
	}

	// Прямой POST мимо формы с посторонней severity — 422 из
	// metricRuleFromForm (до сервиса), а не 500 от CHECK-ограничения БД.
	sevBad := metricRuleUpdateForm()
	sevBad.Set("severity", "bogus")
	resp = postForm(t, s.srv, base+"/"+strconv.FormatInt(r2.ID, 10), sevBad, s.srv.URL, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("bogus severity 422 status = %d", resp.StatusCode)
	}
	if !strings.Contains(string(body), `id="`+openID+`" class="modal modal--open"`) {
		t.Fatalf("bogus severity 422 must reopen edit modal %s", openID)
	}
}

func TestWebMetricAlertRuleModalUniqueIDs(t *testing.T) {
	s := newMetricAlertsStack(t, true)
	ctx := context.Background()
	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, "ma-ids-owner@example.com")
	o, err := s.org.CreateOrg(ctx, "ma-ids-co", "MA IDs Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	project, err := s.org.CreateProject(ctx, o.ID, "ma-ids-proj", "MA IDs Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	names := []string{"cpu", "mem", "disk"}
	for i, name := range names {
		if _, err := s.rules.Create(ctx, metric.Rule{ProjectID: project.ID, MetricName: name, Aggregation: "avg", Comparator: "gt", Threshold: float64(10 * (i + 1)), WindowSeconds: 300, Enabled: true}); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
	}
	base := "/projects/" + strconv.FormatInt(project.ID, 10) + "/metrics/alerts"
	resp := getWithCookie(t, s.srv, base, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("page status = %d", resp.StatusCode)
	}
	// Несколько правил — модалка правки на каждую строку: ни один id
	// в документе не должен повторяться (datalist known-metrics — один).
	seen := map[string]int{}
	for _, m := range regexp.MustCompile(`id="([^"]+)"`).FindAllStringSubmatch(string(body), -1) {
		seen[m[1]]++
	}
	for id, n := range seen {
		if n > 1 {
			t.Fatalf("duplicate id %q: %d occurrences", id, n)
		}
	}
	if seen["known-metrics"] != 1 {
		t.Fatalf("datalist known-metrics must appear exactly once, got %d", seen["known-metrics"])
	}
	// Каждая форма (создание + правка на строку) несёт hidden enabled=off —
	// без него снятый чекбокс не отличим от POST мимо формы; и в каждой
	// (правила включены, у создания дефолт «включено») чекбокс взведён —
	// позитивный контроль для негативного ассерта в тесте переоткрытия.
	want := len(names) + 1
	if got := strings.Count(string(body), `type="hidden" name="enabled" value="off"`); got != want {
		t.Fatalf("hidden enabled=off: %d, want %d (one per form)", got, want)
	}
	if got := strings.Count(string(body), `value="on" checked`); got != want {
		t.Fatalf("checked enabled boxes: %d, want %d", got, want)
	}
}

// TestWebMetricAlertCreateEnabledContract — контракт поля enabled на
// создании: POST без поля вовсе (клиент старее формы либо запрос мимо неё) —
// правило ВКЛЮЧЕНО, как до появления чекбокса; hidden "off" без "on" (снятый
// чекбокс) — выключено; "off"+"on" (взведённый) — включено.
func TestWebMetricAlertCreateEnabledContract(t *testing.T) {
	s := newMetricAlertsStack(t, true)
	ctx := context.Background()
	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, "ma-en-owner@example.com")
	o, err := s.org.CreateOrg(ctx, "ma-en-co", "MA En Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	project, err := s.org.CreateProject(ctx, o.ID, "ma-en-proj", "MA En Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	base := "/projects/" + strconv.FormatInt(project.ID, 10) + "/metrics/alerts"

	cases := []struct {
		name    string
		enabled []string
		want    bool
	}{
		{"absent-field-defaults-enabled", nil, true},
		{"hidden-only-disabled", []string{"off"}, false},
		{"hidden-plus-checkbox-enabled", []string{"off", "on"}, true},
	}
	for i, tc := range cases {
		form := url.Values{
			"metric_name": {"m." + strconv.Itoa(i)}, "aggregation": {"avg"}, "comparator": {"gt"},
			"threshold": {"100"}, "window_seconds": {"300"},
		}
		if tc.enabled != nil {
			form["enabled"] = tc.enabled
		}
		resp := postForm(t, s.srv, base, form, s.srv.URL, ownerCookie)
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("%s: create status = %d, want 303", tc.name, resp.StatusCode)
		}
		rules, err := s.rules.List(ctx, project.ID)
		if err != nil {
			t.Fatalf("%s: list: %v", tc.name, err)
		}
		var found bool
		for _, r := range rules {
			if r.MetricName == "m."+strconv.Itoa(i) {
				found = true
				if r.Enabled != tc.want {
					t.Fatalf("%s: Enabled = %v, want %v", tc.name, r.Enabled, tc.want)
				}
			}
		}
		if !found {
			t.Fatalf("%s: rule not created", tc.name)
		}
	}

	// Правка: отсутствие поля enabled целиком — тот же контракт «включено»,
	// что у создания. Эндпоинт — полная замена правила (отсутствие environment
	// стирает окружение, отсутствие severity сбрасывает наследование), и
	// результат определяется запросом, а не историей строки; из формы случай
	// недостижим — hidden enabled=off шлётся всегда.
	disabled, err := s.rules.Create(ctx, metric.Rule{ProjectID: project.ID, MetricName: "upd.absent", Aggregation: "avg", Comparator: "gt", Threshold: 5, WindowSeconds: 60, Enabled: false})
	if err != nil {
		t.Fatalf("create disabled rule: %v", err)
	}
	form := url.Values{
		"metric_name": {"upd.absent"}, "aggregation": {"avg"}, "comparator": {"gt"},
		"threshold": {"5"}, "window_seconds": {"60"},
	}
	resp := postForm(t, s.srv, base+"/"+strconv.FormatInt(disabled.ID, 10), form, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("update without enabled field: status = %d, want 303", resp.StatusCode)
	}
	if got, _, _ := s.rules.Get(ctx, disabled.ID); !got.Enabled {
		t.Fatalf("update without enabled field must follow the create contract (enabled): %+v", got)
	}
}
