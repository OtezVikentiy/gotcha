package web_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/logfilter"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
)

// newFiltersStack — logsStack (logs_test.go) с проведённым LogFilters:
// newLogsStack сам его не заводит (как и LogQuery без wireLogQuery=true),
// сохранённые фильтры логов — отдельная опциональная зависимость Handler.
func newFiltersStack(t *testing.T) *logsStack {
	t.Helper()
	s := newLogsStack(t, true)
	s.h.LogFilters = logfilter.NewStore(s.pool)
	return s
}

// addProjectMember делает userID участником организации orgID (role=member)
// И даёт ему доступ к projectID через персональную команду
// (CreateTeam+AddTeamMember+AttachTeam) — тот же приём, что у актора
// «оператор (team-attached, role=member)» в authz_behavior_test.go:407.
// Голого org.AddMember(...RoleMember) НЕДОСТАТОЧНО: org.CanAccessProject
// (org/project.go, accessCondition) даёт доступ либо owner/admin
// организации, либо участнику команды, прикреплённой к проекту — рядовой
// member без команды не видит сам /logs, не то что его фильтры (это и
// проверяет TestPersonalFilterOfAnotherUserIsInvisible: даже владелец
// проекта — team-attachment ему не нужен, он проходит по первой ветви
// accessCondition — не видит чужой личный фильтр).
func addProjectMember(t *testing.T, s *logsStack, orgID, projectID, userID int64) {
	t.Helper()
	ctx := context.Background()
	if err := s.org.AddMember(ctx, orgID, userID, org.RoleMember); err != nil {
		t.Fatalf("add member: %v", err)
	}
	team, err := s.org.CreateTeam(ctx, orgID, fmt.Sprintf("team-%d", userID), fmt.Sprintf("team-%d", userID))
	if err != nil {
		t.Fatalf("create team: %v", err)
	}
	if err := s.org.AddTeamMember(ctx, team.ID, userID); err != nil {
		t.Fatalf("add team member: %v", err)
	}
	if err := s.org.AttachTeam(ctx, projectID, team.ID); err != nil {
		t.Fatalf("attach team: %v", err)
	}
}

// lastFilterID/countFilters/filterShared — прямые обёртки над pool.QueryRow
// по log_saved_filters, как просит бриф задачи 9: тестам нужно проверять
// состояние хранилища, которое хендлеры не всегда отражают в ответе (счётчик
// после отказа, факт понижения/повышения фильтра).
func lastFilterID(t *testing.T, pool *pgxpool.Pool, projectID int64) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(),
		"SELECT max(id) FROM log_saved_filters WHERE project_id = $1", projectID).Scan(&id); err != nil {
		t.Fatalf("last filter id: %v", err)
	}
	return id
}

func countFilters(t *testing.T, pool *pgxpool.Pool, projectID int64) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		"SELECT count(*) FROM log_saved_filters WHERE project_id = $1", projectID).Scan(&n); err != nil {
		t.Fatalf("count filters: %v", err)
	}
	return n
}

func filterShared(t *testing.T, pool *pgxpool.Pool, filterID int64) bool {
	t.Helper()
	var shared bool
	if err := pool.QueryRow(context.Background(),
		"SELECT owner_user_id IS NULL FROM log_saved_filters WHERE id = $1", filterID).Scan(&shared); err != nil {
		t.Fatalf("filter shared: %v", err)
	}
	return shared
}

// TestLogFiltersCreatePersonal — счастливый путь личного фильтра: создание,
// появление в панели «Мои», ссылка применения разворачивает условие в
// обычный query-параметр (не прячет его за идентификатором).
func TestLogFiltersCreatePersonal(t *testing.T) {
	s := newFiltersStack(t)
	_, ownerCookie, project := newLogsProject(t, s, "create-personal@example.com", "cp-org", "cp-proj")
	projectID := project.ID

	resp := postForm(t, s.srv, logsBasePath(projectID)+"/filters",
		url.Values{"name": {"мои ошибки"}, "q_not": {"buffered"}}, s.srv.URL, ownerCookie)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("создание личного фильтра: статус %d: %s", resp.StatusCode, body)
	}
	if loc := resp.Header.Get("Location"); !strings.Contains(loc, logsBasePath(projectID)) {
		t.Errorf("редирект уводит не на страницу логов: %q", loc)
	}

	page := getWithCookie(t, s.srv, logsBasePath(projectID), ownerCookie)
	defer page.Body.Close()
	body, _ := io.ReadAll(page.Body)
	text := string(body)
	if !strings.Contains(text, "мои ошибки") {
		t.Errorf("сохранённый фильтр не отображается в панели: %s", text)
	}
	if !strings.Contains(text, "q_not=buffered") {
		t.Errorf("ссылка применения не разворачивает условие фильтра в параметр: %s", text)
	}
}

