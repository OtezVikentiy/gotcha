package web_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
)

func TestCoverOrgSettingsInvalidPathAndForm(t *testing.T) {
	s := newStack(t)
	authSvc := auth.NewService(s.pool)
	orgSvc := org.NewService(s.pool, 1_000_000)

	ownerID, ownerCookie := orgSettingsRegister(t, authSvc, "cover-org-owner@example.com")
	adminID, adminCookie := orgSettingsRegister(t, authSvc, "cover-org-admin@example.com")
	o, err := orgSvc.CreateOrg(context.Background(), "cover-org-co", "Cover Org", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	if err := orgSvc.AddMember(context.Background(), o.ID, adminID, org.RoleAdmin); err != nil {
		t.Fatalf("add admin: %v", err)
	}

	resp := getWithCookie(t, s.srv, "/orgs/not-a-number/settings", ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET /orgs/not-a-number/settings status = %d, want 404", resp.StatusCode)
	}

	base := "/orgs/" + strconv.FormatInt(o.ID, 10) + "/settings"

	resp = postForm(t, s.srv, base+"/role", url.Values{"user_id": {"abc"}, "role": {"admin"}}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("POST role (bad user_id) status = %d, want 400", resp.StatusCode)
	}

	resp = postForm(t, s.srv, base+"/remove", url.Values{"user_id": {"xyz"}}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("POST remove (bad user_id) status = %d, want 400", resp.StatusCode)
	}

	resp = postForm(t, s.srv, base+"/remove", url.Values{"user_id": {strconv.FormatInt(adminID, 10)}}, s.srv.URL, ownerCookie)
	removeBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(removeBody), "cover-org-admin@example.com") {
		t.Fatalf("подтверждение исключения участника не называет email: status=%d, %s", resp.StatusCode, removeBody)
	}

	resp = postForm(t, s.srv, base+"/sso", url.Values{"issuer": {"x"}}, "", ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST sso (no origin) status = %d, want 403", resp.StatusCode)
	}

	resp = postForm(t, s.srv, base+"/sso", url.Values{"issuer": {""}, "client_id": {""}}, s.srv.URL, adminCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST sso (non-instance-admin) status = %d, want 403", resp.StatusCode)
	}

	if _, err := s.pool.Exec(context.Background(), "UPDATE users SET is_instance_admin = true WHERE id = $1", ownerID); err != nil {
		t.Fatalf("promote owner: %v", err)
	}
	resp = postForm(t, s.srv, base+"/sso", url.Values{"issuer": {""}, "client_id": {""}}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST sso (invalid fields) status = %d, want 422", resp.StatusCode)
	}

	resp = postForm(t, s.srv, base+"/sso/delete", url.Values{"confirmed": {"yes"}}, "", ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST sso/delete (no origin) status = %d, want 403", resp.StatusCode)
	}
	resp = postForm(t, s.srv, base+"/sso/delete", url.Values{"confirmed": {"yes"}}, s.srv.URL, adminCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST sso/delete (non-instance-admin) status = %d, want 403", resp.StatusCode)
	}

	resp = postForm(t, s.srv, base+"/sso/delete", url.Values{}, s.srv.URL, ownerCookie)
	ssoConfirmBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(ssoConfirmBody), "Cover Org") {
		t.Fatalf("подтверждение удаления SSO не называет организацию: status=%d, %s", resp.StatusCode, ssoConfirmBody)
	}

	resp = postForm(t, s.srv, base+"/sso/delete", url.Values{"confirmed": {"yes"}}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST sso/delete (owner) status = %d, want 303", resp.StatusCode)
	}

	resp = postForm(t, s.srv, base+"/invite", url.Values{"email": {"x@example.com"}, "role": {"member"}}, "", ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST invite (no origin) status = %d, want 403", resp.StatusCode)
	}

	resp = postForm(t, s.srv, base+"/quota", url.Values{"event_quota": {"not-a-number"}}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST quota (non-numeric) status = %d, want 422", resp.StatusCode)
	}
}

