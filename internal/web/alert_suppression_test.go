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

// TestWebAlertSuppressionPage — owner (оператор проекта) видит свёрнутую
// справку, кнопку-триггер модалки добавления, список рёбер с резолвленными
// именами узлов, действиями «Редактировать»/«Удалить» и модалкой правки на
// строку; member без командного доступа к проекту — 404 (тот же
// existence-oracle, что и escalations).
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
	edges, err := s.h.AlertDeps.List(context.Background(), proj.ID)
	if err != nil || len(edges) != 1 {
		t.Fatalf("List = %+v / %v, want 1 edge", edges, err)
	}
	editModalID := "edit-suppression-edge-" + strconv.FormatInt(edges[0].ID, 10)
	for _, want := range []string{
		"ping-gw", "web-1", "parent_kind", "child_kind",
		// теория — в свёрнутой справке, не стеной хинтов на странице.
		`class="help-panel"`, `href="/docs/alert-suppression"`,
		// кнопка-триггер модалки добавления + сама модалка.
		`href="#new-suppression-edge"`, `id="new-suppression-edge"`,
		// класс формы — крючок CSS :has()-скрытия нерелевантных полей —
		// и классы самих скрываемых полей.
		`class="alert-suppression-form"`,
		`class="field as-parent-host"`, `class="field as-parent-monitor"`,
		`class="field as-child-host"`, `class="field as-child-monitor"`,
		`class="field as-child-label"`,
		// строка ребра: модалка правки со стабильным якорем по id ребра,
		// «Удалить» — кнопкой btn-danger, как на прочих страницах.
		`href="#` + editModalID + `"`, `id="` + editModalID + `"`,
		`class="btn btn-danger"`,
		// ритм секций: карточки списка и предпросмотра несут класс отступа.
		`class="card suppression-section"`,
	} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("GET %s missing %q: %s", path, want, body)
		}
	}
	// Класс-крючок CSS обязан стоять на КАЖДОЙ форме ребра: модалка создания
	// плюс модалка правки на строку (1 ребро → 2 формы). Contains нашёл бы и
	// одну из двух — форма без класса показывала бы все поля разом.
	if got := strings.Count(string(body), `class="alert-suppression-form"`); got != 2 {
		t.Fatalf("GET %s: %d forms with class alert-suppression-form, want 2 (create + 1 edit)", path, got)
	}
	// Стены вводных абзацев на самой странице больше нет: текст модели живёт
	// только внутри help-panel (сам текст присутствует — проверяем один из
	// ключей), а прежних четырёх <p class="hint"> подряд под <h1> нет.
	if !strings.Contains(string(body), "одно уведомление о корневой причине") {
		t.Fatalf("GET %s: intro text missing entirely (must live inside help panel): %s", path, body)
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

	resp = postForm(t, s.srv, path+"/1", url.Values{}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("nil AlertDeps update status = %d, want 404", resp.StatusCode)
	}
}

// suppressionSelectChunk вырезает из куска формы разметку одного <select>
// по его name: id хостов и мониторов — независимые последовательности, в
// свежей БД первый хост и первый монитор оба получают id=1, и Contains
// `value="1" selected` по всей форме матчился бы селектом РОДИТЕЛЯ-хоста,
// а не проверяемым селектом ребёнка.
func suppressionSelectChunk(t *testing.T, formChunk, selectName string) string {
	t.Helper()
	marker := `name="` + selectName + `"`
	start := strings.Index(formChunk, marker)
	if start < 0 {
		t.Fatalf("select %s not found in form chunk: %s", marker, formChunk)
	}
	end := strings.Index(formChunk[start:], "</select>")
	if end < 0 {
		t.Fatalf("select %s not closed: %s", marker, formChunk)
	}
	return formChunk[start : start+end]
}

// suppressionEditFormChunk вырезает из HTML кусок формы модалки правки
// конкретного ребра (от action до </form>): create-модалка на той же
// странице содержит те же поля, и Contains по всему телу проверял бы не ту
// форму.
func suppressionEditFormChunk(t *testing.T, body string, projectID, depID int64) string {
	t.Helper()
	action := `action="/projects/` + strconv.FormatInt(projectID, 10) + `/alert-suppression/` + strconv.FormatInt(depID, 10) + `"`
	start := strings.Index(body, action)
	if start < 0 {
		t.Fatalf("edit form %s not found in body: %s", action, body)
	}
	end := strings.Index(body[start:], "</form>")
	if end < 0 {
		t.Fatalf("edit form %s not closed: %s", action, body)
	}
	return body[start : start+end]
}