// TestLogFiltersCreateSharedVisibleToMembers — общий фильтр, заведённый
// оператором, виден рядовому участнику проекта в панели «Общие».
func TestLogFiltersCreateSharedVisibleToMembers(t *testing.T) {
	s := newFiltersStack(t)
	_, ownerCookie, project := newLogsProject(t, s, "create-shared@example.com", "cs-org", "cs-proj")
	projectID := project.ID
	memberID, memberCookie := orgSettingsRegister(t, s.auth, "cs-member@example.com")
	addProjectMember(t, s, project.OrgID, projectID, memberID)

	resp := postForm(t, s.srv, logsBasePath(projectID)+"/filters",
		url.Values{"name": {"общий вид"}, "environment": {"staging"}, "shared": {"1"}}, s.srv.URL, ownerCookie)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("создание общего фильтра оператором: статус %d: %s", resp.StatusCode, body)
	}

	page := getWithCookie(t, s.srv, logsBasePath(projectID), memberCookie)
	defer page.Body.Close()
	body, _ := io.ReadAll(page.Body)
	text := string(body)
	if !strings.Contains(text, "общий вид") {
		t.Errorf("общий фильтр не виден рядовому участнику: %s", text)
	}
	if !strings.Contains(text, "environment=staging") {
		t.Errorf("ссылка применения общего фильтра не содержит условие: %s", text)
	}
}

// TestSharedFilterRequiresOperator — вторичный гейт создания: рядовой
// участник не может завести общий фильтр (403), оператор — может (303).
func TestSharedFilterRequiresOperator(t *testing.T) {
	s := newFiltersStack(t)
	_, ownerCookie, project := newLogsProject(t, s, "gate-owner@example.com", "gate-org", "gate-proj")
	projectID := project.ID

	memberID, memberCookie := orgSettingsRegister(t, s.auth, "gate-member@example.com")
	addProjectMember(t, s, project.OrgID, projectID, memberID)

	form := url.Values{
		"name":   {"без шума"},
		"shared": {"1"},
		"q_not":  {"buffered"},
	}
	path := logsBasePath(projectID) + "/filters"

	resp := postForm(t, s.srv, path, form, s.srv.URL, memberCookie)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("рядовой участник создал общий фильтр: статус %d", resp.StatusCode)
	}

	resp2 := postForm(t, s.srv, path, form, s.srv.URL, ownerCookie)
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusSeeOther {
		t.Fatalf("владелец не смог создать общий фильтр: статус %d", resp2.StatusCode)
	}
}

// TestPersonalFilterOfAnotherUserIsInvisible — чужой личный фильтр не
// отдаётся никому: ни в списке (даже владельцу проекта), ни по прямому
// идентификатору (404, не 403 — существование не подтверждаем).
func TestPersonalFilterOfAnotherUserIsInvisible(t *testing.T) {
	s := newFiltersStack(t)
	_, ownerCookie, project := newLogsProject(t, s, "owner2@example.com", "priv-org", "priv-proj")
	projectID := project.ID

	aliceID, aliceCookie := orgSettingsRegister(t, s.auth, "alice@example.com")
	addProjectMember(t, s, project.OrgID, projectID, aliceID)

	create := postForm(t, s.srv, logsBasePath(projectID)+"/filters",
		url.Values{"name": {"личный алисы"}, "q_not": {"buffered"}}, s.srv.URL, aliceCookie)
	create.Body.Close()
	if create.StatusCode != http.StatusSeeOther {
		t.Fatalf("алиса не смогла создать личный фильтр: статус %d", create.StatusCode)
	}

	filterID := lastFilterID(t, s.pool, projectID)

	// Владелец проекта — тоже посторонний для чужого личного фильтра.
	page := getWithCookie(t, s.srv, logsBasePath(projectID), ownerCookie)
	defer page.Body.Close()
	body, err := io.ReadAll(page.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if strings.Contains(string(body), "личный алисы") {
		t.Errorf("чужой личный фильтр виден владельцу проекта")
	}

	for _, action := range []string{"/update", "/delete", "/default"} {
		path := fmt.Sprintf("%s/filters/%d%s", logsBasePath(projectID), filterID, action)
		resp := postForm(t, s.srv, path, url.Values{"name": {"чужое"}}, s.srv.URL, ownerCookie)
		resp.Body.Close()
		// 404, а не 403: существование чужого личного фильтра не подтверждаем
		// (тот же приём, что в internal/web/exports.go:309).
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s на чужой личный фильтр вернул %d, ожидался 404", action, resp.StatusCode)
		}
	}
}

