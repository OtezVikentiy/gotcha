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
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
)

// TestWebTeamRename — переименование команды через форму: новое имя, прежний
// slug, а на 422 открывается модалка ИМЕННО этой команды.
func TestWebTeamRename(t *testing.T) {
	s := newStack(t)
	authSvc := auth.NewService(s.pool)
	orgSvc := org.NewService(s.pool, 1_000_000)

	ownerID, ownerCookie := orgSettingsRegister(t, authSvc, "rename-web-owner@example.com")
	o, err := orgSvc.CreateOrg(context.Background(), "rename-web-co", "Rename Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	tm, err := orgSvc.CreateTeam(context.Background(), o.ID, "backend", "Бэкенд")
	if err != nil {
		t.Fatalf("create team: %v", err)
	}

	teamsPath := "/orgs/" + strconv.FormatInt(o.ID, 10) + "/teams"
	renamePath := "/teams/" + strconv.FormatInt(tm.ID, 10) + "/rename"

	// Страница показывает ссылку на модалку переименования.
	resp := getWithCookie(t, s.srv, teamsPath, ownerCookie)
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	anchor := "edit-team-" + strconv.FormatInt(tm.ID, 10)
	if !strings.Contains(string(page), anchor) {
		t.Fatalf("teams page has no rename anchor %q", anchor)
	}

	resp = postForm(t, s.srv, renamePath, url.Values{"name": {"Платформа"}}, s.srv.URL, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST rename status = %d, want 303: %s", resp.StatusCode, body)
	}
	teams, err := orgSvc.TeamsOf(context.Background(), o.ID)
	if err != nil || len(teams) != 1 || teams[0].Name != "Платформа" || teams[0].Slug != "backend" {
		t.Fatalf("teams after rename = %+v err=%v, want renamed with slug kept", teams, err)
	}

	// Пустое имя -> 422 и модалка этой команды открыта, а не форма создания.
	resp = postForm(t, s.srv, renamePath, url.Values{"name": {"  "}}, s.srv.URL, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST blank name status = %d, want 422: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), `id="`+anchor+`" class="modal modal--open"`) {
		t.Fatalf("rename modal not reopened:\n%s", body)
	}
	if strings.Contains(string(body), `id="new-team" class="modal modal--open"`) {
		t.Fatalf("create modal opened instead of the rename one:\n%s", body)
	}
}

// TestWebTeamRenameForbiddenForOutsider — переименование команды чужой
// организации отдаёт 404: скоуп тот же, что и у остальных действий над
// командой (requireTeamRole).
func TestWebTeamRenameForbiddenForOutsider(t *testing.T) {
	s := newStack(t)
	authSvc := auth.NewService(s.pool)
	orgSvc := org.NewService(s.pool, 1_000_000)

	ownerID, _ := orgSettingsRegister(t, authSvc, "rename-web-victim@example.com")
	_, outsiderCookie := orgSettingsRegister(t, authSvc, "rename-web-outsider@example.com")
	o, err := orgSvc.CreateOrg(context.Background(), "rename-web-victim-co", "Victim Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	tm, err := orgSvc.CreateTeam(context.Background(), o.ID, "backend", "Бэкенд")
	if err != nil {
		t.Fatalf("create team: %v", err)
	}

	resp := postForm(t, s.srv, "/teams/"+strconv.FormatInt(tm.ID, 10)+"/rename",
		url.Values{"name": {"Hijacked"}}, s.srv.URL, outsiderCookie)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("outsider rename status = %d, want 404", resp.StatusCode)
	}
	teams, err := orgSvc.TeamsOf(context.Background(), o.ID)
	if err != nil || len(teams) != 1 || teams[0].Name != "Бэкенд" {
		t.Fatalf("victim team = %+v err=%v, want untouched", teams, err)
	}
}

