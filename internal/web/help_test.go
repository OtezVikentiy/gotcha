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
)

// TestHelpPanel — задача 4 (docs-onboarding): страницы-хабы (issues, alerts,
// ...) показывают свёрнутую по умолчанию контекстную справку helpPanel сразу
// под <h1> — нативный <details class="help-panel"> без JS (CSP без
// unsafe-inline). Проверяем разметку и RU-текст заголовка панели хотя бы для
// двух областей.
func TestHelpPanel(t *testing.T) {
	s := newStack(t)
	authSvc := auth.NewService(s.pool)
	orgSvc := org.NewService(s.pool, 1_000_000)

	ownerID, ownerCookie := orgSettingsRegister(t, authSvc, "helppanel-owner@example.com")
	o, err := orgSvc.CreateOrg(context.Background(), "helppanel-co", "HelpPanel Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	proj, err := orgSvc.CreateProject(context.Background(), o.ID, "helppanel-proj", "HelpPanel Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	projPath := "/projects/" + strconv.FormatInt(proj.ID, 10)

	// Issues: панель под <h1>, свёрнута (нативный <details>), ссылка на
	// /docs/issues.
	resp := getWithCookie(t, s.srv, projPath+"/issues", ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s/issues status = %d, want 200: %s", projPath, resp.StatusCode, body)
	}
	if !strings.Contains(string(body), `class="help-panel"`) {
		t.Fatalf("GET %s/issues body missing help-panel: %s", projPath, body)
	}
	if !strings.Contains(string(body), "Что это за раздел?") {
		t.Fatalf("GET %s/issues body missing help.issues.title (RU): %s", projPath, body)
	}
	if !strings.Contains(string(body), `href="/docs/issues"`) {
		t.Fatalf("GET %s/issues body missing link to /docs/issues: %s", projPath, body)
	}

	// Alerts: та же панель, другая область.
	resp = getWithCookie(t, s.srv, projPath+"/alerts", ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s/alerts status = %d, want 200: %s", projPath, resp.StatusCode, body)
	}
	if !strings.Contains(string(body), `class="help-panel"`) {
		t.Fatalf("GET %s/alerts body missing help-panel: %s", projPath, body)
	}
	if !strings.Contains(string(body), `href="/docs/alerts"`) {
		t.Fatalf("GET %s/alerts body missing link to /docs/alerts: %s", projPath, body)
	}
}
