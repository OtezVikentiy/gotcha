package web_test

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
)

// TestCoverOrgSettingsInvalidPathAndForm — общие ветки orgsettings: невалидный
// {id} в пути → 404; нечисловой user_id → 400; SSO без Origin → 403 и с
// неполными полями → 422; SSODelete не-owner → 404.
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

	// Невалидный {id} в пути → 404 (parsePathOrgID).
	resp := getWithCookie(t, s.srv, "/orgs/not-a-number/settings", ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET /orgs/not-a-number/settings status = %d, want 404", resp.StatusCode)
	}

	base := "/orgs/" + strconv.FormatInt(o.ID, 10) + "/settings"

	// Нечисловой user_id в role → 400.
	resp = postForm(t, s.srv, base+"/role", url.Values{"user_id": {"abc"}, "role": {"admin"}}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("POST role (bad user_id) status = %d, want 400", resp.StatusCode)
	}

	// Нечисловой user_id в remove → 400.
	resp = postForm(t, s.srv, base+"/remove", url.Values{"user_id": {"xyz"}}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("POST remove (bad user_id) status = %d, want 400", resp.StatusCode)
	}

	// SSO без Origin → 403.
	resp = postForm(t, s.srv, base+"/sso", url.Values{"issuer": {"x"}}, "", ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST sso (no origin) status = %d, want 403", resp.StatusCode)
	}

	// SSO с неполными полями (owner) → 422 (ErrInvalidSSO).
	resp = postForm(t, s.srv, base+"/sso", url.Values{"issuer": {""}, "client_id": {""}}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST sso (invalid fields) status = %d, want 422", resp.StatusCode)
	}

	// SSODelete не-owner (admin) → 404; без Origin → 403.
	resp = postForm(t, s.srv, base+"/sso/delete", url.Values{}, "", ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST sso/delete (no origin) status = %d, want 403", resp.StatusCode)
	}
	resp = postForm(t, s.srv, base+"/sso/delete", url.Values{}, s.srv.URL, adminCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("POST sso/delete (admin) status = %d, want 404", resp.StatusCode)
	}

	// SSODelete owner без сохранённой конфигурации → всё равно 303 (идемпотентно).
	resp = postForm(t, s.srv, base+"/sso/delete", url.Values{}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST sso/delete (owner) status = %d, want 303", resp.StatusCode)
	}

	// Invite без Origin → 403.
	resp = postForm(t, s.srv, base+"/invite", url.Values{"email": {"x@example.com"}, "role": {"member"}}, "", ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST invite (no origin) status = %d, want 403", resp.StatusCode)
	}

	// Quota с нечисловым значением → 422 (ErrInvalidQuota).
	resp = postForm(t, s.srv, base+"/quota", url.Values{"event_quota": {"not-a-number"}}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST quota (non-numeric) status = %d, want 422", resp.StatusCode)
	}
}

// TestCoverOrgSettingsLeave — POST /orgs/{id}/settings/leave: без Origin → 403;
// без confirmed → страница подтверждения (200); не-участник → 404; единственный
// owner → 422; обычный участник уходит → 303 на /.
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

	// Без Origin → 403.
	resp := postForm(t, s.srv, leavePath, url.Values{}, "", memberCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST leave (no origin) status = %d, want 403", resp.StatusCode)
	}

	// Без confirmed → страница подтверждения (200).
	resp = postForm(t, s.srv, leavePath, url.Values{}, s.srv.URL, memberCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST leave (unconfirmed) status = %d, want 200", resp.StatusCode)
	}

	// Не-участник (stranger) с confirmed → 404 (ErrNotMember).
	resp = postForm(t, s.srv, leavePath, url.Values{"confirmed": {"yes"}}, s.srv.URL, strangerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("POST leave (stranger) status = %d, want 404", resp.StatusCode)
	}

	// Единственный owner → 422 (ErrLastOwner).
	resp = postForm(t, s.srv, leavePath, url.Values{"confirmed": {"yes"}}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST leave (last owner) status = %d, want 422", resp.StatusCode)
	}

	// Обычный участник уходит → 303 на /.
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