func TestCoverOrgSettingsLeave(t *testing.T) {
	s := newStack(t)
	authSvc := auth.NewService(s.pool)
	orgSvc := org.NewService(s.pool, 1_000_000)

	ownerID, ownerCookie := orgSettingsRegister(t, authSvc, "leave-owner@example.com")
	memberID, memberCookie := orgSettingsRegister(t, authSvc, "leave-member@example.com")
	_, strangerCookie := orgSettingsRegister(t, authSvc, "leave-stranger@example.com")

	o, err := orgSvc.CreateOrg(context.Background(), "leave-co", "Leave Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	if err := orgSvc.AddMember(context.Background(), o.ID, memberID, org.RoleMember); err != nil {
		t.Fatalf("add member: %v", err)
	}
	leavePath := "/orgs/" + strconv.FormatInt(o.ID, 10) + "/settings/leave"

	resp := postForm(t, s.srv, leavePath, url.Values{}, "", memberCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST leave (no origin) status = %d, want 403", resp.StatusCode)
	}

	resp = postForm(t, s.srv, leavePath, url.Values{}, s.srv.URL, memberCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST leave (unconfirmed) status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(string(body), "Leave Co") {
		t.Fatalf("подтверждение выхода из организации не называет её: %s", body)
	}

	// Посторонний не должен получить название организации даже на неподтверждённом
	// запросе — иначе подстановкой чужого orgID можно было бы узнавать чужие имена.
	resp = postForm(t, s.srv, leavePath, url.Values{}, s.srv.URL, strangerCookie)
	strangerBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("POST leave (stranger, unconfirmed) status = %d, want 404", resp.StatusCode)
	}
	if strings.Contains(string(strangerBody), "Leave Co") {
		t.Fatalf("подтверждение выхода утекло постороннему название организации: %s", strangerBody)
	}

	resp = postForm(t, s.srv, leavePath, url.Values{"confirmed": {"yes"}}, s.srv.URL, strangerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("POST leave (stranger) status = %d, want 404", resp.StatusCode)
	}

	resp = postForm(t, s.srv, leavePath, url.Values{"confirmed": {"yes"}}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST leave (last owner) status = %d, want 422", resp.StatusCode)
	}

	resp = postForm(t, s.srv, leavePath, url.Values{"confirmed": {"yes"}}, s.srv.URL, memberCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST leave (member) status = %d, want 303", resp.StatusCode)
	}
	if _, err := orgSvc.Role(context.Background(), o.ID, memberID); err == nil {
		t.Fatalf("member still in org after leave")
	}
}

func TestCoverQuotaBannerNearLimit(t *testing.T) {
	s := newStack(t)
	authSvc := auth.NewService(s.pool)
	orgSvc := org.NewService(s.pool, 1_000_000)

	ownerID, ownerCookie := orgSettingsRegister(t, authSvc, "banner-owner@example.com")
	o, err := orgSvc.CreateOrg(context.Background(), "banner-co", "Banner Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	if err := orgSvc.SetQuota(context.Background(), o.ID, 10); err != nil {
		t.Fatalf("set quota: %v", err)
	}
	for i := 0; i < 9; i++ {
		if _, err := orgSvc.IncUsage(context.Background(), o.ID, time.Now()); err != nil {
			t.Fatalf("inc usage: %v", err)
		}
	}
	resp := getWithCookie(t, s.srv, "/orgs/"+strconv.FormatInt(o.ID, 10)+"/settings", ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET settings (near limit) status = %d, want 200", resp.StatusCode)
	}
	html := string(body)
	if !containsAll(html, "События", "9", "10") {
		t.Fatalf("баннер приближения к лимиту не показывает вид/использование для событий: %s", html)
	}
}

