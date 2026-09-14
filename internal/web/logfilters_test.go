package web_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/log"
	"gitflic.ru/otezvikentiy/gotcha/internal/logfilter"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/web"
)

func newFiltersStack(t *testing.T) *logsStack {
	t.Helper()
	s := newLogsStack(t, true)
	s.h.LogFilters = logfilter.NewStore(s.pool)
	return s
}

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
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s на чужой личный фильтр вернул %d, ожидался 404", action, resp.StatusCode)
		}
	}
}

func TestLogFiltersCrossProjectUpdateAndDeleteRejected(t *testing.T) {
	s := newFiltersStack(t)
	_, ownerACookie, projectA := newLogsProject(t, s, "cross-a@example.com", "cross-a-org", "cross-a-proj")
	_, ownerBCookie, projectB := newLogsProject(t, s, "cross-b@example.com", "cross-b-org", "cross-b-proj")

	create := postForm(t, s.srv, logsBasePath(projectB.ID)+"/filters",
		url.Values{"name": {"фильтр проекта B"}, "shared": {"1"}, "q_not": {"buffered"}}, s.srv.URL, ownerBCookie)
	create.Body.Close()
	if create.StatusCode != http.StatusSeeOther {
		t.Fatalf("создание фильтра в проекте B: статус %d", create.StatusCode)
	}
	filterID := lastFilterID(t, s.pool, projectB.ID)

	updatePath := fmt.Sprintf("%s/filters/%d/update", logsBasePath(projectA.ID), filterID)
	update := postForm(t, s.srv, updatePath, url.Values{"name": {"угнанное имя"}, "shared": {"1"}}, s.srv.URL, ownerACookie)
	update.Body.Close()
	if update.StatusCode != http.StatusNotFound {
		t.Errorf("update чужого фильтра под своим project_id в пути: статус %d, ожидали 404", update.StatusCode)
	}

	deletePath := fmt.Sprintf("%s/filters/%d/delete", logsBasePath(projectA.ID), filterID)
	del := postForm(t, s.srv, deletePath, url.Values{}, s.srv.URL, ownerACookie)
	del.Body.Close()
	if del.StatusCode != http.StatusNotFound {
		t.Errorf("delete чужого фильтра под своим project_id в пути: статус %d, ожидали 404", del.StatusCode)
	}

	if got := countFilters(t, s.pool, projectB.ID); got != 1 {
		t.Errorf("фильтр проекта B пропал после чужих запросов: осталось %d", got)
	}
	if name := filterName(t, s.pool, filterID); name != "фильтр проекта B" {
		t.Errorf("имя фильтра проекта B изменено чужим update: %q", name)
	}
}

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

func filterUpdateFormHTML(t *testing.T, html string, projectID, filterID int64) string {
	t.Helper()
	marker := fmt.Sprintf(`%s/filters/%d/update"`, logsBasePath(projectID), filterID)
	idx := strings.Index(html, marker)
	if idx < 0 {
		t.Fatalf("форма «Обновить» для фильтра %d не найдена: %s", filterID, html)
	}
	formStart := strings.LastIndex(html[:idx], "<form")
	if formStart < 0 {
		t.Fatalf("не нашли открывающий <form для фильтра %d", filterID)
	}
	formEnd := strings.Index(html[idx:], "</form>")
	if formEnd < 0 {
		t.Fatalf("не нашли закрывающий </form для фильтра %d", filterID)
	}
	return html[formStart : idx+formEnd+len("</form>")]
}

func TestLogFiltersUpdateFormShowsRenameAndVisibilityControlsForOperator(t *testing.T) {
	s := newFiltersStack(t)
	_, ownerCookie, project := newLogsProject(t, s, "ui-controls-owner@example.com", "uic-org", "uic-proj")
	projectID := project.ID

	create := postForm(t, s.srv, logsBasePath(projectID)+"/filters",
		url.Values{"name": {"мой личный"}, "q_not": {"buffered"}}, s.srv.URL, ownerCookie)
	create.Body.Close()
	if create.StatusCode != http.StatusSeeOther {
		t.Fatalf("создание личного: статус %d", create.StatusCode)
	}
	filterID := lastFilterID(t, s.pool, projectID)

	page := getWithCookie(t, s.srv, logsBasePath(projectID), ownerCookie)
	defer page.Body.Close()
	body, _ := io.ReadAll(page.Body)
	form := filterUpdateFormHTML(t, string(body), projectID, filterID)

	if strings.Contains(form, `type="hidden" name="name"`) {
		t.Errorf("имя всё ещё скрытым полем, переименовать через интерфейс нельзя: %s", form)
	}
	if !strings.Contains(form, `type="text" name="name"`) || !strings.Contains(form, `value="мой личный"`) {
		t.Errorf("нет текстового поля имени, предзаполненного текущим значением: %s", form)
	}
	if !strings.Contains(form, `type="checkbox" name="shared"`) {
		t.Errorf("оператору не показан переключатель видимости: %s", form)
	}
}