// TestLogFiltersUpdatePersonalRoundTrip — владелец меняет условия своего
// личного фильтра; проверяется, что Update ЗАМЕЩАЕТ старые условия, а не
// добавляет к ним (иначе мутация «забыли передать новые предикаты» осталась
// бы незамеченной).
func TestLogFiltersUpdatePersonalRoundTrip(t *testing.T) {
	s := newFiltersStack(t)
	_, ownerCookie, project := newLogsProject(t, s, "update-personal@example.com", "up-org", "up-proj")
	projectID := project.ID

	create := postForm(t, s.srv, logsBasePath(projectID)+"/filters",
		url.Values{"name": {"черновик"}, "q_not": {"buffered"}}, s.srv.URL, ownerCookie)
	create.Body.Close()
	if create.StatusCode != http.StatusSeeOther {
		t.Fatalf("создание: статус %d", create.StatusCode)
	}
	filterID := lastFilterID(t, s.pool, projectID)

	upd := postForm(t, s.srv, fmt.Sprintf("%s/filters/%d/update", logsBasePath(projectID), filterID),
		url.Values{"name": {"черновик"}, "environment": {"production"}}, s.srv.URL, ownerCookie)
	defer upd.Body.Close()
	if upd.StatusCode != http.StatusSeeOther {
		body, _ := io.ReadAll(upd.Body)
		t.Fatalf("обновление: статус %d: %s", upd.StatusCode, body)
	}

	page := getWithCookie(t, s.srv, logsBasePath(projectID), ownerCookie)
	defer page.Body.Close()
	body, _ := io.ReadAll(page.Body)
	text := string(body)
	if strings.Contains(text, "q_not=buffered") {
		t.Errorf("обновление не заменило старые условия: %s", text)
	}
	if !strings.Contains(text, "environment=production") {
		t.Errorf("обновление не применило новые условия: %s", text)
	}
}

// TestLogFiltersUpdateSharedRequiresOperator — правка УЖЕ общего фильтра
// требует оператора, даже когда результат остаётся общим.
func TestLogFiltersUpdateSharedRequiresOperator(t *testing.T) {
	s := newFiltersStack(t)
	_, ownerCookie, project := newLogsProject(t, s, "update-shared@example.com", "us-org", "us-proj")
	projectID := project.ID
	memberID, memberCookie := orgSettingsRegister(t, s.auth, "us-member@example.com")
	addProjectMember(t, s, project.OrgID, projectID, memberID)

	create := postForm(t, s.srv, logsBasePath(projectID)+"/filters",
		url.Values{"name": {"общий"}, "shared": {"1"}, "q_not": {"buffered"}}, s.srv.URL, ownerCookie)
	create.Body.Close()
	if create.StatusCode != http.StatusSeeOther {
		t.Fatalf("создание общего: статус %d", create.StatusCode)
	}
	filterID := lastFilterID(t, s.pool, projectID)
	path := fmt.Sprintf("%s/filters/%d/update", logsBasePath(projectID), filterID)

	forbidden := postForm(t, s.srv, path, url.Values{"name": {"общий"}, "shared": {"1"}, "environment": {"staging"}}, s.srv.URL, memberCookie)
	defer forbidden.Body.Close()
	if forbidden.StatusCode != http.StatusForbidden {
		t.Fatalf("рядовой участник изменил общий фильтр: статус %d", forbidden.StatusCode)
	}

	ok := postForm(t, s.srv, path, url.Values{"name": {"общий"}, "shared": {"1"}, "environment": {"staging"}}, s.srv.URL, ownerCookie)
	defer ok.Body.Close()
	if ok.StatusCode != http.StatusSeeOther {
		t.Fatalf("оператор не смог изменить общий фильтр: статус %d", ok.StatusCode)
	}
}

