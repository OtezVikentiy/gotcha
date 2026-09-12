package web_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
)

func TestWebIssuesEmptyStateCTA(t *testing.T) {
	s := newIssuesStack(t)

	ownerID, ownerCookie := registerAndLogin(t, s, "empty-cta-owner@example.com")
	project := createProject(t, s, ownerID, "empty-cta-org", "empty-cta-proj")

	issuesPath := "/projects/" + strconv.FormatInt(project.ID, 10) + "/issues"
	setupPath := "/projects/" + strconv.FormatInt(project.ID, 10) + "/setup"

	resp := getWithCookie(t, s.srv, issuesPath, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200: %s", issuesPath, resp.StatusCode, body)
	}
	if !strings.Contains(string(body), setupPath) {
		t.Fatalf("GET %s (empty) missing setup CTA link %q: %s", issuesPath, setupPath, body)
	}
	if !strings.Contains(string(body), "Подключите DSN") {
		t.Fatalf("GET %s (empty) missing CTA text: %s", issuesPath, body)
	}
}

func TestWebProjectSettingsRevokeConfirm(t *testing.T) {
	s := newStack(t)
	authSvc := auth.NewService(s.pool)
	orgSvc := org.NewService(s.pool, 1_000_000)

	ownerID, ownerCookie := orgSettingsRegister(t, authSvc, "revoke-confirm-owner@example.com")
	o, err := orgSvc.CreateOrg(context.Background(), "revoke-confirm-co", "Revoke Confirm Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	proj, err := orgSvc.CreateProject(context.Background(), o.ID, "revoke-confirm-proj", "Revoke Confirm Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	settingsPath := "/projects/" + strconv.FormatInt(proj.ID, 10) + "/settings"
	keysPath := settingsPath + "/keys"
	revokePath := keysPath + "/revoke"

	resp := postForm(t, s.srv, keysPath, url.Values{"kind": {"server"}}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST %s (create key) status = %d, want 303", keysPath, resp.StatusCode)
	}

	resp = getWithCookie(t, s.srv, settingsPath, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200: %s", settingsPath, resp.StatusCode, body)
	}
	if strings.Contains(string(body), "confirm(") {
		t.Fatalf("GET %s revoke button still carries dead inline confirm(): %s", settingsPath, body)
	}
	keys, err := orgSvc.KeysForProject(context.Background(), proj.ID)
	if err != nil || len(keys) != 1 {
		t.Fatalf("KeysForProject = %+v, err=%v, want 1 key", keys, err)
	}
	keyID := keys[0].ID

	resp = postForm(t, s.srv, revokePath, url.Values{"key_id": {strconv.FormatInt(keyID, 10)}}, s.srv.URL, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST %s (unconfirmed, sole key) status = %d, want 200: %s", revokePath, resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "последний активный ключ") {
		t.Fatalf("POST %s (unconfirmed, sole key) missing last-of-kind warning: %s", revokePath, body)
	}
	if strings.Contains(string(body), "получат отказ при приёме") {
		t.Fatalf("POST %s (unconfirmed, sole key) unexpectedly shows the ordinary message: %s", revokePath, body)
	}
	if !strings.Contains(string(body), `name="confirmed" value="yes"`) {
		t.Fatalf("POST %s (unconfirmed, sole key) missing confirmed hidden field: %s", revokePath, body)
	}
	if keys, err := orgSvc.KeysForProject(context.Background(), proj.ID); err != nil || keyRevoked(keys, keyID) {
		t.Fatalf("key revoked by unconfirmed POST: %+v err=%v", keys, err)
	}

	resp = postForm(t, s.srv, keysPath, url.Values{"kind": {"server"}}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST %s (create second key) status = %d, want 303", keysPath, resp.StatusCode)
	}

	resp = postForm(t, s.srv, revokePath, url.Values{"key_id": {strconv.FormatInt(keyID, 10)}}, s.srv.URL, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST %s (unconfirmed, paired key) status = %d, want 200: %s", revokePath, resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "получат отказ при приёме") {
		t.Fatalf("POST %s (unconfirmed, paired key) missing ordinary confirm message: %s", revokePath, body)
	}
	if strings.Contains(string(body), "последний активный ключ") {
		t.Fatalf("POST %s (unconfirmed, paired key) unexpectedly shows last-of-kind warning: %s", revokePath, body)
	}
	if !strings.Contains(string(body), `name="confirmed" value="yes"`) {
		t.Fatalf("POST %s (unconfirmed, paired key) missing confirmed hidden field: %s", revokePath, body)
	}
	if keys, err := orgSvc.KeysForProject(context.Background(), proj.ID); err != nil || keyRevoked(keys, keyID) {
		t.Fatalf("key revoked by unconfirmed POST: %+v err=%v", keys, err)
	}

	resp = postForm(t, s.srv, revokePath, url.Values{"key_id": {strconv.FormatInt(keyID, 10)}, "confirmed": {"yes"}}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST %s (confirmed) status = %d, want 303", revokePath, resp.StatusCode)
	}
	if keys, err := orgSvc.KeysForProject(context.Background(), proj.ID); err != nil || !keyRevoked(keys, keyID) {
		t.Fatalf("key not revoked after confirmed POST: %+v err=%v", keys, err)
	}
}

// Позиционный keys[0].Revoked не надёжен, когда ключей несколько.
func keyRevoked(keys []org.Key, keyID int64) bool {
	for _, k := range keys {
		if k.ID == keyID {
			return k.Revoked
		}
	}
	return false
}

func TestWebProjectSettingsRetentionNotice(t *testing.T) {
	s := newStack(t)
	authSvc := auth.NewService(s.pool)
	orgSvc := org.NewService(s.pool, 1_000_000)

	ownerID, ownerCookie := orgSettingsRegister(t, authSvc, "retention-owner@example.com")
	o, err := orgSvc.CreateOrg(context.Background(), "retention-co", "Retention Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	proj, err := orgSvc.CreateProject(context.Background(), o.ID, "retention-proj", "Retention Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	settingsPath := "/projects/" + strconv.FormatInt(proj.ID, 10) + "/settings"

	resp := getWithCookie(t, s.srv, settingsPath, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200: %s", settingsPath, resp.StatusCode, body)
	}
	if strings.Contains(string(body), "хранятся") {
		t.Fatalf("GET %s shows retention notice with RetentionDays=0: %s", settingsPath, body)
	}

	s.h.RetentionDays = 30
	resp = getWithCookie(t, s.srv, settingsPath, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200: %s", settingsPath, resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "хранятся") || !strings.Contains(string(body), "30") {
		t.Fatalf("GET %s missing retention notice: %s", settingsPath, body)
	}
}

func TestWebOrgSettingsLeave(t *testing.T) {
	s := newStack(t)
	authSvc := auth.NewService(s.pool)
	orgSvc := org.NewService(s.pool, 1_000_000)

	ownerID, ownerCookie := orgSettingsRegister(t, authSvc, "leave-owner@example.com")
	memberID, memberCookie := orgSettingsRegister(t, authSvc, "leave-member@example.com")

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
		t.Fatalf("POST %s (no origin) status = %d, want 403", leavePath, resp.StatusCode)
	}

	resp = postForm(t, s.srv, leavePath, url.Values{}, s.srv.URL, memberCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST %s (member, unconfirmed) status = %d, want 200: %s", leavePath, resp.StatusCode, body)
	}
	if !strings.Contains(string(body), `name="confirmed" value="yes"`) {
		t.Fatalf("POST %s (member, unconfirmed) missing confirm page hidden field: %s", leavePath, body)
	}
	if role, err := orgSvc.Role(context.Background(), o.ID, memberID); err != nil || role != org.RoleMember {
		t.Fatalf("member role after unconfirmed leave = %v, %v, want member, nil", role, err)
	}

	resp = postForm(t, s.srv, leavePath, url.Values{"confirmed": {"yes"}}, s.srv.URL, memberCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST %s (member) status = %d, want 303", leavePath, resp.StatusCode)
	}
	if _, err := orgSvc.Role(context.Background(), o.ID, memberID); !errors.Is(err, org.ErrNotMember) {
		t.Fatalf("member role after leave: got %v, want ErrNotMember", err)
	}

	resp = postForm(t, s.srv, leavePath, url.Values{"confirmed": {"yes"}}, s.srv.URL, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST %s (last owner) status = %d, want 422: %s", leavePath, resp.StatusCode, body)
	}
	if role, err := orgSvc.Role(context.Background(), o.ID, ownerID); err != nil || role != org.RoleOwner {
		t.Fatalf("owner role after blocked leave = %v, %v, want owner, nil", role, err)
	}

	_, strangerCookie := orgSettingsRegister(t, authSvc, "leave-stranger@example.com")
	resp = postForm(t, s.srv, leavePath, url.Values{"confirmed": {"yes"}}, s.srv.URL, strangerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("POST %s (stranger) status = %d, want 404", leavePath, resp.StatusCode)
	}
}
