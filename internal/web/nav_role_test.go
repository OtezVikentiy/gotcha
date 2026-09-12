package web_test

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
)

func TestMemberSeesNoLinksToPagesThatRejectHim(t *testing.T) {
	s := newStack(t)
	s.h.Uptime = uptime.NewService(s.pool)
	authSvc := auth.NewService(s.pool)
	orgSvc := org.NewService(s.pool, 1_000_000)

	ownerID, _ := orgSettingsRegister(t, authSvc, "navrole-owner@example.com")
	memberID, memberCookie := orgSettingsRegister(t, authSvc, "navrole-member@example.com")
	o, err := orgSvc.CreateOrg(context.Background(), "navrole-co", "Nav Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	if err := orgSvc.AddMember(context.Background(), o.ID, memberID, org.RoleMember); err != nil {
		t.Fatalf("add member: %v", err)
	}
	proj, err := orgSvc.CreateProject(context.Background(), o.ID, "navrole-proj", "Nav Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	addTeamAccess(t, orgSvc, o.ID, proj.ID, memberID, "navrole-team")

	pid := strconv.FormatInt(proj.ID, 10)
	oid := strconv.FormatInt(o.ID, 10)
	resp := getWithCookie(t, s.srv, "/projects/"+pid+"/issues", memberCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET issues как участник = %d, want 200", resp.StatusCode)
	}
	page := string(body)

	forbidden := []string{
		"/projects/" + pid + "/settings",
		"/orgs/" + oid + "/settings",
		"/orgs/" + oid + "/teams",
		"/orgs/" + oid + "/probes",
	}
	for _, href := range forbidden {
		if strings.Contains(page, `href="`+href+`"`) {
			t.Errorf("участнику показана ссылка %q, которая отдаёт ему отказ", href)
		}
	}

	for _, path := range forbidden {
		r := getWithCookie(t, s.srv, path, memberCookie)
		r.Body.Close()
		if r.StatusCode != http.StatusForbidden && r.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s как участник = %d, want 403/404 (иначе прятать ссылку незачем)", path, r.StatusCode)
		}
	}

	if !strings.Contains(page, `href="/projects/`+pid+`/alerts"`) {
		t.Error("участнику команды не показана область «Оповещения» — регрессия задачи 5")
	}
	if !strings.Contains(page, `href="/projects/`+pid+`/statuspages"`) {
		t.Error("участнику команды не показан первый видимый подраздел «Настроек» (статус-страницы)")
	}

	if !strings.Contains(page, `href="/projects/`+pid+`/monitors"`) {
		t.Error("участник потерял область «Аптайм» — мониторы ему доступны")
	}

	settingsResp := getWithCookie(t, s.srv, "/projects/"+pid+"/statuspages", memberCookie)
	settingsBody, _ := io.ReadAll(settingsResp.Body)
	settingsResp.Body.Close()
	if settingsResp.StatusCode != http.StatusOK {
		t.Fatalf("GET statuspages как оператор команды = %d, want 200", settingsResp.StatusCode)
	}
	settingsPage := string(settingsBody)
	for _, href := range []string{
		"/orgs/" + oid + "/settings",
		"/orgs/" + oid + "/teams",
		"/orgs/" + oid + "/probes",
		"/projects/" + pid + "/settings",
	} {
		if strings.Contains(settingsPage, `href="`+href+`"`) {
			t.Errorf("сайдбар «Настроек» показывает участнику команды CanManage-only ссылку %q", href)
		}
	}
	if !strings.Contains(settingsPage, `href="/projects/`+pid+`/statuspages"`) {
		t.Error("сайдбар «Настроек» не содержит «Статус-страницы» (CanOperate) для оператора")
	}
	if !strings.Contains(settingsPage, `href="/projects/`+pid+`/setup"`) {
		t.Error("сайдбар «Настроек» не содержит саму себя («Первые шаги», доступна без гейта)")
	}
}