// TestLogFiltersConvertPersonalToSharedRequiresOperator — рядовой участник
// не может повысить СВОЙ ЖЕ личный фильтр до общего в обход гейта оператора.
func TestLogFiltersConvertPersonalToSharedRequiresOperator(t *testing.T) {
	s := newFiltersStack(t)
	_, _, project := newLogsProject(t, s, "convert@example.com", "cv-org", "cv-proj")
	projectID := project.ID
	memberID, memberCookie := orgSettingsRegister(t, s.auth, "cv-member@example.com")
	addProjectMember(t, s, project.OrgID, projectID, memberID)

	create := postForm(t, s.srv, logsBasePath(projectID)+"/filters",
		url.Values{"name": {"личный участника"}, "q_not": {"buffered"}}, s.srv.URL, memberCookie)
	create.Body.Close()
	if create.StatusCode != http.StatusSeeOther {
		t.Fatalf("создание личного: статус %d", create.StatusCode)
	}
	filterID := lastFilterID(t, s.pool, projectID)

	path := fmt.Sprintf("%s/filters/%d/update", logsBasePath(projectID), filterID)
	forbidden := postForm(t, s.srv, path,
		url.Values{"name": {"личный участника"}, "shared": {"1"}, "q_not": {"buffered"}}, s.srv.URL, memberCookie)
	defer forbidden.Body.Close()
	if forbidden.StatusCode != http.StatusForbidden {
		t.Fatalf("рядовой участник повысил свой личный фильтр до общего: статус %d", forbidden.StatusCode)
	}
	if filterShared(t, s.pool, filterID) {
		t.Errorf("фильтр стал общим без права оператора")
	}
}

// TestLogFiltersOwnerConvertsPersonalToShared — оператор (в т.ч. владелец
// проекта) может повысить СВОЙ личный фильтр до общего; после этого фильтр
// виден рядовым участникам.
func TestLogFiltersOwnerConvertsPersonalToShared(t *testing.T) {
	s := newFiltersStack(t)
	_, ownerCookie, project := newLogsProject(t, s, "convert-ok@example.com", "cvo-org", "cvo-proj")
	projectID := project.ID
	memberID, memberCookie := orgSettingsRegister(t, s.auth, "cvo-member@example.com")
	addProjectMember(t, s, project.OrgID, projectID, memberID)

	create := postForm(t, s.srv, logsBasePath(projectID)+"/filters",
		url.Values{"name": {"станет общим"}, "q_not": {"buffered"}}, s.srv.URL, ownerCookie)
	create.Body.Close()
	if create.StatusCode != http.StatusSeeOther {
		t.Fatalf("создание личного: статус %d", create.StatusCode)
	}
	filterID := lastFilterID(t, s.pool, projectID)

	upd := postForm(t, s.srv, fmt.Sprintf("%s/filters/%d/update", logsBasePath(projectID), filterID),
		url.Values{"name": {"станет общим"}, "shared": {"1"}, "q_not": {"buffered"}}, s.srv.URL, ownerCookie)
	defer upd.Body.Close()
	if upd.StatusCode != http.StatusSeeOther {
		body, _ := io.ReadAll(upd.Body)
		t.Fatalf("владелец не смог повысить свой фильтр до общего: статус %d: %s", upd.StatusCode, body)
	}
	if !filterShared(t, s.pool, filterID) {
		t.Errorf("фильтр не стал общим после обновления")
	}

	page := getWithCookie(t, s.srv, logsBasePath(projectID), memberCookie)
	defer page.Body.Close()
	body, _ := io.ReadAll(page.Body)
	if !strings.Contains(string(body), "станет общим") {
		t.Errorf("повышенный фильтр не виден рядовому участнику: %s", body)
	}
}

// TestLogFiltersDeleteOwnPersonal — владелец удаляет свой личный фильтр.
func TestLogFiltersDeleteOwnPersonal(t *testing.T) {
	s := newFiltersStack(t)
	_, ownerCookie, project := newLogsProject(t, s, "delete-personal@example.com", "dp-org", "dp-proj")
	projectID := project.ID

	create := postForm(t, s.srv, logsBasePath(projectID)+"/filters",
		url.Values{"name": {"на удаление"}, "q_not": {"buffered"}}, s.srv.URL, ownerCookie)
	create.Body.Close()
	if create.StatusCode != http.StatusSeeOther {
		t.Fatalf("создание: статус %d", create.StatusCode)
	}
	filterID := lastFilterID(t, s.pool, projectID)
	before := countFilters(t, s.pool, projectID)

	del := postForm(t, s.srv, fmt.Sprintf("%s/filters/%d/delete", logsBasePath(projectID), filterID),
		url.Values{}, s.srv.URL, ownerCookie)
	defer del.Body.Close()
	if del.StatusCode != http.StatusSeeOther {
		t.Fatalf("удаление личного фильтра владельцем: статус %d", del.StatusCode)
	}
	if after := countFilters(t, s.pool, projectID); after != before-1 {
		t.Errorf("фильтр не удалён: было %d, стало %d", before, after)
	}
}

