package web_test

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/depsuppress"
	"gitflic.ru/otezvikentiy/gotcha/internal/host"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
)

// suppressionSeedHost/suppressionSeedMonitor — минимальные вставки узла в
// проект напрямую через пул, тем же приёмом, что и seedHost в
// internal/depsuppress/store_test.go (Task 2): здесь важна форма запроса, а
// не путь через host.Store.Upsert/uptime.Service.Create, которым для
// создания одной строки пришлось бы тащить лишние обязательные поля
// (troch-протокол хоста, регионы/каналы монитора).
func suppressionSeedHost(t *testing.T, s *stack, projectID int64, name string) int64 {
	t.Helper()
	var id int64
	if err := s.pool.QueryRow(context.Background(),
		`INSERT INTO hosts (project_id, name, environment, role) VALUES ($1,$2,'prod','web') RETURNING id`,
		projectID, name).Scan(&id); err != nil {
		t.Fatalf("seed host: %v", err)
	}
	return id
}

// suppressionSeedHostRole — как suppressionSeedHost, но с явной ролью:
// нужна TestWebAlertSuppressionPreview для label-ребра, где парент и
// раскрываемый ребёнок должны быть в РАЗНЫХ ролях (иначе
// depsuppress.Store.Create сам отвергнет ребро как ErrSelfMatch — self-match
// уже не даёт создать такое ребро через настоящий Create, поэтому его
// исключение проверено отдельно в internal/depsuppress/preview_test.go на
// самой чистой функции, где Edge собирается напрямую в обход валидации Store).
func suppressionSeedHostRole(t *testing.T, s *stack, projectID int64, name, role string) int64 {
	t.Helper()
	var id int64
	if err := s.pool.QueryRow(context.Background(),
		`INSERT INTO hosts (project_id, name, environment, role) VALUES ($1,$2,'prod',$3) RETURNING id`,
		projectID, name, role).Scan(&id); err != nil {
		t.Fatalf("seed host: %v", err)
	}
	return id
}

func suppressionSeedMonitor(t *testing.T, s *stack, projectID int64, name string) int64 {
	t.Helper()
	var id int64
	if err := s.pool.QueryRow(context.Background(),
		`INSERT INTO monitors (project_id, name, kind, interval_seconds) VALUES ($1,$2,'http',60) RETURNING id`,
		projectID, name).Scan(&id); err != nil {
		t.Fatalf("seed monitor: %v", err)
	}
	return id
}

// wireAlertSuppression заводит h.AlertDeps/h.Hosts/h.Uptime на стенде —
// узкий стенд newStack их не проводит (как и h.EscalationPolicy в
// escalations_test.go), а странице нужны все три: Store для CRUD рёбер,
// Hosts/Uptime — резолвить id в человекочитаемое имя и наполнить селекты
// формы.
func wireAlertSuppression(s *stack) {
	s.h.AlertDeps = depsuppress.NewStore(s.pool)
	s.h.Hosts = host.NewStore(s.pool)
	s.h.Uptime = uptime.NewService(s.pool)
}

// TestWebAlertSuppressionPage — owner (оператор проекта) видит список рёбер
// с резолвленными именами узлов и форму добавления; member без командного
// доступа к проекту — 404 (тот же existence-oracle, что и escalations).
func TestWebAlertSuppressionPage(t *testing.T) {
	s := newStack(t)
	wireAlertSuppression(s)
	authSvc := auth.NewService(s.pool)
	orgSvc := org.NewService(s.pool, 1_000_000)

	ownerID, ownerCookie := orgSettingsRegister(t, authSvc, "dep-owner@example.com")
	memberID, memberCookie := orgSettingsRegister(t, authSvc, "dep-member@example.com")

	o, err := orgSvc.CreateOrg(context.Background(), "dep-co", "Dep Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	if err := orgSvc.AddMember(context.Background(), o.ID, memberID, org.RoleMember); err != nil {
		t.Fatalf("add member: %v", err)
	}
	proj, err := orgSvc.CreateProject(context.Background(), o.ID, "dep-proj", "Dep Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	monID := suppressionSeedMonitor(t, s, proj.ID, "ping-gw")
	hostID := suppressionSeedHost(t, s, proj.ID, "web-1")
	if _, err := s.h.AlertDeps.Create(context.Background(), depsuppress.Edge{
		ProjectID: proj.ID, ParentMonitorID: &monID, ChildHostID: &hostID,
	}); err != nil {
		t.Fatalf("seed edge: %v", err)
	}

	path := "/projects/" + strconv.FormatInt(proj.ID, 10) + "/alert-suppression"

	resp := getWithCookie(t, s.srv, path, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s (owner) status = %d, want 200: %s", path, resp.StatusCode, body)
	}
	for _, want := range []string{"ping-gw", "web-1", "parent_kind", "child_kind"} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("GET %s missing %q: %s", path, want, body)
		}
	}

	resp = getWithCookie(t, s.srv, path, memberCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET %s (member, no team) status = %d, want 404", path, resp.StatusCode)
	}
}

