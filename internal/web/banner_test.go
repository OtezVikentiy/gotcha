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