// TestLogFiltersDeleteSharedRequiresOperator — удаление общего фильтра
// требует оператора; рядовой участник получает отказ, и фильтр остаётся на
// месте (гейт ДО удаления, а не после).
func TestLogFiltersDeleteSharedRequiresOperator(t *testing.T) {
	s := newFiltersStack(t)
	_, ownerCookie, project := newLogsProject(t, s, "delete-shared@example.com", "ds-org", "ds-proj")
	projectID := project.ID
	memberID, memberCookie := orgSettingsRegister(t, s.auth, "ds-member@example.com")
	addProjectMember(t, s, project.OrgID, projectID, memberID)

	create := postForm(t, s.srv, logsBasePath(projectID)+"/filters",
		url.Values{"name": {"общий на удаление"}, "shared": {"1"}, "q_not": {"buffered"}}, s.srv.URL, ownerCookie)
	create.Body.Close()
	if create.StatusCode != http.StatusSeeOther {
		t.Fatalf("создание общего: статус %d", create.StatusCode)
	}
	filterID := lastFilterID(t, s.pool, projectID)
	before := countFilters(t, s.pool, projectID)
	path := fmt.Sprintf("%s/filters/%d/delete", logsBasePath(projectID), filterID)

	forbidden := postForm(t, s.srv, path, url.Values{}, s.srv.URL, memberCookie)
	defer forbidden.Body.Close()
	if forbidden.StatusCode != http.StatusForbidden {
		t.Fatalf("рядовой участник удалил общий фильтр: статус %d", forbidden.StatusCode)
	}
	if after := countFilters(t, s.pool, projectID); after != before {
		t.Errorf("общий фильтр исчез после отказа: было %d, стало %d", before, after)
	}

	ok := postForm(t, s.srv, path, url.Values{}, s.srv.URL, ownerCookie)
	defer ok.Body.Close()
	if ok.StatusCode != http.StatusSeeOther {
		t.Fatalf("оператор не смог удалить общий фильтр: статус %d", ok.StatusCode)
	}
	if after := countFilters(t, s.pool, projectID); after != before-1 {
		t.Errorf("удаление оператором не сработало: было %d, стало %d", before, after)
	}
}

// TestLogFiltersSetDefault — бейдж умолчания появляется после назначения
// и отсутствует до него.
func TestLogFiltersSetDefault(t *testing.T) {
	s := newFiltersStack(t)
	_, ownerCookie, project := newLogsProject(t, s, "default@example.com", "def-org", "def-proj")
	projectID := project.ID

	create := postForm(t, s.srv, logsBasePath(projectID)+"/filters",
		url.Values{"name": {"избранный"}, "q_not": {"buffered"}}, s.srv.URL, ownerCookie)
	create.Body.Close()
	if create.StatusCode != http.StatusSeeOther {
		t.Fatalf("создание: статус %d", create.StatusCode)
	}
	filterID := lastFilterID(t, s.pool, projectID)

	before := getWithCookie(t, s.srv, logsBasePath(projectID), ownerCookie)
	beforeBody, _ := io.ReadAll(before.Body)
	before.Body.Close()
	if strings.Contains(string(beforeBody), "по умолчанию") {
		t.Fatalf("бейдж умолчания появился раньше времени: %s", beforeBody)
	}

	def := postForm(t, s.srv, fmt.Sprintf("%s/filters/%d/default", logsBasePath(projectID), filterID),
		url.Values{}, s.srv.URL, ownerCookie)
	defer def.Body.Close()
	if def.StatusCode != http.StatusSeeOther {
		t.Fatalf("назначение умолчания: статус %d", def.StatusCode)
	}

	after := getWithCookie(t, s.srv, logsBasePath(projectID), ownerCookie)
	defer after.Body.Close()
	afterBody, _ := io.ReadAll(after.Body)
	text := string(afterBody)
	if !strings.Contains(text, "избранный") || !strings.Contains(text, "по умолчанию") {
		t.Errorf("бейдж умолчания не появился: %s", text)
	}
}

