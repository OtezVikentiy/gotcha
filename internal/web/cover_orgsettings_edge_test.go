package web_test

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
)

// TestCoverOrgSettingsMemberPostsAndSameOrigin — member (не owner/admin) POST'ы
// role/remove/invite → 404 (requireOrgRole); remove/invite-accept без Origin → 403.
func TestCoverOrgSettingsMemberPostsAndSameOrigin(t *testing.T) {
	s := newStack(t)
	authSvc := auth.NewService(s.pool)
	orgSvc := org.NewService(s.pool, 1_000_000)
	ctx := context.Background()

	ownerID, _ := orgSettingsRegister(t, authSvc, "edge-owner@example.com")
	memberID, memberCookie := orgSettingsRegister(t, authSvc, "edge-member@example.com")
	o, err := orgSvc.CreateOrg(ctx, "edge-co", "Edge Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	if err := orgSvc.AddMember(ctx, o.ID, memberID, org.RoleMember); err != nil {
		t.Fatalf("add member: %v", err)
	}
	base := "/orgs/" + strconv.FormatInt(o.ID, 10) + "/settings"

	// member POST role/remove/invite → 404 (requireOrgRole).
	for _, tc := range []struct {
		path string
		form url.Values
	}{
		{base + "/role", url.Values{"user_id": {strconv.FormatInt(ownerID, 10)}, "role": {"member"}}},
		{base + "/remove", url.Values{"user_id": {strconv.FormatInt(ownerID, 10)}}},
		{base + "/invite", url.Values{"email": {"x@example.com"}, "role": {"member"}}},
	} {
		resp := postForm(t, s.srv, tc.path, tc.form, s.srv.URL, memberCookie)
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("POST %s (member) = %d, want 404", tc.path, resp.StatusCode)
		}
	}

	// remove без Origin → 403.
	resp := postForm(t, s.srv, base+"/remove", url.Values{"user_id": {"1"}}, "", memberCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST remove (no origin) = %d, want 403", resp.StatusCode)
	}

	// invite-accept без Origin → 403.
	token, err := orgSvc.Invite(ctx, o.ID, "edge-invited@example.com", org.RoleMember)
	if err != nil {
		t.Fatalf("invite: %v", err)
	}
	resp = postForm(t, s.srv, "/invite/"+token, url.Values{}, "", memberCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST invite-accept (no origin) = %d, want 403", resp.StatusCode)
	}
}

// TestCoverOrgPurgeExportPurgerBranches — purge/export без Purger (nil-ветки:
// purge → 303 best-effort, export → 503), удаление орга С проектом (цикл CH-очистки
// проектов), export с user_id+ip (subjectCriteria).
func TestCoverOrgPurgeExportPurgerBranches(t *testing.T) {
	s := newStack(t)
	authSvc := auth.NewService(s.pool)
	orgSvc := org.NewService(s.pool, 1_000_000)
	ctx := context.Background()

	ownerID, ownerCookie := orgSettingsRegister(t, authSvc, "purger-owner@example.com")
	o, err := orgSvc.CreateOrg(ctx, "purger-co", "Purger Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	proj, err := orgSvc.CreateProject(ctx, o.ID, "purger-proj", "Purger Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	base := "/orgs/" + strconv.FormatInt(o.ID, 10) + "/settings"

	// Purger не задан (nil) на этом стенде.
	// purge-subject валидный субъект → 303 (best-effort, ветка nil-Purger).
	resp := postForm(t, s.srv, base+"/purge-subject", url.Values{
		"project_id": {strconv.FormatInt(proj.ID, 10)}, "email": {"subj@example.com"},
	}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST purge-subject (nil purger) = %d, want 303", resp.StatusCode)
	}

	// export-subject без Purger → 503.
	resp = postForm(t, s.srv, base+"/export-subject", url.Values{
		"project_id": {strconv.FormatInt(proj.ID, 10)}, "email": {"subj@example.com"},
	}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("POST export-subject (nil purger) = %d, want 503", resp.StatusCode)
	}

	// Теперь с fakePurger.
	fp := &fakePurger{}
	s.h.Purger = fp

	// purge-subject cross-org (проект чужого орга) → 404.
	otherOwner, _ := orgSettingsRegister(t, authSvc, "purger-other@example.com")
	other, err := orgSvc.CreateOrg(ctx, "purger-other-co", "Other", otherOwner)
	if err != nil {
		t.Fatalf("create other org: %v", err)
	}
	otherProj, err := orgSvc.CreateProject(ctx, other.ID, "purger-other-proj", "Other Proj", "go")
	if err != nil {
		t.Fatalf("create other project: %v", err)
	}
	resp = postForm(t, s.srv, base+"/purge-subject", url.Values{
		"project_id": {strconv.FormatInt(otherProj.ID, 10)}, "email": {"x@example.com"},
	}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("POST purge-subject (cross-org) = %d, want 404", resp.StatusCode)
	}

	// export с user_id+ip (без email) → 200, subjectCriteria покрывает обе ветки.
	resp = postForm(t, s.srv, base+"/export-subject", url.Values{
		"project_id": {strconv.FormatInt(proj.ID, 10)}, "user_id": {"user-42"}, "ip": {"10.0.0.1"},
	}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST export-subject (user_id+ip) = %d, want 200", resp.StatusCode)
	}

	// Удаление орга С проектом → 303, цикл CH-очистки проектов вызвал PurgeProject.
	resp = postForm(t, s.srv, base+"/delete", url.Values{"confirmed": {"yes"}}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST org delete (with project) = %d, want 303", resp.StatusCode)
	}
	found := false
	for _, id := range fp.projects {
		if id == proj.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("PurgeProject not called for project %d during org delete: %v", proj.ID, fp.projects)
	}
}
