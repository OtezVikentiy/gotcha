package web_test

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/alert"
	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
)

// TestCoverAlertsBranches — недокрытые ветки алертов: bad-id 404, страница
// доставок, sameOrigin 403, невалидные правило/канал → 422, создание/удаление
// канала, чужой/битый channel_id.
func TestCoverAlertsBranches(t *testing.T) {
	s := newStack(t)
	authSvc := auth.NewService(s.pool)
	orgSvc := org.NewService(s.pool, 1_000_000)
	alertSvc := alert.NewService(s.pool)

	ownerID, ownerCookie := orgSettingsRegister(t, authSvc, "cover-alerts-owner@example.com")
	_, memberCookie := orgSettingsRegister(t, authSvc, "cover-alerts-member@example.com")
	o, err := orgSvc.CreateOrg(context.Background(), "cover-alerts-co", "Cover Alerts", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	proj, err := orgSvc.CreateProject(context.Background(), o.ID, "cover-alerts-proj", "Cover Alerts Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	base := "/projects/" + strconv.FormatInt(proj.ID, 10) + "/alerts"

	// Невалидный {id} → 404.
	resp := getWithCookie(t, s.srv, "/projects/not-a-number/alerts", ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET alerts (bad id) status = %d, want 404", resp.StatusCode)
	}

	// Страница доставок (owner) → 200.
	resp = getWithCookie(t, s.srv, base+"/deliveries", ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET deliveries status = %d, want 200", resp.StatusCode)
	}
	// deliveries member → 404.
	resp = getWithCookie(t, s.srv, base+"/deliveries", memberCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET deliveries (member) status = %d, want 404", resp.StatusCode)
	}

	// rules без Origin → 403.
	resp = postForm(t, s.srv, base+"/rules", url.Values{}, "", ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST rules (no origin) status = %d, want 403", resp.StatusCode)
	}

	// rules с невалидным spike (threshold=0) → 422.
	resp = postForm(t, s.srv, base+"/rules", url.Values{
		"spike_enabled": {"on"}, "spike_threshold": {"0"}, "spike_window": {"0"},
	}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST rules (invalid spike) status = %d, want 422", resp.StatusCode)
	}

	// channels без Origin → 403.
	resp = postForm(t, s.srv, base+"/channels", url.Values{"kind": {"email"}, "target": {"ops@example.com"}}, "", ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST channels (no origin) status = %d, want 403", resp.StatusCode)
	}

	// channels невалидный target → 422.
	resp = postForm(t, s.srv, base+"/channels", url.Values{"kind": {"email"}, "target": {"not-an-email"}}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST channels (invalid target) status = %d, want 422", resp.StatusCode)
	}

	// channels валидный → 303.
	resp = postForm(t, s.srv, base+"/channels", url.Values{"kind": {"email"}, "target": {"ops@example.com"}, "enabled": {"on"}}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST channels (valid) status = %d, want 303", resp.StatusCode)
	}

	// channels/delete без Origin → 403.
	resp = postForm(t, s.srv, base+"/channels/delete", url.Values{"channel_id": {"1"}}, "", ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST channels/delete (no origin) status = %d, want 403", resp.StatusCode)
	}

	// channels/delete нечисловой channel_id → 400.
	resp = postForm(t, s.srv, base+"/channels/delete", url.Values{"channel_id": {"abc"}}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("POST channels/delete (bad channel_id) status = %d, want 400", resp.StatusCode)
	}

	// channels/delete чужого канала (другой проект) → 404.
	otherProj, err := orgSvc.CreateProject(context.Background(), o.ID, "cover-alerts-other", "Other", "go")
	if err != nil {
		t.Fatalf("create other project: %v", err)
	}
	foreignChID, err := alertSvc.CreateChannel(context.Background(), alert.Channel{
		ProjectID: otherProj.ID, Kind: alert.ChannelEmail, Enabled: true, Target: "x@example.com",
	})
	if err != nil {
		t.Fatalf("create foreign channel: %v", err)
	}
	resp = postForm(t, s.srv, base+"/channels/delete", url.Values{"channel_id": {strconv.FormatInt(foreignChID, 10)}}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("POST channels/delete (foreign) status = %d, want 404", resp.StatusCode)
	}

	// channels/delete своего канала → 303.
	myChID, err := alertSvc.CreateChannel(context.Background(), alert.Channel{
		ProjectID: proj.ID, Kind: alert.ChannelEmail, Enabled: true, Target: "mine@example.com",
	})
	if err != nil {
		t.Fatalf("create my channel: %v", err)
	}
	resp = postForm(t, s.srv, base+"/channels/delete", url.Values{"channel_id": {strconv.FormatInt(myChID, 10)}}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST channels/delete (mine) status = %d, want 303", resp.StatusCode)
	}
}
