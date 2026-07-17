package web_test

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestWebBannerOnIssuesAndOrgSettings — баннер про ограничение приёма (PROD-P1:
// конец молчаливых потерь). Показывается на странице issues проекта и на
// странице настроек орга, когда за текущий месяц есть отклонённые элементы. При
// нулевых дропах и безлимитном/далёком-от-лимита приёме баннера быть не должно.
func TestWebBannerOnIssuesAndOrgSettings(t *testing.T) {
	s := newIssuesStack(t)

	ownerID, ownerCookie := registerAndLogin(t, s, "banner-owner@example.com")
	project := createProject(t, s, ownerID, "banner-org", "banner-proj")
	orgID, err := s.org.ProjectOrg(context.Background(), project.ID)
	if err != nil {
		t.Fatalf("project org: %v", err)
	}

	issuesPath := "/projects/" + strconv.FormatInt(project.ID, 10) + "/issues"
	orgSettingsPath := "/orgs/" + strconv.FormatInt(orgID, 10) + "/settings"

	// Без дропов и с дефолтным лимитом (usage=0) — баннера нет ни на issues,
	// ни на настройках орга.
	for _, path := range []string{issuesPath, orgSettingsPath} {
		resp := getWithCookie(t, s.srv, path, ownerCookie)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s status = %d, want 200: %s", path, resp.StatusCode, body)
		}
		if strings.Contains(string(body), "quota-banner") {
			t.Fatalf("GET %s: banner shown without drops: %s", path, body)
		}
	}

	// Инкремент дропов за текущий месяц → баннер появляется на обеих страницах.
	if err := s.org.IncDroppedEvents(context.Background(), orgID, time.Now(), 7); err != nil {
		t.Fatalf("inc dropped events: %v", err)
	}

	for _, path := range []string{issuesPath, orgSettingsPath} {
		resp := getWithCookie(t, s.srv, path, ownerCookie)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s status = %d, want 200: %s", path, resp.StatusCode, body)
		}
		if !strings.Contains(string(body), "quota-banner") {
			t.Fatalf("GET %s: banner missing after drops: %s", path, body)
		}
		if !strings.Contains(string(body), "отклонено") {
			t.Fatalf("GET %s: banner text missing after drops: %s", path, body)
		}
	}
}
