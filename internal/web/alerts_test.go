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

	"gitflic.ru/otezvikentiy/gotcha/internal/alert"
	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/notify"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
)

// TestWebAlertsRules — сквозной сценарий задачи 5 (алерты, часть 1): owner
// видит страницу правил, member — 404, форма сохраняет все три kind разом
// (UpsertRule), невалидный spike (без threshold) → 422 без сохранения.
func TestWebAlertsRules(t *testing.T) {
	s := newStack(t)
	authSvc := auth.NewService(s.pool)
	orgSvc := org.NewService(s.pool, 1_000_000)
	alertSvc := alert.NewService(s.pool)

	ownerID, ownerCookie := orgSettingsRegister(t, authSvc, "alerts-owner@example.com")
	memberID, memberCookie := orgSettingsRegister(t, authSvc, "alerts-member@example.com")

	o, err := orgSvc.CreateOrg(context.Background(), "alerts-co", "Alerts Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	if err := orgSvc.AddMember(context.Background(), o.ID, memberID, org.RoleMember); err != nil {
		t.Fatalf("add member: %v", err)
	}
	proj, err := orgSvc.CreateProject(context.Background(), o.ID, "alerts-proj", "Alerts Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	alertsPath := "/projects/" + strconv.FormatInt(proj.ID, 10) + "/alerts"
	rulesPath := alertsPath + "/rules"

	// GET owner -> 200, все три kind представлены в форме.
	resp := getWithCookie(t, s.srv, alertsPath, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s (owner) status = %d, want 200: %s", alertsPath, resp.StatusCode, body)
	}
	for _, want := range []string{"new_issue_enabled", "regression_enabled", "spike_enabled", "spike_threshold", "spike_window"} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("GET %s missing field %q: %s", alertsPath, want, body)
		}
	}

	// GET member (не owner/admin) -> 404.
	resp = getWithCookie(t, s.srv, alertsPath, memberCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET %s (member) status = %d, want 404", alertsPath, resp.StatusCode)
	}

	validForm := url.Values{
		"new_issue_enabled":   {"on"},
		"new_issue_throttle":  {"15"},
		"regression_enabled":  {"on"},
		"regression_throttle": {"20"},
		"spike_enabled":       {"on"},
		"spike_threshold":     {"5"},
		"spike_window":        {"10"},
		"spike_throttle":      {"30"},
	}

	// POST rules без Origin -> 403.
	resp = postForm(t, s.srv, rulesPath, validForm, "", ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST %s (no origin) status = %d, want 403", rulesPath, resp.StatusCode)
	}

	// POST rules member -> 404.
	resp = postForm(t, s.srv, rulesPath, validForm, s.srv.URL, memberCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("POST %s (member) status = %d, want 404", rulesPath, resp.StatusCode)
	}

	// POST rules валидный -> 303, все три правила сохранены.
	resp = postForm(t, s.srv, rulesPath, validForm, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST %s status = %d, want 303", rulesPath, resp.StatusCode)
	}
	rules, err := alertSvc.Rules(context.Background(), proj.ID)
	if err != nil || len(rules) != 3 {
		t.Fatalf("Rules after valid save = %+v, err=%v, want 3", rules, err)
	}
	byKind := map[string]alert.Rule{}
	for _, r := range rules {
		byKind[r.Kind] = r
	}
	if r := byKind[alert.KindNewIssue]; !r.Enabled || r.ThrottleMinutes != 15 {
		t.Errorf("new_issue rule = %+v, want enabled throttle=15", r)
	}
	if r := byKind[alert.KindRegression]; !r.Enabled || r.ThrottleMinutes != 20 {
		t.Errorf("regression rule = %+v, want enabled throttle=20", r)
	}
	if r := byKind[alert.KindSpike]; !r.Enabled || r.Threshold != 5 || r.WindowMinutes != 10 || r.ThrottleMinutes != 30 {
		t.Errorf("spike rule = %+v, want enabled threshold=5 window=10 throttle=30", r)
	}

	// POST rules с невалидным spike (threshold=0) -> 422, spike-правило не
	// перезаписано (остаётся threshold=5/window=10 из предыдущего успешного
	// сохранения).
	invalidForm := url.Values{
		"new_issue_enabled":   {"on"},
		"new_issue_throttle":  {"15"},
		"regression_enabled":  {"on"},
		"regression_throttle": {"20"},
		"spike_enabled":       {"on"},
		"spike_threshold":     {"0"},
		"spike_window":        {"10"},
		"spike_throttle":      {"30"},
	}
	resp = postForm(t, s.srv, rulesPath, invalidForm, s.srv.URL, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST %s (spike no threshold) status = %d, want 422: %s", rulesPath, resp.StatusCode, body)
	}
	rules, err = alertSvc.Rules(context.Background(), proj.ID)
	if err != nil || len(rules) != 3 {
		t.Fatalf("Rules after invalid save = %+v, err=%v, want 3", rules, err)
	}
	byKind = map[string]alert.Rule{}
	for _, r := range rules {
		byKind[r.Kind] = r
	}
	if r := byKind[alert.KindSpike]; r.Threshold != 5 || r.WindowMinutes != 10 {
		t.Errorf("spike rule after invalid POST = %+v, want unchanged threshold=5 window=10", r)
	}
}