// TestWebAlertsChannelUpdate — правка канала доставки через форму: адрес и
// включённость меняются, пустой секрет оставляет прежний, а на 422 открывается
// модалка именно этого канала с введёнными значениями.
func TestWebAlertsChannelUpdate(t *testing.T) {
	s := newStack(t)
	authSvc := auth.NewService(s.pool)
	orgSvc := org.NewService(s.pool, 1_000_000)

	ownerID, ownerCookie := orgSettingsRegister(t, authSvc, "chanupd-web@example.com")
	o, err := orgSvc.CreateOrg(context.Background(), "chanupd-web-co", "Chan Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	proj, err := orgSvc.CreateProject(context.Background(), o.ID, "chanupd-proj", "Chan Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	alertsPath := "/projects/" + strconv.FormatInt(proj.ID, 10) + "/alerts"
	resp := postForm(t, s.srv, alertsPath+"/channels", url.Values{
		"kind": {"webhook"}, "target": {"https://example.com/hook"}, "enabled": {"on"},
	}, s.srv.URL, ownerCookie)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("create channel status = %d, want 303", resp.StatusCode)
	}
	channels, err := s.h.Alerts.Channels(context.Background(), proj.ID)
	if err != nil || len(channels) != 1 {
		t.Fatalf("Channels = %+v err=%v, want one", channels, err)
	}
	chID := channels[0].ID

	// Страница показывает ссылку на модалку правки этого канала.
	resp = getWithCookie(t, s.srv, alertsPath, ownerCookie)
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	anchor := "edit-channel-" + strconv.FormatInt(chID, 10)
	if !strings.Contains(string(page), anchor) {
		t.Fatalf("alerts page has no edit anchor %q", anchor)
	}

	// Правка: другой адрес, канал выключается.
	resp = postForm(t, s.srv, alertsPath+"/channels/update", url.Values{
		"channel_id": {strconv.FormatInt(chID, 10)},
		"target":     {"https://example.com/hook-v2"},
	}, s.srv.URL, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("update channel status = %d, want 303: %s", resp.StatusCode, body)
	}
	channels, err = s.h.Alerts.Channels(context.Background(), proj.ID)
	if err != nil || len(channels) != 1 {
		t.Fatalf("Channels = %+v err=%v", channels, err)
	}
	if channels[0].Target != "https://example.com/hook-v2" || channels[0].Enabled {
		t.Fatalf("channel after update = %+v, want new target and disabled", channels[0])
	}

	// Невалидный адрес -> 422, модалка этого канала открыта, введённое на месте.
	resp = postForm(t, s.srv, alertsPath+"/channels/update", url.Values{
		"channel_id": {strconv.FormatInt(chID, 10)},
		"target":     {"not-a-url"},
		"enabled":    {"on"},
	}, s.srv.URL, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("update with bad target status = %d, want 422: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), `id="`+anchor+`" class="modal modal--open"`) {
		t.Fatalf("edit modal not reopened:\n%s", body)
	}
	if strings.Contains(string(body), `id="new-channel" class="modal modal--open"`) {
		t.Fatalf("create modal opened instead of the edit one:\n%s", body)
	}
	if !strings.Contains(string(body), `value="not-a-url"`) {
		t.Fatalf("entered target not returned to the form:\n%s", body)
	}
}

// TestWebAlertsChannelUpdateForeign — канал чужого проекта не правится по
// подобранному id: 404, как и у удаления.
func TestWebAlertsChannelUpdateForeign(t *testing.T) {
	s := newStack(t)
	authSvc := auth.NewService(s.pool)
	orgSvc := org.NewService(s.pool, 1_000_000)

	ownerID, ownerCookie := orgSettingsRegister(t, authSvc, "chanupd-foreign@example.com")
	o, err := orgSvc.CreateOrg(context.Background(), "chanupd-foreign-co", "Chan Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	mine, err := orgSvc.CreateProject(context.Background(), o.ID, "chanupd-mine", "Mine", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	theirs, err := orgSvc.CreateProject(context.Background(), o.ID, "chanupd-theirs", "Theirs", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	chID, err := s.h.Alerts.CreateChannel(context.Background(), alert.Channel{
		ProjectID: theirs.ID, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://example.com/hook",
	})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}

	resp := postForm(t, s.srv, "/projects/"+strconv.FormatInt(mine.ID, 10)+"/alerts/channels/update",
		url.Values{"channel_id": {strconv.FormatInt(chID, 10)}, "target": {"https://evil.example/hook"}},
		s.srv.URL, ownerCookie)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("update foreign channel status = %d, want 404", resp.StatusCode)
	}
	channels, err := s.h.Alerts.Channels(context.Background(), theirs.ID)
	if err != nil || len(channels) != 1 || channels[0].Target != "https://example.com/hook" {
		t.Fatalf("victim channel = %+v err=%v, want untouched", channels, err)
	}
}
