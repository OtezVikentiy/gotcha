package web_test

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/alert"
	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/escalation"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
)

// TestWebEscalationsPage — owner (оператор проекта) видит редактор двух
// лесенок, member без командного доступа к проекту — 404 (тот же
// existence-oracle, что и alerts/slos: requireProjectOperator).
func TestWebEscalationsPage(t *testing.T) {
	s := newStack(t)
	s.h.EscalationPolicy = escalation.NewPolicyStore(s.pool)
	authSvc := auth.NewService(s.pool)
	orgSvc := org.NewService(s.pool, 1_000_000)

	ownerID, ownerCookie := orgSettingsRegister(t, authSvc, "esc-owner@example.com")
	memberID, memberCookie := orgSettingsRegister(t, authSvc, "esc-member@example.com")

	o, err := orgSvc.CreateOrg(context.Background(), "esc-co", "Esc Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	if err := orgSvc.AddMember(context.Background(), o.ID, memberID, org.RoleMember); err != nil {
		t.Fatalf("add member: %v", err)
	}
	proj, err := orgSvc.CreateProject(context.Background(), o.ID, "esc-proj", "Esc Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	// Канал проекта — без него ни один чекбокс step0_channels не рендерится
	// (только Deliverable-каналы, см. escalations.templ), а форма всё равно
	// обязана показать поля ступеней.
	if _, err := s.h.Alerts.CreateChannel(context.Background(), alert.Channel{
		ProjectID: proj.ID, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://example.com/hook",
	}); err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	path := "/projects/" + strconv.FormatInt(proj.ID, 10) + "/escalations"

	resp := getWithCookie(t, s.srv, path, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s (owner) status = %d, want 200: %s", path, resp.StatusCode, body)
	}
	// Обе severity и фиксированные поля ступеней должны быть в форме.
	for _, want := range []string{"step0_delay", "step0_channels", "severity", "critical", "warning"} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("GET %s missing %q: %s", path, want, body)
		}
	}

	resp = getWithCookie(t, s.srv, path, memberCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET %s (member, no team) status = %d, want 404", path, resp.StatusCode)
	}
}