// TestLogFiltersValidationErrors — пустое имя, слишком длинное имя и
// дублирующееся имя перерисовывают страницу логов с 422 и понятным
// сообщением вместо голого редиректа/500.
func TestLogFiltersValidationErrors(t *testing.T) {
	s := newFiltersStack(t)
	_, ownerCookie, project := newLogsProject(t, s, "validation@example.com", "val-org", "val-proj")
	path := logsBasePath(project.ID) + "/filters"

	empty := postForm(t, s.srv, path, url.Values{"name": {"   "}, "q_not": {"buffered"}}, s.srv.URL, ownerCookie)
	defer empty.Body.Close()
	body, _ := io.ReadAll(empty.Body)
	if empty.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("пустое имя: статус %d, тело: %s", empty.StatusCode, body)
	}
	if !strings.Contains(string(body), "Укажите имя фильтра") {
		t.Errorf("сообщение об ошибке пустого имени не показано: %s", body)
	}

	tooLong := postForm(t, s.srv, path, url.Values{"name": {strings.Repeat("а", 61)}, "q_not": {"buffered"}}, s.srv.URL, ownerCookie)
	defer tooLong.Body.Close()
	if tooLong.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("слишком длинное имя: статус %d", tooLong.StatusCode)
	}

	first := postForm(t, s.srv, path, url.Values{"name": {"дубликат"}, "q_not": {"buffered"}}, s.srv.URL, ownerCookie)
	first.Body.Close()
	if first.StatusCode != http.StatusSeeOther {
		t.Fatalf("первый фильтр не создан: статус %d", first.StatusCode)
	}
	dup := postForm(t, s.srv, path, url.Values{"name": {"дубликат"}, "environment": {"staging"}}, s.srv.URL, ownerCookie)
	defer dup.Body.Close()
	dupBody, _ := io.ReadAll(dup.Body)
	if dup.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("дублирующееся имя: статус %d, тело: %s", dup.StatusCode, dupBody)
	}
	if !strings.Contains(string(dupBody), "Фильтр с таким именем уже есть") {
		t.Errorf("сообщение о занятом имени не показано: %s", dupBody)
	}
}

// TestLogFiltersInapplicablePayloadShown — фильтр с payload неизвестной
// версии (продукт мог сменить формат) не роняет страницу и не даёт ссылку
// применения, но остаётся виден в списке с пояснением.
func TestLogFiltersInapplicablePayloadShown(t *testing.T) {
	s := newFiltersStack(t)
	ownerID, ownerCookie, project := newLogsProject(t, s, "inapplicable@example.com", "ia-org", "ia-proj")
	projectID := project.ID

	if _, err := s.pool.Exec(context.Background(),
		`INSERT INTO log_saved_filters (project_id, owner_user_id, author_user_id, name, payload)
		 VALUES ($1, $2, $2, $3, $4)`,
		projectID, ownerID, "устаревший формат", []byte(`{"v":99,"predicates":[]}`)); err != nil {
		t.Fatalf("вставка фильтра с чужим форматом: %v", err)
	}

	page := getWithCookie(t, s.srv, logsBasePath(projectID), ownerCookie)
	defer page.Body.Close()
	body, _ := io.ReadAll(page.Body)
	if page.StatusCode != http.StatusOK {
		t.Fatalf("страница логов упала на неприменимом фильтре: статус %d: %s", page.StatusCode, body)
	}
	text := string(body)
	if !strings.Contains(text, "устаревший формат") {
		t.Errorf("неприменимый фильтр не показан в списке: %s", text)
	}
	if !strings.Contains(text, "Формат фильтра устарел") {
		t.Errorf("пометка о неприменимости не показана: %s", text)
	}
}

// TestLogFilterHandlersRejectForeignOrigin — все четыре хендлера отказывают
// запросу с чужим Origin (sameOrigin) и не меняют состав фильтров проекта.
// filterName/filterExists — точечные проверки состояния одного фильтра
// (в отличие от countFilters, который видит только общее число и не ловит,
// например, «удаления не было, но имя подменили»).
func filterName(t *testing.T, pool *pgxpool.Pool, filterID int64) string {
	t.Helper()
	var name string
	if err := pool.QueryRow(context.Background(),
		"SELECT name FROM log_saved_filters WHERE id = $1", filterID).Scan(&name); err != nil {
		t.Fatalf("filter name: %v", err)
	}
	return name
}