func TestLogFiltersUpdateFormHidesVisibilityToggleForNonOperator(t *testing.T) {
	s := newFiltersStack(t)
	_, _, project := newLogsProject(t, s, "ui-controls-owner2@example.com", "uic2-org", "uic2-proj")
	projectID := project.ID
	memberID, memberCookie := orgSettingsRegister(t, s.auth, "ui-controls-member@example.com")
	addProjectMember(t, s, project.OrgID, projectID, memberID)

	create := postForm(t, s.srv, logsBasePath(projectID)+"/filters",
		url.Values{"name": {"фильтр участника"}, "q_not": {"buffered"}}, s.srv.URL, memberCookie)
	create.Body.Close()
	if create.StatusCode != http.StatusSeeOther {
		t.Fatalf("создание личного участником: статус %d", create.StatusCode)
	}
	filterID := lastFilterID(t, s.pool, projectID)

	page := getWithCookie(t, s.srv, logsBasePath(projectID), memberCookie)
	defer page.Body.Close()
	body, _ := io.ReadAll(page.Body)
	form := filterUpdateFormHTML(t, string(body), projectID, filterID)

	if !strings.Contains(form, `type="text" name="name"`) {
		t.Errorf("рядовому участнику не показано поле переименования собственного фильтра: %s", form)
	}
	if strings.Contains(form, `type="checkbox" name="shared"`) {
		t.Errorf("рядовому участнику показан переключатель видимости: %s", form)
	}
}

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
	deletePath := fmt.Sprintf("%s/filters/%d/delete", logsBasePath(projectID), filterID)

	// Без confirmed=yes — страница подтверждения, называющая фильтр, а не удаление.
	unconfirmed := postForm(t, s.srv, deletePath, url.Values{}, s.srv.URL, ownerCookie)
	confirmBody, _ := io.ReadAll(unconfirmed.Body)
	unconfirmed.Body.Close()
	if unconfirmed.StatusCode != http.StatusOK || !strings.Contains(string(confirmBody), "на удаление") {
		t.Fatalf("подтверждение удаления фильтра не называет его: статус=%d, %s", unconfirmed.StatusCode, confirmBody)
	}
	if after := countFilters(t, s.pool, projectID); after != before {
		t.Fatalf("фильтр удалён без подтверждения: было %d, стало %d", before, after)
	}

	del := postForm(t, s.srv, deletePath, url.Values{"confirmed": {"yes"}}, s.srv.URL, ownerCookie)
	defer del.Body.Close()
	if del.StatusCode != http.StatusSeeOther {
		t.Fatalf("удаление личного фильтра владельцем: статус %d", del.StatusCode)
	}
	if after := countFilters(t, s.pool, projectID); after != before-1 {
		t.Errorf("фильтр не удалён: было %d, стало %d", before, after)
	}
}

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

	forbidden := postForm(t, s.srv, path, url.Values{"confirmed": {"yes"}}, s.srv.URL, memberCookie)
	defer forbidden.Body.Close()
	if forbidden.StatusCode != http.StatusForbidden {
		t.Fatalf("рядовой участник удалил общий фильтр: статус %d", forbidden.StatusCode)
	}
	if after := countFilters(t, s.pool, projectID); after != before {
		t.Errorf("общий фильтр исчез после отказа: было %d, стало %d", before, after)
	}

	ok := postForm(t, s.srv, path, url.Values{"confirmed": {"yes"}}, s.srv.URL, ownerCookie)
	defer ok.Body.Close()
	if ok.StatusCode != http.StatusSeeOther {
		t.Fatalf("оператор не смог удалить общий фильтр: статус %d", ok.StatusCode)
	}
	if after := countFilters(t, s.pool, projectID); after != before-1 {
		t.Errorf("удаление оператором не сработало: было %d, стало %d", before, after)
	}
}

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