// Раньше баннер приближения к лимиту проверял только квоту событий — транзакции,
// метрики, профили и логи отбрасывались молча, без единого предупреждения.
func TestCoverQuotaBannerNearLimitOtherKind(t *testing.T) {
	s := newStack(t)
	authSvc := auth.NewService(s.pool)
	orgSvc := org.NewService(s.pool, 1_000_000)

	ownerID, ownerCookie := orgSettingsRegister(t, authSvc, "banner-tx-owner@example.com")
	o, err := orgSvc.CreateOrg(context.Background(), "banner-tx-co", "Banner Tx Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	if err := orgSvc.SetTransactionQuota(context.Background(), o.ID, 10); err != nil {
		t.Fatalf("set transaction quota: %v", err)
	}
	for i := 0; i < 9; i++ {
		if _, err := orgSvc.IncTransactionUsage(context.Background(), o.ID, time.Now()); err != nil {
			t.Fatalf("inc transaction usage: %v", err)
		}
	}
	resp := getWithCookie(t, s.srv, "/orgs/"+strconv.FormatInt(o.ID, 10)+"/settings", ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET settings (tx near limit) status = %d, want 200", resp.StatusCode)
	}
	html := string(body)
	if !containsAll(html, "Транзакции", "9", "10") {
		t.Fatalf("баннер приближения к лимиту не сработал для транзакций: %s", html)
	}
}

