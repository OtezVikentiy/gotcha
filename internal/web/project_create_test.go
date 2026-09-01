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
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
)

// TestWebProjectCreate — второй проект заводится из интерфейса.
//
// До этого CreateProject вызывался ровно из одного места — формы онбординга, а
// она отдаётся только тому, у кого нет ни одного проекта. То есть добавить
// второй сервис в работающую установку было нельзя вообще: ни кнопки, ни
// маршрута. Для продукта, наблюдающего за сервисами, это повторяющийся
// сценарий, а не разовая настройка.
func TestWebProjectCreate(t *testing.T) {
	s := newStack(t)
	authSvc := auth.NewService(s.pool)
	orgSvc := org.NewService(s.pool, 1_000_000)

	ownerID, ownerCookie := orgSettingsRegister(t, authSvc, "projcreate-owner@example.com")
	o, err := orgSvc.CreateOrg(context.Background(), "projcreate-co", "Proj Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	if _, err := orgSvc.CreateProject(context.Background(), o.ID, "first", "First", "go"); err != nil {
		t.Fatalf("create first project: %v", err)
	}

	// На странице проектов организации (/projects теперь лишь дверь туда,
	// задача 5 nav-ia) есть форма создания.
	orgProjectsPath := "/orgs/" + strconv.FormatInt(o.ID, 10) + "/projects"
	resp := getWithCookie(t, s.srv, orgProjectsPath, ownerCookie)
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(page), `id="new-project"`) {
		t.Fatalf("на %s нет формы создания проекта:\n%s", orgProjectsPath, page)
	}

	resp = postForm(t, s.srv, "/projects/new", url.Values{
		"org_id": {strconv.FormatInt(o.ID, 10)}, "slug": {"second"}, "name": {"Second"}, "platform": {"php"},
	}, s.srv.URL, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST /projects/new = %d, want 303: %s", resp.StatusCode, body)
	}

	projects, err := orgSvc.ProjectsForUser(context.Background(), ownerID)
	if err != nil || len(projects) != 2 {
		t.Fatalf("ProjectsForUser = %+v err=%v, want 2", projects, err)
	}
	var created org.Project
	for _, p := range projects {
		if p.Slug == "second" {
			created = p
		}
	}
	if created.ID == 0 {
		t.Fatalf("проект не создан: %+v", projects)
	}
	// Ключ приёма заведён — иначе страница подключения SDK показала бы проект
	// без DSN, то есть бесполезный.
	keys, err := orgSvc.KeysForProject(context.Background(), created.ID)
	if err != nil || len(keys) == 0 {
		t.Fatalf("Keys = %+v err=%v, want at least one", keys, err)
	}
	// Редирект ведёт на страницу подключения — как и после онбординга.
	if loc := resp.Header.Get("Location"); !strings.Contains(loc, "/setup") {
		t.Errorf("Location = %q, want страницу подключения SDK", loc)
	}
}

// TestWebProjectCreateInvalidSlugReopensForm — на 422 форма возвращается
// открытой и с введённым, как у остальных модалок.
func TestWebProjectCreateInvalidSlugReopensForm(t *testing.T) {
	s := newStack(t)
	authSvc := auth.NewService(s.pool)
	orgSvc := org.NewService(s.pool, 1_000_000)

	ownerID, ownerCookie := orgSettingsRegister(t, authSvc, "projcreate-bad@example.com")
	o, err := orgSvc.CreateOrg(context.Background(), "projcreate-bad-co", "Bad Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}

	resp := postForm(t, s.srv, "/projects/new", url.Values{
		"org_id": {strconv.FormatInt(o.ID, 10)}, "slug": {"Не Слаг"}, "name": {"Имя"}, "platform": {"go"},
	}, s.srv.URL, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST с невалидным slug = %d, want 422", resp.StatusCode)
	}
	if !strings.Contains(string(body), `id="new-project" class="modal modal--open"`) {
		t.Fatalf("форма не открыта заново:\n%s", body)
	}
	if !strings.Contains(string(body), `value="Имя"`) {
		t.Fatalf("введённое имя потеряно:\n%s", body)
	}
}

// TestWebProjectCreateForbiddenForMember — обычный участник организации
// проектов не заводит: те же owner/admin, что и на остальных управляющих
// действиях.
func TestWebProjectCreateForbiddenForMember(t *testing.T) {
	s := newStack(t)
	authSvc := auth.NewService(s.pool)
	orgSvc := org.NewService(s.pool, 1_000_000)

	ownerID, _ := orgSettingsRegister(t, authSvc, "projcreate-owner2@example.com")
	memberID, memberCookie := orgSettingsRegister(t, authSvc, "projcreate-member@example.com")
	o, err := orgSvc.CreateOrg(context.Background(), "projcreate-member-co", "Member Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	if err := orgSvc.AddMember(context.Background(), o.ID, memberID, org.RoleMember); err != nil {
		t.Fatalf("add member: %v", err)
	}

	resp := postForm(t, s.srv, "/projects/new", url.Values{
		"org_id": {strconv.FormatInt(o.ID, 10)}, "slug": {"sneaky"}, "name": {"Sneaky"}, "platform": {"go"},
	}, s.srv.URL, memberCookie)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST от участника = %d, want 403 (№72)", resp.StatusCode)
	}
	projects, err := orgSvc.ProjectsOf(context.Background(), o.ID)
	if err != nil || len(projects) != 0 {
		t.Fatalf("ProjectsOf = %+v err=%v, want none", projects, err)
	}
}
