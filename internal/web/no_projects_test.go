package web_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
)

// TestNoProjectsPageByRole — тупик «нет доступных проектов» различает роли
// (№21): владельцу/админу — CTA «создайте проект» (ссылка на /projects, там
// модалка), участнику — прежний «попросите администратора». Обоим доступен
// выход: раньше застрявший без проектов не мог даже разлогиниться.
func TestNoProjectsPageByRole(t *testing.T) {
	s := newStack(t)
	authSvc := auth.NewService(s.pool)
	orgSvc := org.NewService(s.pool, 1_000_000)

	ownerID, ownerCookie := orgSettingsRegister(t, authSvc, "noproj-owner@example.com")
	memberID, memberCookie := orgSettingsRegister(t, authSvc, "noproj-member@example.com")
	o, err := orgSvc.CreateOrg(context.Background(), "noproj-co", "NoProj Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	if err := orgSvc.AddMember(context.Background(), o.ID, memberID, org.RoleMember); err != nil {
		t.Fatalf("add member: %v", err)
	}

	fetch := func(cookie *http.Cookie) string {
		t.Helper()
		resp := getWithCookie(t, s.srv, "/", cookie)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET / = %d, want 200", resp.StatusCode)
		}
		body, _ := io.ReadAll(resp.Body)
		return string(body)
	}

	ownerPage := fetch(ownerCookie)
	if !strings.Contains(ownerPage, `href="/projects"`) {
		t.Errorf("владелец без проектов не видит CTA на /projects:\n%s", ownerPage)
	}
	if !strings.Contains(ownerPage, `action="/logout"`) {
		t.Error("на странице владельца нет формы выхода")
	}

	memberPage := fetch(memberCookie)
	if strings.Contains(memberPage, `href="/projects"`) {
		t.Error("участник без прав видит CTA создания проекта")
	}
	if !strings.Contains(memberPage, `action="/logout"`) {
		t.Error("на странице участника нет формы выхода")
	}
}