func TestLogFiltersSaveErrorPreservesConditionsAndSkipsDefault(t *testing.T) {
	s, projectID, cookie := seedDefaultFilterCase(t)

	dup := postForm(t, s.srv, logsBasePath(projectID)+"/filters",
		url.Values{"name": {"без шума"}, "service_not": {"worker"}}, s.srv.URL, cookie)
	defer dup.Body.Close()
	body, _ := io.ReadAll(dup.Body)
	if dup.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("дублирующееся имя: статус %d, тело: %s", dup.StatusCode, body)
	}
	text := string(body)
	if !strings.Contains(text, "Сервис ≠ worker") {
		t.Errorf("страница 422 потеряла условие, введённое в форме: %s", text)
	}
	if strings.Contains(text, "logs-default-notice") {
		t.Errorf("страница 422 молча применила фильтр по умолчанию поверх введённых условий: %s", text)
	}
}

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
	// Причина обязана быть частью видимого текста, не только атрибутом title —
	// title недостижим с клавиатуры и с тача.
	if strings.Contains(text, `title="Формат фильтра устарел`) {
		t.Errorf("причина неприменимости доступна только через title: %s", text)
	}
}

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

const (
	noisyRowSummary  = "<summary>buffered to a temporary file</summary>"
	usefulRowSummary = "<summary>полезная запись</summary>"
)

func seedDefaultFilterCase(t *testing.T) (s *logsStack, projectID int64, cookie *http.Cookie) {
	t.Helper()
	s = newFiltersStack(t)
	_, ownerCookie, project := newLogsProject(t, s, "def@example.com", "def-org", "def-proj")
	projectID = project.ID

	now := time.Now().UTC().Add(-time.Minute)
	s.seedLogs(t, projectID,
		log.LogRecord{
			Timestamp: now, ObservedTS: now,
			Severity: log.SevWarn, SeverityNumber: 13, SeverityText: "WARN",
			Body: "buffered to a temporary file", Service: "nginx", Environment: "production",
		},
		log.LogRecord{
			Timestamp: now, ObservedTS: now,
			Severity: log.SevWarn, SeverityNumber: 13, SeverityText: "WARN",
			Body: "полезная запись", Service: "nginx", Environment: "production",
		},
	)

	create := postForm(t, s.srv, logsBasePath(projectID)+"/filters",
		url.Values{"name": {"без шума"}, "q_not": {"buffered to a temporary file"}}, s.srv.URL, ownerCookie)
	create.Body.Close()
	if create.StatusCode != http.StatusSeeOther {
		t.Fatalf("создание фильтра: статус %d", create.StatusCode)
	}
	filterID := lastFilterID(t, s.pool, projectID)

	def := postForm(t, s.srv, fmt.Sprintf("%s/filters/%d/default", logsBasePath(projectID), filterID),
		url.Values{}, s.srv.URL, ownerCookie)
	def.Body.Close()
	if def.StatusCode != http.StatusSeeOther {
		t.Fatalf("назначение умолчания: статус %d", def.StatusCode)
	}
	return s, projectID, ownerCookie
}

func fetchLogsBody(t *testing.T, s *logsStack, path string, cookie *http.Cookie) string {
	t.Helper()
	resp := getWithCookie(t, s.srv, path, cookie)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("чтение тела: %v", err)
	}
	return string(body)
}

func TestDefaultFilterAppliesOnBareURL(t *testing.T) {
	s, projectID, cookie := seedDefaultFilterCase(t)

	html := fetchLogsBody(t, s, logsBasePath(projectID), cookie)
	if strings.Contains(html, noisyRowSummary) {
		t.Errorf("умолчание не применилось: шумная запись в выдаче")
	}
	if !strings.Contains(html, usefulRowSummary) {
		t.Errorf("умолчание съело полезную запись")
	}
	if !strings.Contains(html, "logs-default-notice") {
		t.Errorf("нет плашки о применённом умолчании — пользователь не узнает, что часть логов скрыта")
	}
}