// TestWebEscalationsSave — сохранение валидной лесенки (две ступени) →
// PolicyStore.Ladder возвращает её; лесенка с дырой в step_no → 422, ничего
// не сохраняется; dry-run-предпросмотр на странице после сохранения содержит
// цель канала (концерн: "содержит названия каналов ступеней").
func TestWebEscalationsSave(t *testing.T) {
	s := newStack(t)
	s.h.EscalationPolicy = escalation.NewPolicyStore(s.pool)
	authSvc := auth.NewService(s.pool)
	orgSvc := org.NewService(s.pool, 1_000_000)

	ownerID, ownerCookie := orgSettingsRegister(t, authSvc, "esc-save-owner@example.com")
	o, err := orgSvc.CreateOrg(context.Background(), "esc-save-co", "Esc Save Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	proj, err := orgSvc.CreateProject(context.Background(), o.ID, "esc-save-proj", "Esc Save Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	path := "/projects/" + strconv.FormatInt(proj.ID, 10) + "/escalations"

	c1, err := s.h.Alerts.CreateChannel(context.Background(), alert.Channel{
		ProjectID: proj.ID, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://example.com/hook-one",
	})
	if err != nil {
		t.Fatalf("CreateChannel c1: %v", err)
	}
	c2, err := s.h.Alerts.CreateChannel(context.Background(), alert.Channel{
		ProjectID: proj.ID, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://example.com/hook-two",
	})
	if err != nil {
		t.Fatalf("CreateChannel c2: %v", err)
	}

	// Валидная лесенка: ступень 0 сразу к c1, ступень 1 через 15 мин к c2.
	valid := url.Values{
		"severity":       {"critical"},
		"step0_delay":    {"0"},
		"step0_channels": {strconv.FormatInt(c1, 10)},
		"step1_delay":    {"15"},
		"step1_channels": {strconv.FormatInt(c2, 10)},
	}
	resp := postForm(t, s.srv, path, valid, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("save valid ladder status = %d, want 303", resp.StatusCode)
	}

	ladder, err := s.h.EscalationPolicy.Ladder(context.Background(), proj.ID, escalation.SeverityCritical)
	if err != nil {
		t.Fatalf("Ladder: %v", err)
	}
	if len(ladder) != 2 || ladder[0].DelayMinutes != 0 || ladder[1].DelayMinutes != 15 ||
		len(ladder[0].ChannelIDs) != 1 || ladder[0].ChannelIDs[0] != c1 ||
		len(ladder[1].ChannelIDs) != 1 || ladder[1].ChannelIDs[0] != c2 {
		t.Fatalf("Ladder(critical) = %+v, want step0->c1, step1(15m)->c2", ladder)
	}

	// Dry-run на странице отражает то, что реально сохранено: цель канала
	// должна быть видна в предпросмотре.
	resp = getWithCookie(t, s.srv, path, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "hook-one") || !strings.Contains(string(body), "hook-two") {
		t.Fatalf("GET %s after save missing dry-run channel targets (status %d): %s", path, resp.StatusCode, body)
	}

	// Дыра в step_no (ступень 0 занята, 1 пустая, 2 занята) → 422, старая
	// лесенка не тронута.
	gap := url.Values{
		"severity":       {"critical"},
		"step0_delay":    {"0"},
		"step0_channels": {strconv.FormatInt(c1, 10)},
		"step2_delay":    {"30"},
		"step2_channels": {strconv.FormatInt(c2, 10)},
	}
	resp = postForm(t, s.srv, path, gap, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("save ladder with gap status = %d, want 422", resp.StatusCode)
	}
	ladder2, err := s.h.EscalationPolicy.Ladder(context.Background(), proj.ID, escalation.SeverityCritical)
	if err != nil {
		t.Fatalf("Ladder after rejected save: %v", err)
	}
	if len(ladder2) != 2 {
		t.Fatalf("Ladder(critical) after rejected save = %+v, want unchanged 2-step ladder", ladder2)
	}
}

// TestWebEscalationsCrossTenant — concern T2: channel_id чужого проекта в
// форме отвергается ДО SetLadder, лесенка не сохраняется. Тот же сценарий,
// что и TestWebAlertsChannelUpdateForeign (edit_forms_test.go), для эскалаций.
func TestWebEscalationsCrossTenant(t *testing.T) {
	s := newStack(t)
	s.h.EscalationPolicy = escalation.NewPolicyStore(s.pool)
	authSvc := auth.NewService(s.pool)
	orgSvc := org.NewService(s.pool, 1_000_000)

	ownerID, ownerCookie := orgSettingsRegister(t, authSvc, "esc-cross-owner@example.com")
	o, err := orgSvc.CreateOrg(context.Background(), "esc-cross-co", "Esc Cross Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	mine, err := orgSvc.CreateProject(context.Background(), o.ID, "esc-cross-mine", "Mine", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	theirs, err := orgSvc.CreateProject(context.Background(), o.ID, "esc-cross-theirs", "Theirs", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	foreignChannel, err := s.h.Alerts.CreateChannel(context.Background(), alert.Channel{
		ProjectID: theirs.ID, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://example.com/foreign-hook",
	})
	if err != nil {
		t.Fatalf("CreateChannel (theirs): %v", err)
	}

	path := "/projects/" + strconv.FormatInt(mine.ID, 10) + "/escalations"
	form := url.Values{
		"severity":       {"critical"},
		"step0_delay":    {"0"},
		"step0_channels": {strconv.FormatInt(foreignChannel, 10)},
	}
	resp := postForm(t, s.srv, path, form, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("save with foreign channel status = %d, want 422", resp.StatusCode)
	}
	ladder, err := s.h.EscalationPolicy.Ladder(context.Background(), mine.ID, escalation.SeverityCritical)
	if err != nil {
		t.Fatalf("Ladder: %v", err)
	}
	// Ничего не настроено -> дефолт-fallback (пустая лесенка проекта mine, у
	// которого нет собственных каналов), а не лесенка с чужим channel_id.
	if len(ladder) != 1 || len(ladder[0].ChannelIDs) != 0 {
		t.Fatalf("Ladder(mine) after rejected cross-tenant save = %+v, want default fallback with no channels", ladder)
	}
}

// TestWebEscalationsNilService — h.EscalationPolicy не проведён (узкий
// тестовый стенд) -> 404, тот же nil-guard, что у alertsPage/slosPage.
func TestWebEscalationsNilService(t *testing.T) {
	s := newStack(t)
	authSvc := auth.NewService(s.pool)
	orgSvc := org.NewService(s.pool, 1_000_000)
	ownerID, ownerCookie := orgSettingsRegister(t, authSvc, "esc-nil-owner@example.com")
	o, _ := orgSvc.CreateOrg(context.Background(), "esc-nil-co", "Esc Nil Co", ownerID)
	proj, _ := orgSvc.CreateProject(context.Background(), o.ID, "esc-nil-proj", "Esc Nil Proj", "go")
	path := "/projects/" + strconv.FormatInt(proj.ID, 10) + "/escalations"

	resp := getWithCookie(t, s.srv, path, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("nil EscalationPolicy status = %d, want 404", resp.StatusCode)
	}
}