func filterExists(t *testing.T, pool *pgxpool.Pool, filterID int64) bool {
	t.Helper()
	var exists bool
	if err := pool.QueryRow(context.Background(),
		"SELECT EXISTS(SELECT 1 FROM log_saved_filters WHERE id = $1)", filterID).Scan(&exists); err != nil {
		t.Fatalf("filter exists: %v", err)
	}
	return exists
}

// TestLogFilterHandlersRejectForeignOrigin — все четыре хендлера отказывают
// запросу с чужим Origin (sameOrigin). Состояние ИЗОЛИРОВАНО по маршруту:
// свой фильтр под update/delete/default (разные имена, никто не переиспользует
// id после удаления) — иначе прогон через общее имя "чужое" и общий filterID
// (как было раньше) ловит обход только на create/delete: update глушится
// побочным ErrNameTaken (имя уже занято предыдущим шагом), а default бьёт по
// уже удалённому предыдущим шагом id — обе ветки «зелёные» не по защите,
// а по случайному конфликту состояния (находка ревью, Important).
func TestLogFilterHandlersRejectForeignOrigin(t *testing.T) {
	s := newFiltersStack(t)
	ownerID, cookie, project := newLogsProject(t, s, "csrf@example.com", "csrf-org", "csrf-proj")
	projectID := project.ID

	seed := func(name string) int64 {
		resp := postForm(t, s.srv, logsBasePath(projectID)+"/filters",
			url.Values{"name": {name}, "q_not": {"buffered"}}, s.srv.URL, cookie)
		resp.Body.Close()
		if resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("подготовка фильтра %q: статус %d", name, resp.StatusCode)
		}
		return lastFilterID(t, s.pool, projectID)
	}

	updateID := seed("для обновления")
	deleteID := seed("для удаления")
	defaultID := seed("для умолчания")
	before := countFilters(t, s.pool, projectID)

	createResp := postForm(t, s.srv, logsBasePath(projectID)+"/filters",
		url.Values{"name": {"чужое создание"}, "q_not": {"buffered"}}, "https://evil.example", cookie)
	defer createResp.Body.Close()
	if createResp.StatusCode == http.StatusSeeOther {
		t.Errorf("create принял запрос с чужого Origin")
	}
	if after := countFilters(t, s.pool, projectID); after != before {
		t.Errorf("create с чужого Origin изменил число фильтров: было %d, стало %d", before, after)
	}

	updateResp := postForm(t, s.srv, fmt.Sprintf("%s/filters/%d/update", logsBasePath(projectID), updateID),
		url.Values{"name": {"чужое обновление"}}, "https://evil.example", cookie)
	defer updateResp.Body.Close()
	if updateResp.StatusCode == http.StatusSeeOther {
		t.Errorf("update принял запрос с чужого Origin")
	}
	if name := filterName(t, s.pool, updateID); name != "для обновления" {
		t.Errorf("update с чужого Origin изменил имя фильтра: стало %q", name)
	}

	deleteResp := postForm(t, s.srv, fmt.Sprintf("%s/filters/%d/delete", logsBasePath(projectID), deleteID),
		url.Values{}, "https://evil.example", cookie)
	defer deleteResp.Body.Close()
	if deleteResp.StatusCode == http.StatusSeeOther {
		t.Errorf("delete принял запрос с чужого Origin")
	}
	if !filterExists(t, s.pool, deleteID) {
		t.Errorf("delete с чужого Origin удалил фильтр")
	}

	defaultResp := postForm(t, s.srv, fmt.Sprintf("%s/filters/%d/default", logsBasePath(projectID), defaultID),
		url.Values{}, "https://evil.example", cookie)
	defer defaultResp.Body.Close()
	if defaultResp.StatusCode == http.StatusSeeOther {
		t.Errorf("default принял запрос с чужого Origin")
	}
	if _, hasDefault, err := s.h.LogFilters.Default(context.Background(), projectID, ownerID); err != nil {
		t.Fatalf("чтение умолчания: %v", err)
	} else if hasDefault {
		t.Errorf("default с чужого Origin выставил умолчание")
	}
}