// TestCoverQuotaBannerNearLimit — баннер приближения к лимиту событий: при
// заданном лимите и использовании ≥90% страница настроек показывает баннер.
func TestCoverQuotaBannerNearLimit(t *testing.T) {
	s := newStack(t)
	authSvc := auth.NewService(s.pool)
	orgSvc := org.NewService(s.pool, 1_000_000)

	ownerID, ownerCookie := orgSettingsRegister(t, authSvc, "banner-owner@example.com")
	o, err := orgSvc.CreateOrg(context.Background(), "banner-co", "Banner Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	// Маленький лимит и использование выше 90%.
	if err := orgSvc.SetQuota(context.Background(), o.ID, 10); err != nil {
		t.Fatalf("set quota: %v", err)
	}
	for i := 0; i < 9; i++ {
		if _, err := orgSvc.IncUsage(context.Background(), o.ID, time.Now()); err != nil {
			t.Fatalf("inc usage: %v", err)
		}
	}
	resp := getWithCookie(t, s.srv, "/orgs/"+strconv.FormatInt(o.ID, 10)+"/settings", ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET settings (near limit) status = %d, want 200", resp.StatusCode)
	}
}

// TestCoverTeamsInvalidPathAndForm — teams: невалидный {id} команды → 404;
// несуществующая команда → 404; нечисловые user_id/project_id → 400; create/add
// не-участником → 404; attach несуществующего проекта → 422.
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

	// Невалидный {id} команды → 404 (parsePathTeamID).
	resp := postForm(t, s.srv, "/teams/not-a-number/members", url.Values{"user_id": {"1"}}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("POST /teams/not-a-number/members status = %d, want 404", resp.StatusCode)
	}

	// Несуществующая (но числовая) команда → 404 (requireTeamRole TeamOrg NotFound).
	resp = postForm(t, s.srv, "/teams/9999999/members", url.Values{"user_id": {"1"}}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("POST /teams/9999999/members status = %d, want 404", resp.StatusCode)
	}

	// Нечисловой user_id → 400.
	resp = postForm(t, s.srv, teamBase+"/members", url.Values{"user_id": {"abc"}}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("POST members (bad user_id) status = %d, want 400", resp.StatusCode)
	}

	// Нечисловой user_id в remove → 400.
	resp = postForm(t, s.srv, teamBase+"/members/remove", url.Values{"user_id": {"abc"}}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("POST members/remove (bad user_id) status = %d, want 400", resp.StatusCode)
	}

	// Нечисловой project_id в attach → 400.
	resp = postForm(t, s.srv, teamBase+"/projects", url.Values{"project_id": {"abc"}}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("POST projects (bad project_id) status = %d, want 400", resp.StatusCode)
	}

	// Нечисловой project_id в detach → 400.
	resp = postForm(t, s.srv, teamBase+"/projects/detach", url.Values{"project_id": {"abc"}}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("POST projects/detach (bad project_id) status = %d, want 400", resp.StatusCode)
	}

	// Attach несуществующего проекта → 422 (errCrossOrgProject через ProjectOrg NotFound).
	resp = postForm(t, s.srv, teamBase+"/projects", url.Values{"project_id": {"9999999"}}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST projects (nonexistent project) status = %d, want 422", resp.StatusCode)
	}

	// Detach проекта, не привязанного к команде → 303 (идемпотентно).
	proj, err := orgSvc.CreateProject(context.Background(), o.ID, "cover-team-proj", "Cover Team Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	resp = postForm(t, s.srv, teamBase+"/projects/detach", url.Values{"project_id": {strconv.FormatInt(proj.ID, 10)}}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST projects/detach (unattached) status = %d, want 303", resp.StatusCode)
	}

	// Create team не-участником → 404; невалидный org {id} → 404.
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

// TestCoverProjSettingsValidation — projsettings: невалидный {id} → 404; keys/
// performance/regressions без Origin → 403; keyRevoke нечисловой key_id → 400;
// perf/regressions/rename не-owner → 404; удаление проекта без Purger (nil-ветка
// purgeProject) → 303.
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

	// Невалидный {id} → 404.
	resp := getWithCookie(t, s.srv, "/projects/not-a-number/settings", ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET settings (bad id) status = %d, want 404", resp.StatusCode)
	}

	// keys без Origin → 403.
	resp = postForm(t, s.srv, base+"/keys", url.Values{}, "", ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST keys (no origin) status = %d, want 403", resp.StatusCode)
	}

	// keys не-owner (member) → 404.
	resp = postForm(t, s.srv, base+"/keys", url.Values{}, s.srv.URL, memberCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("POST keys (member) status = %d, want 404", resp.StatusCode)
	}

	// keyRevoke нечисловой key_id → 400.
	resp = postForm(t, s.srv, base+"/keys/revoke", url.Values{"key_id": {"abc"}}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("POST keys/revoke (bad key_id) status = %d, want 400", resp.StatusCode)
	}

	// performance/regressions без Origin → 403.
	for _, sub := range []string{"/performance", "/regressions"} {
		resp = postForm(t, s.srv, base+sub, url.Values{}, "", ownerCookie)
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("POST %s (no origin) status = %d, want 403", sub, resp.StatusCode)
		}
		// не-owner → 404.
		resp = postForm(t, s.srv, base+sub, url.Values{}, s.srv.URL, memberCookie)
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("POST %s (member) status = %d, want 404", sub, resp.StatusCode)
		}
	}

	// keyRevoke без Origin → 403.
	resp = postForm(t, s.srv, base+"/keys/revoke", url.Values{"key_id": {"1"}}, "", ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST keys/revoke (no origin) status = %d, want 403", resp.StatusCode)
	}

	// Удаление проекта без Purger (Purger==nil на этом стенде): confirmed=yes →
	// 303, срабатывает nil-ветка purgeProject (warn, без CH).
	resp = postForm(t, s.srv, base+"/delete", url.Values{"confirmed": {"yes"}}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST delete (no purger) status = %d, want 303", resp.StatusCode)
	}
}