// TestWebAlertSuppressionSaveAndDelete — POST с валидным ребром создаёт
// строку в Store (303 редирект), она видна в GET-списке; POST на
// .../{depID}/delete удаляет её (303 редирект), List снова пуст.
func TestWebAlertSuppressionSaveAndDelete(t *testing.T) {
	s := newStack(t)
	wireAlertSuppression(s)
	authSvc := auth.NewService(s.pool)
	orgSvc := org.NewService(s.pool, 1_000_000)

	ownerID, ownerCookie := orgSettingsRegister(t, authSvc, "dep-save-owner@example.com")
	o, err := orgSvc.CreateOrg(context.Background(), "dep-save-co", "Dep Save Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	proj, err := orgSvc.CreateProject(context.Background(), o.ID, "dep-save-proj", "Dep Save Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	hostID := suppressionSeedHost(t, s, proj.ID, "web-1")
	monID := suppressionSeedMonitor(t, s, proj.ID, "ping-gw")

	path := "/projects/" + strconv.FormatInt(proj.ID, 10) + "/alert-suppression"
	form := url.Values{
		"parent_kind":      {"host"},
		"parent_host_id":   {strconv.FormatInt(hostID, 10)},
		"child_kind":       {"monitor"},
		"child_monitor_id": {strconv.FormatInt(monID, 10)},
	}
	resp := postForm(t, s.srv, path, form, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("save valid edge status = %d, want 303", resp.StatusCode)
	}

	edges, err := s.h.AlertDeps.List(context.Background(), proj.ID)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(edges) != 1 || edges[0].ParentHostID == nil || *edges[0].ParentHostID != hostID ||
		edges[0].ChildMonitorID == nil || *edges[0].ChildMonitorID != monID {
		t.Fatalf("List = %+v, want single edge host(%d) -> monitor(%d)", edges, hostID, monID)
	}

	deletePath := path + "/" + strconv.FormatInt(edges[0].ID, 10) + "/delete"
	resp = postForm(t, s.srv, deletePath, url.Values{}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("delete edge status = %d, want 303", resp.StatusCode)
	}

	edges, err = s.h.AlertDeps.List(context.Background(), proj.ID)
	if err != nil {
		t.Fatalf("List after delete: %v", err)
	}
	if len(edges) != 0 {
		t.Fatalf("List after delete = %+v, want empty", edges)
	}
}

// TestWebAlertSuppressionCrossTenant — concern T2: узел (хост) чужого
// проекта в форме отвергается ДО вставки (defense-in-depth самого
// depsuppress.Store.Create, ErrForeignNode → 422), ребро не создаётся.
func TestWebAlertSuppressionCrossTenant(t *testing.T) {
	s := newStack(t)
	wireAlertSuppression(s)
	authSvc := auth.NewService(s.pool)
	orgSvc := org.NewService(s.pool, 1_000_000)

	ownerID, ownerCookie := orgSettingsRegister(t, authSvc, "dep-cross-owner@example.com")
	o, err := orgSvc.CreateOrg(context.Background(), "dep-cross-co", "Dep Cross Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	mine, err := orgSvc.CreateProject(context.Background(), o.ID, "dep-cross-mine", "Mine", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	theirs, err := orgSvc.CreateProject(context.Background(), o.ID, "dep-cross-theirs", "Theirs", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	myMonitor := suppressionSeedMonitor(t, s, mine.ID, "my-mon")
	foreignHost := suppressionSeedHost(t, s, theirs.ID, "foreign-host")

	path := "/projects/" + strconv.FormatInt(mine.ID, 10) + "/alert-suppression"
	form := url.Values{
		"parent_kind":       {"monitor"},
		"parent_monitor_id": {strconv.FormatInt(myMonitor, 10)},
		"child_kind":        {"host"},
		"child_host_id":     {strconv.FormatInt(foreignHost, 10)},
	}
	resp := postForm(t, s.srv, path, form, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("save with foreign host status = %d, want 422", resp.StatusCode)
	}

	edges, err := s.h.AlertDeps.List(context.Background(), mine.ID)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(edges) != 0 {
		t.Fatalf("List(mine) after rejected cross-tenant save = %+v, want empty", edges)
	}
}

// TestWebAlertSuppressionPreview — Task 9b: экран реально рендерит текст
// dry-run предпросмотра «если бы <родитель> сейчас упал, подавились бы:
// <дети>» (а не просто не падает, как проверяют остальные тесты этого
// файла). Локаль стенда по умолчанию — ru (i18n.Default, нет
// Accept-Language в запросе, см. internal/i18n/match.go) — ожидаемый текст
// сверен с internal/i18n/locales/ru.json дословно.
func TestWebAlertSuppressionPreview(t *testing.T) {
	s := newStack(t)
	wireAlertSuppression(s)
	authSvc := auth.NewService(s.pool)
	orgSvc := org.NewService(s.pool, 1_000_000)

	ownerID, ownerCookie := orgSettingsRegister(t, authSvc, "dep-preview-owner@example.com")
	o, err := orgSvc.CreateOrg(context.Background(), "dep-preview-co", "Dep Preview Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	proj, err := orgSvc.CreateProject(context.Background(), o.ID, "dep-preview-proj", "Dep Preview Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	path := "/projects/" + strconv.FormatInt(proj.ID, 10) + "/alert-suppression"

	// Без единого ребра шаблон вообще не рендерит секцию предпросмотра
	// (AlertSuppression: `if len(edges) > 0 { @suppressionPreview(...) }`) —
	// проверяем это ДО того, как заводим первое ребро ниже.
	resp := getWithCookie(t, s.srv, path, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s (no edges) status = %d, want 200: %s", path, resp.StatusCode, body)
	}
	if strings.Contains(string(body), "preview-list") || strings.Contains(string(body), "Предпросмотр подавления") {
		t.Fatalf("GET %s (no edges) unexpectedly rendered preview section: %s", path, body)
	}

	// Explicit-ребро: монитор-родитель -> хост-ребёнок. Роль "app" (а не
	// дефолтная "web" из suppressionSeedHost) нарочно отличается от роли
	// в label-ребре ниже — иначе этот же хост попал бы ЕЩЁ и в
	// label-раскрытие role=web, и строку предпросмотра пришлось бы
	// сверять с двумя детьми вместо одного.
	monID := suppressionSeedMonitor(t, s, proj.ID, "ping-gw")
	hostChild := suppressionSeedHostRole(t, s, proj.ID, "web-child", "app")
	if _, err := s.h.AlertDeps.Create(context.Background(), depsuppress.Edge{
		ProjectID: proj.ID, ParentMonitorID: &monID, ChildHostID: &hostChild,
	}); err != nil {
		t.Fatalf("seed explicit edge: %v", err)
	}

	// Label-ребро: хост-родитель роли "lb" -> селектор role=web,
	// раскрывается в хост роли "web". Родитель и раскрываемая роль
	// нарочно разные: Store.Create сам отвергает ребро с ErrSelfMatch,
	// если бы родитель совпадал с собственным селектором (MAJOR-5) — то
	// исключение проверено на чистой функции в preview_test.go, здесь же
	// цель — конец-в-конец убедиться, что раскрытие label в реальный
	// найденный хост доходит до HTML.
	hostParent := suppressionSeedHostRole(t, s, proj.ID, "gw-parent", "lb")
	suppressionSeedHostRole(t, s, proj.ID, "web-sibling", "web")
	scope, value := "role", "web"
	if _, err := s.h.AlertDeps.Create(context.Background(), depsuppress.Edge{
		ProjectID: proj.ID, ParentHostID: &hostParent, ChildLabelScope: &scope, ChildLabelValue: &value,
	}); err != nil {
		t.Fatalf("seed label edge: %v", err)
	}

	resp = getWithCookie(t, s.srv, path, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s (with edges) status = %d, want 200: %s", path, resp.StatusCode, body)
	}
	bodyStr := string(body)

	// Explicit-ребро: точная строка предпросмотра целиком (родитель И
	// ребёнок в одном "если бы...подавились бы" блоке).
	wantExplicit := "Если бы Монитор: ping-gw сейчас упал, подавились бы: Хост: web-child"
	if !strings.Contains(bodyStr, wantExplicit) {
		t.Fatalf("GET %s missing explicit-edge preview row %q: %s", path, wantExplicit, bodyStr)
	}

	// Label-ребро: role=web раскрылось РОВНО в web-sibling (родитель
	// gw-parent — роли "lb", в раскрытие не попадает никак). Строка
	// целиком, а не раздельные Contains — фиксирует и парента, и
	// единственного раскрытого ребёнка в одном блоке.
	wantLabel := "Если бы Хост: gw-parent сейчас упал, подавились бы: Хост: web-sibling"
	if !strings.Contains(bodyStr, wantLabel) {
		t.Fatalf("GET %s missing label-edge preview row %q: %s", path, wantLabel, bodyStr)
	}
}

// TestWebAlertSuppressionNilService — h.AlertDeps не проведён (узкий
// тестовый стенд) -> 404, тот же nil-guard, что у escalationsPage/slosPage.
func TestWebAlertSuppressionNilService(t *testing.T) {
	s := newStack(t)
	authSvc := auth.NewService(s.pool)
	orgSvc := org.NewService(s.pool, 1_000_000)
	ownerID, ownerCookie := orgSettingsRegister(t, authSvc, "dep-nil-owner@example.com")
	o, _ := orgSvc.CreateOrg(context.Background(), "dep-nil-co", "Dep Nil Co", ownerID)
	proj, _ := orgSvc.CreateProject(context.Background(), o.ID, "dep-nil-proj", "Dep Nil Proj", "go")
	path := "/projects/" + strconv.FormatInt(proj.ID, 10) + "/alert-suppression"

	resp := getWithCookie(t, s.srv, path, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("nil AlertDeps status = %d, want 404", resp.StatusCode)
	}
}