func TestDefaultFilterSkippedWhenURLHasFilterParams(t *testing.T) {
	s, projectID, cookie := seedDefaultFilterCase(t)

	for _, name := range web.LogFilterParamsForTest {
		qs := url.Values{name: {"x"}}.Encode()
		html := fetchLogsBody(t, s, logsBasePath(projectID)+"?"+qs, cookie)
		if strings.Contains(html, "logs-default-notice") {
			t.Errorf("при параметре %s умолчание всё равно применилось", name)
		}
	}
}

func TestDefaultFilterNotSuppressedByPaginationOrRange(t *testing.T) {
	s, projectID, cookie := seedDefaultFilterCase(t)

	for _, param := range []string{"before=1757200000000", "tskip=2", "facet=source", "period=24h"} {
		html := fetchLogsBody(t, s, logsBasePath(projectID)+"?"+param, cookie)
		if !strings.Contains(html, "logs-default-notice") {
			t.Errorf("параметр %s подавил умолчание, хотя не является параметром отбора", param)
		}
	}
}

func extractShowAllHref(t *testing.T, html string) string {
	t.Helper()
	idx := strings.Index(html, "logs-default-notice")
	if idx < 0 {
		t.Fatalf("плашка не найдена")
	}
	const marker = `href="`
	hrefIdx := strings.Index(html[idx:], marker)
	if hrefIdx < 0 {
		t.Fatalf("ссылка «показать всё» не найдена рядом с плашкой")
	}
	start := idx + hrefIdx + len(marker)
	end := strings.Index(html[start:], `"`)
	if end < 0 {
		t.Fatalf("не удалось прочитать href ссылки")
	}
	return strings.ReplaceAll(html[start:start+end], "&amp;", "&")
}

func TestDefaultFilterShowAllLinkSuppressesDefault(t *testing.T) {
	s, projectID, cookie := seedDefaultFilterCase(t)

	html := fetchLogsBody(t, s, logsBasePath(projectID), cookie)
	href := extractShowAllHref(t, html)

	after := fetchLogsBody(t, s, href, cookie)
	if strings.Contains(after, "logs-default-notice") {
		t.Errorf("ссылка «показать всё» снова включила умолчание: %s", href)
	}
	if !strings.Contains(after, noisyRowSummary) {
		t.Errorf("ссылка «показать всё» не показала запись, скрытую умолчанием")
	}
}

func TestDefaultFilterFormCarriesSuppression(t *testing.T) {
	s, projectID, cookie := seedDefaultFilterCase(t)

	html := fetchLogsBody(t, s, logsBasePath(projectID), cookie)
	href := extractShowAllHref(t, html)

	after := fetchLogsBody(t, s, href, cookie)
	if !strings.Contains(after, `name="nodefault" value="1"`) {
		t.Errorf("форма фильтров не несёт скрытое поле nodefault после «показать всё»: %s", after)
	}

	resubmitted := fetchLogsBody(t, s, logsBasePath(projectID)+"?nodefault=1", cookie)
	if strings.Contains(resubmitted, "logs-default-notice") {
		t.Errorf("сабмит формы без заполненных полей воскресил умолчание")
	}
}

func TestDefaultFilterNotApplicableSkipped(t *testing.T) {
	s, projectID, cookie := seedDefaultFilterCase(t)

	filterID := lastFilterID(t, s.pool, projectID)
	if _, err := s.pool.Exec(context.Background(),
		`UPDATE log_saved_filters SET payload = '{"v":99,"predicates":[]}' WHERE id = $1`, filterID); err != nil {
		t.Fatalf("испортить версию payload: %v", err)
	}

	html := fetchLogsBody(t, s, logsBasePath(projectID), cookie)
	if strings.Contains(html, "Применён фильтр по умолчанию") {
		t.Errorf("неприменимое умолчание показало плашку «умолчание применено», как будто применилось")
	}
	// Пропуск умолчания обязан быть виден в плашке над списком, не молчалив; тот же
	// текст легитимно встречается и в панели фильтров — класс плашки их различает.
	if !strings.Contains(html, `class="notice logs-default-notice">Формат фильтра устарел`) {
		t.Errorf("неприменимое умолчание пропущено молча, без объяснения над списком: %s", html)
	}
	if !strings.Contains(html, noisyRowSummary) || !strings.Contains(html, usefulRowSummary) {
		t.Errorf("неприменимое умолчание скрыло записи вместо игнорирования: %s", html)
	}
}
