package web_test

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/slo"
)

// TestWebSLODetail — экран деталей SLO: показывает SLO и историю его инцидентов;
// чужой проект и несуществующее SLO дают 404 (без утечки существования).
func TestWebSLODetail(t *testing.T) {
	s := newSLOStack(t, true)
	ctx := context.Background()
	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, "slo-detail-owner@example.com")
	o, err := s.org.CreateOrg(ctx, "slo-d-co", "SLO D Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	project, err := s.org.CreateProject(ctx, o.ID, "slo-d-proj", "SLO D Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	created, err := s.slo.Create(ctx, slo.SLO{
		ProjectID: project.ID, Name: "checkout availability", Kind: slo.SLIAvailability,
		Target: 0.99, WindowDays: 30, BurnThreshold: 14.4, BurnLongMin: 60, BurnShortMin: 5, Enabled: true,
	})
	if err != nil {
		t.Fatalf("seed slo: %v", err)
	}
	// История инцидентов: один открыт-затем-закрыт.
	rem := 0.4
	if _, _, err := s.slo.OpenIncident(ctx, created.ID, project.ID, 22.5, &rem); err != nil {
		t.Fatalf("open incident: %v", err)
	}
	if _, _, err := s.slo.ResolveIncident(ctx, created.ID); err != nil {
		t.Fatalf("resolve incident: %v", err)
	}

	detailPath := "/projects/" + strconv.FormatInt(project.ID, 10) + "/slos/" + strconv.FormatInt(created.ID, 10)

	// Экран показывает имя SLO.
	resp := getWithCookie(t, s.srv, detailPath, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "checkout availability") {
		t.Fatalf("detail missing SLO (status %d): %s", resp.StatusCode, body)
	}

	// Несуществующее SLO в своём проекте → 404.
	missing := "/projects/" + strconv.FormatInt(project.ID, 10) + "/slos/9999999"
	resp = getWithCookie(t, s.srv, missing, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("missing slo status = %d, want 404", resp.StatusCode)
	}

	// Чужой проект (SLO принадлежит другому проекту) → 404.
	other, err := s.org.CreateProject(ctx, o.ID, "slo-d-other", "SLO D Other", "go")
	if err != nil {
		t.Fatalf("create other project: %v", err)
	}
	foreign := "/projects/" + strconv.FormatInt(other.ID, 10) + "/slos/" + strconv.FormatInt(created.ID, 10)
	resp = getWithCookie(t, s.srv, foreign, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("foreign project slo status = %d, want 404", resp.StatusCode)
	}

	// Оператор чужой команды без доступа к проекту → 404.
	memberID, memberCookie := orgSettingsRegister(t, s.auth, "slo-detail-member@example.com")
	if err := s.org.AddMember(ctx, o.ID, memberID, org.RoleMember); err != nil {
		t.Fatalf("add member: %v", err)
	}
	resp = getWithCookie(t, s.srv, detailPath, memberCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("member (no team) detail status = %d, want 404", resp.StatusCode)
	}
}