// TestWebAlertSuppressionUpdate — POST /projects/{id}/alert-suppression/{depID}
// меняет содержимое ребра (303), id ребра остаётся прежним; модалка правки
// на GET предзаполнена значениями самого ребра (radio checked + option
// selected).
func TestWebAlertSuppressionUpdate(t *testing.T) {
	s := newStack(t)
	wireAlertSuppression(s)
	authSvc := auth.NewService(s.pool)
	orgSvc := org.NewService(s.pool, 1_000_000)

	ownerID, ownerCookie := orgSettingsRegister(t, authSvc, "dep-upd-owner@example.com")
	o, err := orgSvc.CreateOrg(context.Background(), "dep-upd-co", "Dep Upd Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	proj, err := orgSvc.CreateProject(context.Background(), o.ID, "dep-upd-proj", "Dep Upd Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	hostID := suppressionSeedHost(t, s, proj.ID, "web-1")
	mon1 := suppressionSeedMonitor(t, s, proj.ID, "ping-gw")
	mon2 := suppressionSeedMonitor(t, s, proj.ID, "ping-db")

	depID, err := s.h.AlertDeps.Create(context.Background(), depsuppress.Edge{
		ProjectID: proj.ID, ParentHostID: &hostID, ChildMonitorID: &mon1,
	})
	if err != nil {
		t.Fatalf("seed edge: %v", err)
	}
	path := "/projects/" + strconv.FormatInt(proj.ID, 10) + "/alert-suppression"

	// Предзаполнение модалки правки — из самого ребра.
	resp := getWithCookie(t, s.srv, path, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200: %s", path, resp.StatusCode, body)
	}
	chunk := suppressionEditFormChunk(t, string(body), proj.ID, depID)
	if sel := suppressionSelectChunk(t, chunk, "parent_host_id"); !strings.Contains(sel, `value="`+strconv.FormatInt(hostID, 10)+`" selected`) {
		t.Fatalf("edit modal parent host %d not selected: %s", hostID, sel)
	}
	if sel := suppressionSelectChunk(t, chunk, "child_monitor_id"); !strings.Contains(sel, `value="`+strconv.FormatInt(mon1, 10)+`" selected`) {
		t.Fatalf("edit modal child monitor %d not selected: %s", mon1, sel)
	}
	for _, want := range []string{
		`name="parent_kind" value="host" checked`,
		`name="child_kind" value="monitor" checked`,
		// честная подсказка про пересчёт: правка действует на новые решения
		// о подавлении, уже подавленные открытые инциденты не пересчитываются
		// (флаг suppressed_by_dep одноразовый — см. depsuppress.Store.Update).
		"Правка действует на новые решения о подавлении",
	} {
		if !strings.Contains(chunk, want) {
			t.Fatalf("edit modal prefill missing %q: %s", want, chunk)
		}
	}

	// Правка: ребёнок mon1 → mon2.
	form := url.Values{
		"parent_kind":      {"host"},
		"parent_host_id":   {strconv.FormatInt(hostID, 10)},
		"child_kind":       {"monitor"},
		"child_monitor_id": {strconv.FormatInt(mon2, 10)},
	}
	resp = postForm(t, s.srv, path+"/"+strconv.FormatInt(depID, 10), form, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("update edge status = %d, want 303", resp.StatusCode)
	}

	edges, err := s.h.AlertDeps.List(context.Background(), proj.ID)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(edges) != 1 || edges[0].ID != depID ||
		edges[0].ChildMonitorID == nil || *edges[0].ChildMonitorID != mon2 {
		t.Fatalf("List after update = %+v, want edge id=%d host(%d) -> monitor(%d)", edges, depID, hostID, mon2)
	}
}

// TestWebAlertSuppressionUpdate422Reopen — 422 правки (дубликат другого
// ребра) переоткрывает модалку ИМЕННО этого ребра с введёнными значениями;
// модалка создания и модалки прочих рёбер остаются закрытыми.
func TestWebAlertSuppressionUpdate422Reopen(t *testing.T) {
	s := newStack(t)
	wireAlertSuppression(s)
	authSvc := auth.NewService(s.pool)
	orgSvc := org.NewService(s.pool, 1_000_000)

	ownerID, ownerCookie := orgSettingsRegister(t, authSvc, "dep-reopen-owner@example.com")
	o, err := orgSvc.CreateOrg(context.Background(), "dep-reopen-co", "Dep Reopen Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	proj, err := orgSvc.CreateProject(context.Background(), o.ID, "dep-reopen-proj", "Dep Reopen Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	hostID := suppressionSeedHost(t, s, proj.ID, "web-1")
	mon1 := suppressionSeedMonitor(t, s, proj.ID, "ping-gw")
	mon2 := suppressionSeedMonitor(t, s, proj.ID, "ping-db")

	e1, err := s.h.AlertDeps.Create(context.Background(), depsuppress.Edge{
		ProjectID: proj.ID, ParentHostID: &hostID, ChildMonitorID: &mon1,
	})
	if err != nil {
		t.Fatalf("seed e1: %v", err)
	}
	e2, err := s.h.AlertDeps.Create(context.Background(), depsuppress.Edge{
		ProjectID: proj.ID, ParentHostID: &hostID, ChildMonitorID: &mon2,
	})
	if err != nil {
		t.Fatalf("seed e2: %v", err)
	}

	path := "/projects/" + strconv.FormatInt(proj.ID, 10) + "/alert-suppression"
	// e2 правим в точную копию e1 → ErrDuplicate → 422.
	form := url.Values{
		"parent_kind":      {"host"},
		"parent_host_id":   {strconv.FormatInt(hostID, 10)},
		"child_kind":       {"monitor"},
		"child_monitor_id": {strconv.FormatInt(mon1, 10)},
	}
	resp := postForm(t, s.srv, path+"/"+strconv.FormatInt(e2, 10), form, s.srv.URL, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("duplicate update status = %d, want 422: %s", resp.StatusCode, body)
	}
	bodyStr := string(body)

	openID := "edit-suppression-edge-" + strconv.FormatInt(e2, 10)
	if !strings.Contains(bodyStr, `id="`+openID+`" class="modal modal--open"`) {
		t.Fatalf("422 must reopen edit modal %s: %s", openID, bodyStr)
	}
	if got := strings.Count(bodyStr, "modal--open"); got != 1 {
		t.Fatalf("want exactly 1 open modal (edit %s), got %d: %s", openID, got, bodyStr)
	}
	closedID := "edit-suppression-edge-" + strconv.FormatInt(e1, 10)
	if !strings.Contains(bodyStr, `id="`+closedID+`" class="modal"`) ||
		!strings.Contains(bodyStr, `id="new-suppression-edge" class="modal"`) {
		t.Fatalf("other modals must stay closed: %s", bodyStr)
	}
	// Введённое сохранено именно в переоткрытой модалке: выбран mon1
	// (введённый дубликат), а не mon2 (текущее значение ребра e2); рядом —
	// текст доменной ошибки.
	chunk := suppressionEditFormChunk(t, bodyStr, proj.ID, e2)
	if sel := suppressionSelectChunk(t, chunk, "child_monitor_id"); !strings.Contains(sel, `value="`+strconv.FormatInt(mon1, 10)+`" selected`) {
		t.Fatalf("reopened modal must keep entered child monitor %d: %s", mon1, sel)
	}
	if !strings.Contains(chunk, "Точно такое же ребро зависимости уже существует") {
		t.Fatalf("reopened modal must show duplicate error inside: %s", chunk)
	}
}

// TestWebAlertSuppressionCreate422ReopensCreateModal — 422 создания
// переоткрывает модалку создания с введёнными значениями (метка label-ребра
// сохраняется в поле), модалки правки остаются закрытыми.
func TestWebAlertSuppressionCreate422ReopensCreateModal(t *testing.T) {
	s := newStack(t)
	wireAlertSuppression(s)
	authSvc := auth.NewService(s.pool)
	orgSvc := org.NewService(s.pool, 1_000_000)

	ownerID, ownerCookie := orgSettingsRegister(t, authSvc, "dep-c422-owner@example.com")
	o, err := orgSvc.CreateOrg(context.Background(), "dep-c422-co", "Dep C422 Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	proj, err := orgSvc.CreateProject(context.Background(), o.ID, "dep-c422-proj", "Dep C422 Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	// Хост роли web + label-ребро на роль db существует; повтор той же формы
	// из модалки создания — ErrDuplicate → 422.
	hostID := suppressionSeedHost(t, s, proj.ID, "web-1")
	scope, value := "role", "db"
	if _, err := s.h.AlertDeps.Create(context.Background(), depsuppress.Edge{
		ProjectID: proj.ID, ParentHostID: &hostID, ChildLabelScope: &scope, ChildLabelValue: &value,
	}); err != nil {
		t.Fatalf("seed label edge: %v", err)
	}

	path := "/projects/" + strconv.FormatInt(proj.ID, 10) + "/alert-suppression"
	form := url.Values{
		"parent_kind":       {"host"},
		"parent_host_id":    {strconv.FormatInt(hostID, 10)},
		"child_kind":        {"label"},
		"child_label_scope": {"role"},
		"child_label_value": {"db"},
	}
	resp := postForm(t, s.srv, path, form, s.srv.URL, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("duplicate create status = %d, want 422: %s", resp.StatusCode, body)
	}
	bodyStr := string(body)
	if !strings.Contains(bodyStr, `id="new-suppression-edge" class="modal modal--open"`) {
		t.Fatalf("422 must reopen create modal: %s", bodyStr)
	}
	if got := strings.Count(bodyStr, "modal--open"); got != 1 {
		t.Fatalf("want exactly 1 open modal (create), got %d: %s", got, bodyStr)
	}
	// Введённые значения сохранены: radio label выбран, значение метки в поле.
	start := strings.Index(bodyStr, `id="new-suppression-edge" class="modal modal--open"`)
	end := strings.Index(bodyStr[start:], "</form>")
	if end < 0 {
		t.Fatalf("create modal form not closed: %s", bodyStr)
	}
	chunk := bodyStr[start : start+end]
	for _, want := range []string{
		`name="child_kind" value="label" checked`,
		`name="child_label_value" class="input" value="db"`,
	} {
		if !strings.Contains(chunk, want) {
			t.Fatalf("reopened create modal missing %q: %s", want, chunk)
		}
	}
}

// TestWebAlertSuppressionUpdateCrossTenant — правка чужого ребра не проходит:
// depID другого проекта той же организации — 404 (Store.Update скоупит по
// project_id, ErrNotFound), проект другой организации — 404 existence-oracle
// requireProjectOperator. Чужое ребро в обоих случаях остаётся нетронутым.
func TestWebAlertSuppressionUpdateCrossTenant(t *testing.T) {
	s := newStack(t)
	wireAlertSuppression(s)
	authSvc := auth.NewService(s.pool)
	orgSvc := org.NewService(s.pool, 1_000_000)

	ownerID, ownerCookie := orgSettingsRegister(t, authSvc, "dep-uct-owner@example.com")
	o, err := orgSvc.CreateOrg(context.Background(), "dep-uct-co", "Dep UCT Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	mine, err := orgSvc.CreateProject(context.Background(), o.ID, "dep-uct-mine", "Mine", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	theirs, err := orgSvc.CreateProject(context.Background(), o.ID, "dep-uct-theirs", "Theirs", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	myHost := suppressionSeedHost(t, s, mine.ID, "my-host")
	myMon := suppressionSeedMonitor(t, s, mine.ID, "my-mon")
	theirHost := suppressionSeedHost(t, s, theirs.ID, "their-host")
	theirMon := suppressionSeedMonitor(t, s, theirs.ID, "their-mon")

	theirEdge, err := s.h.AlertDeps.Create(context.Background(), depsuppress.Edge{
		ProjectID: theirs.ID, ParentHostID: &theirHost, ChildMonitorID: &theirMon,
	})
	if err != nil {
		t.Fatalf("seed their edge: %v", err)
	}

	form := url.Values{
		"parent_kind":      {"host"},
		"parent_host_id":   {strconv.FormatInt(myHost, 10)},
		"child_kind":       {"monitor"},
		"child_monitor_id": {strconv.FormatInt(myMon, 10)},
	}
	// depID чужого проекта под МОИМ /projects/{id} — 404 от ErrNotFound.
	minePath := "/projects/" + strconv.FormatInt(mine.ID, 10) + "/alert-suppression/" + strconv.FormatInt(theirEdge, 10)
	resp := postForm(t, s.srv, minePath, form, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("update foreign depID status = %d, want 404", resp.StatusCode)
	}

	// Чужая организация: пользователь без доступа к проекту theirs — 404 от
	// requireProjectOperator, existence-oracle.
	_, strangerCookie := orgSettingsRegister(t, authSvc, "dep-uct-stranger@example.com")
	theirsPath := "/projects/" + strconv.FormatInt(theirs.ID, 10) + "/alert-suppression/" + strconv.FormatInt(theirEdge, 10)
	resp = postForm(t, s.srv, theirsPath, form, s.srv.URL, strangerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("update as stranger status = %d, want 404", resp.StatusCode)
	}

	edges, err := s.h.AlertDeps.List(context.Background(), theirs.ID)
	if err != nil || len(edges) != 1 || edges[0].ID != theirEdge ||
		edges[0].ParentHostID == nil || *edges[0].ParentHostID != theirHost ||
		edges[0].ChildMonitorID == nil || *edges[0].ChildMonitorID != theirMon {
		t.Fatalf("their edge after rejected updates = %+v / %v, want untouched", edges, err)
	}
}

// TestWebAlertSuppressionNoDuplicateIDs — модалка правки на каждую строку
// плюс модалка создания: все id="" документа обязаны быть уникальны (якоря
// CSS :target перестают работать при дублях, aria-labelledby — тоже).
func TestWebAlertSuppressionNoDuplicateIDs(t *testing.T) {
	s := newStack(t)
	wireAlertSuppression(s)
	authSvc := auth.NewService(s.pool)
	orgSvc := org.NewService(s.pool, 1_000_000)

	ownerID, ownerCookie := orgSettingsRegister(t, authSvc, "dep-ids-owner@example.com")
	o, err := orgSvc.CreateOrg(context.Background(), "dep-ids-co", "Dep IDs Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	proj, err := orgSvc.CreateProject(context.Background(), o.ID, "dep-ids-proj", "Dep IDs Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	hostID := suppressionSeedHost(t, s, proj.ID, "web-1")
	mon1 := suppressionSeedMonitor(t, s, proj.ID, "ping-gw")
	mon2 := suppressionSeedMonitor(t, s, proj.ID, "ping-db")
	for _, mon := range []int64{mon1, mon2} {
		if _, err := s.h.AlertDeps.Create(context.Background(), depsuppress.Edge{
			ProjectID: proj.ID, ParentHostID: &hostID, ChildMonitorID: &mon,
		}); err != nil {
			t.Fatalf("seed edge: %v", err)
		}
	}

	path := "/projects/" + strconv.FormatInt(proj.ID, 10) + "/alert-suppression"
	resp := getWithCookie(t, s.srv, path, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200: %s", path, resp.StatusCode, body)
	}

	seen := map[string]bool{}
	for _, m := range regexp.MustCompile(`\sid="([^"]+)"`).FindAllStringSubmatch(string(body), -1) {
		if seen[m[1]] {
			t.Fatalf("duplicate id=%q in document: %s", m[1], body)
		}
		seen[m[1]] = true
	}
	// Санити: модалки правки обоих рёбер действительно в документе.
	edges, err := s.h.AlertDeps.List(context.Background(), proj.ID)
	if err != nil || len(edges) != 2 {
		t.Fatalf("List = %+v / %v, want 2 edges", edges, err)
	}
	for _, e := range edges {
		if !seen["edit-suppression-edge-"+strconv.FormatInt(e.ID, 10)] {
			t.Fatalf("edit modal id for edge %d not found among ids %v", e.ID, seen)
		}
	}
}