// TestWebAlertsChannels — каналы доставки: создание (email/webhook/telegram),
// невалидный канал → 422, удаление, чужой channel_id → 404, member → 404.
func TestWebAlertsChannels(t *testing.T) {
	s := newStack(t)
	authSvc := auth.NewService(s.pool)
	orgSvc := org.NewService(s.pool, 1_000_000)
	alertSvc := alert.NewService(s.pool)

	ownerID, ownerCookie := orgSettingsRegister(t, authSvc, "alertchan-owner@example.com")
	memberID, memberCookie := orgSettingsRegister(t, authSvc, "alertchan-member@example.com")

	o, err := orgSvc.CreateOrg(context.Background(), "alertchan-co", "AlertChan Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	if err := orgSvc.AddMember(context.Background(), o.ID, memberID, org.RoleMember); err != nil {
		t.Fatalf("add member: %v", err)
	}
	proj, err := orgSvc.CreateProject(context.Background(), o.ID, "alertchan-proj", "AlertChan Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	alertsPath := "/projects/" + strconv.FormatInt(proj.ID, 10) + "/alerts"
	channelsPath := alertsPath + "/channels"
	channelsDeletePath := channelsPath + "/delete"

	// POST channels member -> 404.
	resp := postForm(t, s.srv, channelsPath, url.Values{"kind": {"email"}, "target": {"ops@example.com"}, "enabled": {"on"}}, s.srv.URL, memberCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("POST %s (member) status = %d, want 404", channelsPath, resp.StatusCode)
	}

	// POST channels невалидный email -> 422.
	resp = postForm(t, s.srv, channelsPath, url.Values{"kind": {"email"}, "target": {"not-an-email"}, "enabled": {"on"}}, s.srv.URL, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST %s (bad email) status = %d, want 422: %s", channelsPath, resp.StatusCode, body)
	}

	// POST channels email валидный -> 303, канал создан.
	resp = postForm(t, s.srv, channelsPath, url.Values{"kind": {"email"}, "target": {"ops@example.com"}, "enabled": {"on"}}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST %s (email) status = %d, want 303", channelsPath, resp.StatusCode)
	}

	// POST channels webhook валидный -> 303.
	resp = postForm(t, s.srv, channelsPath, url.Values{"kind": {"webhook"}, "target": {"https://example.com/hook"}, "secret": {"sig-secret"}, "enabled": {"on"}}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST %s (webhook) status = %d, want 303", channelsPath, resp.StatusCode)
	}

	channels, err := alertSvc.Channels(context.Background(), proj.ID)
	if err != nil || len(channels) != 2 {
		t.Fatalf("Channels after create = %+v, err=%v, want 2", channels, err)
	}

	// GET показывает оба канала.
	resp = getWithCookie(t, s.srv, alertsPath, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "ops@example.com") || !strings.Contains(string(body), "https://example.com/hook") {
		t.Fatalf("GET %s missing channel targets: %s", alertsPath, body)
	}

	// Удаление ЧУЖОГО channel_id (другой проект) -> 404, канал не тронут.
	otherProj, err := orgSvc.CreateProject(context.Background(), o.ID, "alertchan-other", "Other Proj", "go")
	if err != nil {
		t.Fatalf("create other project: %v", err)
	}
	otherChanID, err := alertSvc.CreateChannel(context.Background(), alert.Channel{
		ProjectID: otherProj.ID, Kind: alert.ChannelEmail, Enabled: true, Target: "other@example.com",
	})
	if err != nil {
		t.Fatalf("create other channel: %v", err)
	}
	resp = postForm(t, s.srv, channelsDeletePath, url.Values{"channel_id": {strconv.FormatInt(otherChanID, 10)}}, s.srv.URL, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("POST %s (foreign channel) status = %d, want 404: %s", channelsDeletePath, resp.StatusCode, body)
	}
	if c2, err := alertSvc.Channels(context.Background(), otherProj.ID); err != nil || len(c2) != 1 {
		t.Fatalf("other project's channel affected unexpectedly: %+v err=%v", c2, err)
	}

	// Удаление member -> 404.
	resp = postForm(t, s.srv, channelsDeletePath, url.Values{"channel_id": {strconv.FormatInt(channels[0].ID, 10)}}, s.srv.URL, memberCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("POST %s (member) status = %d, want 404", channelsDeletePath, resp.StatusCode)
	}

	// Удаление своего канала -> 303, канал исчез.
	resp = postForm(t, s.srv, channelsDeletePath, url.Values{"channel_id": {strconv.FormatInt(channels[0].ID, 10)}}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST %s status = %d, want 303", channelsDeletePath, resp.StatusCode)
	}
	channels, err = alertSvc.Channels(context.Background(), proj.ID)
	if err != nil || len(channels) != 1 {
		t.Fatalf("Channels after delete = %+v, err=%v, want 1", channels, err)
	}
}

// TestWebAlertDeliveriesPageShowsFailedDeliveries — spec §7: failed-
// уведомления должны быть видны в UI, не только в логах воркера. Живут на
// отдельной странице /alerts/deliveries (вынесена из основной страницы
// алертов — UI-фидбек: секция делала страницу алертов слишком длинной).
// Заводим канал + одну failed-запись в notification_outbox напрямую
// (notify-пакет, как и web-тесты, не создаёт для этого отдельного
// сервисного слоя — Outbox это и есть публичный API) и проверяем, что GET
// показывает канал/адресат и текст ошибки, а основная страница алертов эту
// запись больше не показывает.
func TestWebAlertDeliveriesPageShowsFailedDeliveries(t *testing.T) {
	s := newStack(t)
	authSvc := auth.NewService(s.pool)
	orgSvc := org.NewService(s.pool, 1_000_000)
	alertSvc := alert.NewService(s.pool)
	ob := notify.NewOutbox(s.pool)

	ownerID, ownerCookie := orgSettingsRegister(t, authSvc, "alertsfailed-owner@example.com")
	o, err := orgSvc.CreateOrg(context.Background(), "alertsfailed-co", "AlertsFailed Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	proj, err := orgSvc.CreateProject(context.Background(), o.ID, "alertsfailed-proj", "AlertsFailed Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	chanID, err := alertSvc.CreateChannel(context.Background(), alert.Channel{
		ProjectID: proj.ID, Kind: alert.ChannelWebhook, Enabled: true,
		Target: "https://hooks.example.com/failed-test", Secret: "sig-secret",
	})
	if err != nil {
		t.Fatalf("create channel: %v", err)
	}
	if err := ob.Enqueue(context.Background(), chanID, map[string]any{"title": "boom"}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	jobs, err := ob.Claim(context.Background(), 10)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("claim: %+v err=%v", jobs, err)
	}
	if err := ob.MarkFailed(context.Background(), jobs[0].ID, errors.New("connection refused by hooks.example.com")); err != nil {
		t.Fatalf("mark failed: %v", err)
	}

	alertsPath := "/projects/" + strconv.FormatInt(proj.ID, 10) + "/alerts"
	deliveriesPath := alertsPath + "/deliveries"

	// The main alerts page no longer shows the failed-deliveries table (the
	// channel's target legitimately still appears there, in the channels
	// table — so assert on the failed-delivery error text instead, which
	// only ever appears in the failed-deliveries table).
	resp := getWithCookie(t, s.srv, alertsPath, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200: %s", alertsPath, resp.StatusCode, body)
	}
	if strings.Contains(string(body), "connection refused by hooks.example.com") {
		t.Fatalf("GET %s still shows failed delivery error (should have moved to %s): %s", alertsPath, deliveriesPath, body)
	}

	resp = getWithCookie(t, s.srv, deliveriesPath, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200: %s", deliveriesPath, resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "https://hooks.example.com/failed-test") {
		t.Fatalf("GET %s missing failed delivery target: %s", deliveriesPath, body)
	}
	if !strings.Contains(string(body), "connection refused by hooks.example.com") {
		t.Fatalf("GET %s missing failed delivery error: %s", deliveriesPath, body)
	}
	// The signing secret must never reach the page.
	if strings.Contains(string(body), "sig-secret") {
		t.Fatalf("GET %s leaks channel secret: %s", deliveriesPath, body)
	}

	// Member (non-owner/admin) is denied, same guard as the main alerts page.
	memberID, memberCookie := orgSettingsRegister(t, authSvc, "alertsfailed-member@example.com")
	if err := orgSvc.AddMember(context.Background(), o.ID, memberID, org.RoleMember); err != nil {
		t.Fatalf("add member: %v", err)
	}
	resp = getWithCookie(t, s.srv, deliveriesPath, memberCookie)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET %s (member) status = %d, want 404", deliveriesPath, resp.StatusCode)
	}
}

// TestWebAlertsEmailEnabled — PROD-P2: форма канала алертов отражает
// доступность SMTP. При EmailEnabled=false опция Email дизейблена с
// пояснением «SMTP не настроен» (активной опции email в форме нет); при
// EmailEnabled=true — обычная активная опция email.
func TestWebAlertsEmailEnabled(t *testing.T) {
	s := newStack(t)
	authSvc := auth.NewService(s.pool)
	orgSvc := org.NewService(s.pool, 1_000_000)

	ownerID, ownerCookie := orgSettingsRegister(t, authSvc, "alertsemail-owner@example.com")
	o, err := orgSvc.CreateOrg(context.Background(), "alertsemail-co", "AlertsEmail Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	proj, err := orgSvc.CreateProject(context.Background(), o.ID, "alertsemail-proj", "AlertsEmail Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	alertsPath := "/projects/" + strconv.FormatInt(proj.ID, 10) + "/alerts"

	// SMTP не настроен: опция Email дизейблена, есть пояснение, активной
	// опции email нет.
	s.h.EmailEnabled = false
	resp := getWithCookie(t, s.srv, alertsPath, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200: %s", alertsPath, resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "SMTP не настроен") {
		t.Fatalf("GET %s (email disabled) missing SMTP hint: %s", alertsPath, body)
	}
	if strings.Contains(string(body), `<option value="email">Email</option>`) {
		t.Fatalf("GET %s (email disabled) still has active email option: %s", alertsPath, body)
	}

	// SMTP настроен: обычная активная опция email.
	s.h.EmailEnabled = true
	resp = getWithCookie(t, s.srv, alertsPath, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200: %s", alertsPath, resp.StatusCode, body)
	}
	if !strings.Contains(string(body), `<option value="email">Email</option>`) {
		t.Fatalf("GET %s (email enabled) missing active email option: %s", alertsPath, body)
	}
	if strings.Contains(string(body), "SMTP не настроен") {
		t.Fatalf("GET %s (email enabled) unexpectedly shows SMTP hint: %s", alertsPath, body)
	}
}

// TestWebProjectSettingsHasAlertsLink — «Alerts» доступна ссылкой со страницы
// настроек проекта.
func TestWebProjectSettingsHasAlertsLink(t *testing.T) {
	s := newStack(t)
	authSvc := auth.NewService(s.pool)
	orgSvc := org.NewService(s.pool, 1_000_000)

	ownerID, ownerCookie := orgSettingsRegister(t, authSvc, "alertslink-owner@example.com")
	o, err := orgSvc.CreateOrg(context.Background(), "alertslink-co", "AlertsLink Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	proj, err := orgSvc.CreateProject(context.Background(), o.ID, "alertslink-proj", "AlertsLink Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	settingsPath := "/projects/" + strconv.FormatInt(proj.ID, 10) + "/settings"
	alertsPath := "/projects/" + strconv.FormatInt(proj.ID, 10) + "/alerts"

	resp := getWithCookie(t, s.srv, settingsPath, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200: %s", settingsPath, resp.StatusCode, body)
	}
	if !strings.Contains(string(body), alertsPath) {
		t.Fatalf("GET %s missing alerts link %q: %s", settingsPath, alertsPath, body)
	}
}

// TestOnboardingCreatesDefaultAlertRules — задача 5: онбординг заводит
// default-правила алертинга (EnsureDefaultRules) для нового проекта.
func TestOnboardingCreatesDefaultAlertRules(t *testing.T) {
	s := newStack(t)
	alertSvc := alert.NewService(s.pool)

	regForm := url.Values{
		"email":     {"onboard-alerts@example.com"},
		"password":  {"correct-horse-battery"},
		"password2": {"correct-horse-battery"},
	}
	resp := postForm(t, s.srv, "/register", regForm, s.srv.URL, nil)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	cookie := sessionCookie(resp)
	if cookie == nil {
		t.Fatalf("register did not set session cookie")
	}

	validForm := url.Values{
		"org_slug":     {"onboard-alerts-co"},
		"org_name":     {"Onboard Alerts Co"},
		"project_slug": {"onboard-alerts-proj"},
		"project_name": {"Onboard Alerts Proj"},
		"platform":     {"go"},
	}
	resp = postForm(t, s.srv, "/onboarding", validForm, s.srv.URL, cookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST /onboarding status = %d, want 303", resp.StatusCode)
	}
	setupPath := resp.Header.Get("Location")
	// setupPath = /projects/{id}/setup
	idStr := strings.TrimSuffix(strings.TrimPrefix(setupPath, "/projects/"), "/setup")
	projectID, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		t.Fatalf("parse project id from %q: %v", setupPath, err)
	}

	rules, err := alertSvc.Rules(context.Background(), projectID)
	if err != nil || len(rules) != 2 {
		t.Fatalf("Rules after onboarding = %+v, err=%v, want 2 (new_issue, regression)", rules, err)
	}
	for _, r := range rules {
		if !r.Enabled || r.ThrottleMinutes != 30 {
			t.Errorf("default rule %+v, want enabled throttle=30", r)
		}
	}
}