func TestCoverQuotaBannerDroppedLogs(t *testing.T) {
	s := newStack(t)
	authSvc := auth.NewService(s.pool)
	orgSvc := org.NewService(s.pool, 1_000_000)

	ownerID, ownerCookie := orgSettingsRegister(t, authSvc, "banner-logs-owner@example.com")
	o, err := orgSvc.CreateOrg(context.Background(), "banner-logs-co", "Banner Logs Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	if err := orgSvc.IncDroppedLogs(context.Background(), o.ID, time.Now(), 4); err != nil {
		t.Fatalf("inc dropped logs: %v", err)
	}
	resp := getWithCookie(t, s.srv, "/orgs/"+strconv.FormatInt(o.ID, 10)+"/settings", ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET settings (dropped logs) status = %d, want 200", resp.StatusCode)
	}
	html := string(body)
	if !containsAll(html, "логи", "4") {
		t.Fatalf("страница настроек не показывает дропнутые логи в баннере, body=%s", html)
	}
	if !bytes.Contains(body, []byte("отклонено")) {
		t.Fatalf("баннер дропов не отрисован для dropped_logs>0")
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}

func TestCoverTeamsInvalidPathAndForm(t *testing.T) {
	s := newStack(t)
	authSvc := auth.NewService(s.pool)
	orgSvc := org.NewService(s.pool, 1_000_000)

	ownerID, ownerCookie := orgSettingsRegister(t, authSvc, "cover-teams-owner@example.com")
	_, memberCookie := orgSettingsRegister(t, authSvc, "cover-teams-member@example.com")
	o, err := orgSvc.CreateOrg(context.Background(), "cover-teams-co", "Cover Teams", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	team, err := orgSvc.CreateTeam(context.Background(), o.ID, "cover-team", "Cover Team")
	if err != nil {
		t.Fatalf("create team: %v", err)
	}
	teamBase := "/teams/" + strconv.FormatInt(team.ID, 10)

	resp := postForm(t, s.srv, "/teams/not-a-number/members", url.Values{"user_id": {"1"}}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("POST /teams/not-a-number/members status = %d, want 404", resp.StatusCode)
	}

	resp = postForm(t, s.srv, "/teams/9999999/members", url.Values{"user_id": {"1"}}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("POST /teams/9999999/members status = %d, want 404", resp.StatusCode)
	}

	resp = postForm(t, s.srv, teamBase+"/members", url.Values{"user_id": {"abc"}}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("POST members (bad user_id) status = %d, want 400", resp.StatusCode)
	}

	resp = postForm(t, s.srv, teamBase+"/members/remove", url.Values{"confirmed": {"yes"}, "user_id": {"abc"}}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("POST members/remove (bad user_id) status = %d, want 400", resp.StatusCode)
	}

	resp = postForm(t, s.srv, teamBase+"/projects", url.Values{"project_id": {"abc"}}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("POST projects (bad project_id) status = %d, want 400", resp.StatusCode)
	}

	resp = postForm(t, s.srv, teamBase+"/projects/detach", url.Values{"project_id": {"abc"}}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("POST projects/detach (bad project_id) status = %d, want 400", resp.StatusCode)
	}

	resp = postForm(t, s.srv, teamBase+"/projects", url.Values{"project_id": {"9999999"}}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST projects (nonexistent project) status = %d, want 422", resp.StatusCode)
	}

	proj, err := orgSvc.CreateProject(context.Background(), o.ID, "cover-team-proj", "Cover Team Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	resp = postForm(t, s.srv, teamBase+"/projects/detach", url.Values{"project_id": {strconv.FormatInt(proj.ID, 10)}, "confirmed": {"yes"}}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST projects/detach (unattached) status = %d, want 303", resp.StatusCode)
	}

	resp = postForm(t, s.srv, "/orgs/"+strconv.FormatInt(o.ID, 10)+"/teams", url.Values{"slug": {"x"}, "name": {"X"}}, s.srv.URL, memberCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("POST teams (non-member) status = %d, want 404", resp.StatusCode)
	}
	resp = getWithCookie(t, s.srv, "/orgs/not-a-number/teams", ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET /orgs/not-a-number/teams status = %d, want 404", resp.StatusCode)
	}
}

func TestCoverProjSettingsValidation(t *testing.T) {
	s := newStack(t)
	authSvc := auth.NewService(s.pool)
	orgSvc := org.NewService(s.pool, 1_000_000)

	ownerID, ownerCookie := orgSettingsRegister(t, authSvc, "cover-ps-owner@example.com")
	memberID, memberCookie := orgSettingsRegister(t, authSvc, "cover-ps-member@example.com")
	o, err := orgSvc.CreateOrg(context.Background(), "cover-ps-co", "Cover PS", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	if err := orgSvc.AddMember(context.Background(), o.ID, memberID, org.RoleMember); err != nil {
		t.Fatalf("add member: %v", err)
	}
	proj, err := orgSvc.CreateProject(context.Background(), o.ID, "cover-ps-proj", "Cover PS Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	base := "/projects/" + strconv.FormatInt(proj.ID, 10) + "/settings"

	resp := getWithCookie(t, s.srv, "/projects/not-a-number/settings", ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET settings (bad id) status = %d, want 404", resp.StatusCode)
	}

	resp = postForm(t, s.srv, base+"/keys", url.Values{}, "", ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST keys (no origin) status = %d, want 403", resp.StatusCode)
	}

	resp = postForm(t, s.srv, base+"/keys", url.Values{}, s.srv.URL, memberCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST keys (member) status = %d, want 403", resp.StatusCode)
	}

	resp = postForm(t, s.srv, base+"/keys/revoke", url.Values{"key_id": {"abc"}}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("POST keys/revoke (bad key_id) status = %d, want 400", resp.StatusCode)
	}

	for _, sub := range []string{"/performance", "/regressions"} {
		resp = postForm(t, s.srv, base+sub, url.Values{}, "", ownerCookie)
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("POST %s (no origin) status = %d, want 403", sub, resp.StatusCode)
		}
		resp = postForm(t, s.srv, base+sub, url.Values{}, s.srv.URL, memberCookie)
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("POST %s (member) status = %d, want 403", sub, resp.StatusCode)
		}
	}

	resp = postForm(t, s.srv, base+"/keys/revoke", url.Values{"key_id": {"1"}}, "", ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST keys/revoke (no origin) status = %d, want 403", resp.StatusCode)
	}

	resp = postForm(t, s.srv, base+"/delete", url.Values{"confirmed": {"yes"}}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST delete status = %d, want 303", resp.StatusCode)
	}
	var queued bool
	if err := s.pool.QueryRow(context.Background(),
		"SELECT EXISTS (SELECT 1 FROM project_purge_queue WHERE project_id = $1)",
		proj.ID).Scan(&queued); err != nil {
		t.Fatalf("чтение очереди: %v", err)
	}
	if !queued {
		t.Fatalf("проект удалён, а заявки на очистку телеметрии нет")
	}
}
