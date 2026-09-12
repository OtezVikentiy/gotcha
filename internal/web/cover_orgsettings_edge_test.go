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
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("POST %s (member) = %d, want 403", tc.path, resp.StatusCode)
		}
	}

	resp := postForm(t, s.srv, base+"/remove", url.Values{"user_id": {"1"}}, "", memberCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST remove (no origin) = %d, want 403", resp.StatusCode)
	}

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

	resp := postForm(t, s.srv, base+"/purge-subject", url.Values{
		"project_id": {strconv.FormatInt(proj.ID, 10)}, "email": {"subj@example.com"},
	}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("POST purge-subject (nil purger) = %d, want 503", resp.StatusCode)
	}

	resp = postForm(t, s.srv, base+"/export-subject", url.Values{
		"project_id": {strconv.FormatInt(proj.ID, 10)}, "email": {"subj@example.com"},
	}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("POST export-subject (nil purger) = %d, want 503", resp.StatusCode)
	}

	fp := &fakePurger{}
	s.h.Purger = fp

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

	resp = postForm(t, s.srv, base+"/export-subject", url.Values{
		"project_id": {strconv.FormatInt(proj.ID, 10)}, "user_id": {"user-42"}, "ip": {"10.0.0.1"},
	}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST export-subject (user_id+ip) = %d, want 200", resp.StatusCode)
	}

	resp = postForm(t, s.srv, base+"/delete", url.Values{"confirmed": {"yes"}}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST org delete (with project) = %d, want 303", resp.StatusCode)
	}
	for _, id := range fp.projects {
		if id == proj.ID {
			t.Fatalf("PurgeProject вызван из HTTP-запроса для проекта %d — очистка обязана идти фоновым исполнителем", id)
		}
	}
	var queued bool
	if err := s.pool.QueryRow(ctx,
		"SELECT EXISTS (SELECT 1 FROM project_purge_queue WHERE project_id = $1)",
		proj.ID).Scan(&queued); err != nil {
		t.Fatalf("чтение очереди: %v", err)
	}
	if !queued {
		t.Fatalf("организация удалена, а заявки на очистку телеметрии проекта %d нет", proj.ID)
	}
}
