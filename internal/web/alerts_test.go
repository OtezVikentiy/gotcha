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

	resp = postForm(t, s.srv, rulesPath, validForm, "", ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST %s (no origin) status = %d, want 403", rulesPath, resp.StatusCode)
	}

	resp = postForm(t, s.srv, rulesPath, validForm, s.srv.URL, memberCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("POST %s (member) status = %d, want 404", rulesPath, resp.StatusCode)
	}

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

	resp := postForm(t, s.srv, channelsPath, url.Values{"kind": {"email"}, "target": {"ops@example.com"}, "enabled": {"on"}}, s.srv.URL, memberCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST %s (member) status = %d, want 403", channelsPath, resp.StatusCode)
	}

	resp = postForm(t, s.srv, channelsPath, url.Values{"kind": {"email"}, "target": {"not-an-email"}, "enabled": {"on"}}, s.srv.URL, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST %s (bad email) status = %d, want 422: %s", channelsPath, resp.StatusCode, body)
	}

	resp = postForm(t, s.srv, channelsPath, url.Values{"kind": {"email"}, "target": {"ops@example.com"}, "enabled": {"on"}}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST %s (email) status = %d, want 303", channelsPath, resp.StatusCode)
	}

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

	resp = getWithCookie(t, s.srv, alertsPath, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "ops@example.com") || !strings.Contains(string(body), "https://example.com/hook") {
		t.Fatalf("GET %s missing channel targets: %s", alertsPath, body)
	}

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

	resp = postForm(t, s.srv, channelsDeletePath, url.Values{"channel_id": {strconv.FormatInt(channels[0].ID, 10)}}, s.srv.URL, memberCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST %s (member) status = %d, want 403", channelsDeletePath, resp.StatusCode)
	}

	resp = postForm(t, s.srv, channelsDeletePath, url.Values{"channel_id": {strconv.FormatInt(channels[0].ID, 10)}}, s.srv.URL, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST %s (unconfirmed) status = %d, want 200: %s", channelsDeletePath, resp.StatusCode, body)
	}
	if !strings.Contains(string(body), `name="confirmed" value="yes"`) {
		t.Fatalf("POST %s (unconfirmed) missing confirm page hidden field: %s", channelsDeletePath, body)
	}
	if !strings.Contains(string(body), "«"+channels[0].Target+"»") {
		t.Fatalf("POST %s (unconfirmed) confirm page does not name the channel target %q: %s", channelsDeletePath, channels[0].Target, body)
	}
	if c, err := alertSvc.Channels(context.Background(), proj.ID); err != nil || len(c) != 2 {
		t.Fatalf("channel gone after unconfirmed delete: %+v err=%v", c, err)
	}

	resp = postForm(t, s.srv, channelsDeletePath, url.Values{"confirmed": {"yes"}, "channel_id": {strconv.FormatInt(channels[0].ID, 10)}}, s.srv.URL, ownerCookie)
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
	if err := ob.MarkFailed(context.Background(), jobs[0].ID, jobs[0].Attempts, errors.New("connection refused by hooks.example.com")); err != nil {
		t.Fatalf("mark failed: %v", err)
	}

	alertsPath := "/projects/" + strconv.FormatInt(proj.ID, 10) + "/alerts"
	deliveriesPath := alertsPath + "/deliveries"

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

	bcStart := strings.Index(string(body), `<p class="breadcrumb">`)
	if bcStart < 0 {
		t.Fatalf("GET %s missing breadcrumb: %s", deliveriesPath, body)
	}
	bcEnd := strings.Index(string(body)[bcStart:], "</p>")
	if bcEnd < 0 {
		t.Fatalf("GET %s breadcrumb <p> never closes: %s", deliveriesPath, body)
	}
	breadcrumbHTML := string(body)[bcStart : bcStart+bcEnd]
	if !strings.Contains(breadcrumbHTML, alertsPath) {
		t.Fatalf("GET %s breadcrumb does not link to %s: %s", deliveriesPath, alertsPath, breadcrumbHTML)
	}
	if !strings.Contains(breadcrumbHTML, "По ошибкам") {
		t.Errorf("GET %s breadcrumb to %s = %q, want label «По ошибкам» (nav.rules_errors — target page's own title)", deliveriesPath, alertsPath, breadcrumbHTML)
	}
	if strings.Contains(breadcrumbHTML, "Оповещения") {
		t.Errorf("GET %s breadcrumb to %s still carries the area name «Оповещения» (nav.alerts) instead of the page title: %s", deliveriesPath, alertsPath, breadcrumbHTML)
	}

	if !strings.Contains(string(body), "https://hooks.example.com/failed-test") {
		t.Fatalf("GET %s missing failed delivery target: %s", deliveriesPath, body)
	}
	if !strings.Contains(string(body), "connection refused by hooks.example.com") {
		t.Fatalf("GET %s missing failed delivery error: %s", deliveriesPath, body)
	}
	if strings.Contains(string(body), "sig-secret") {
		t.Fatalf("GET %s leaks channel secret: %s", deliveriesPath, body)
	}

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

type fakeTestSender struct {
	payloads []map[string]any
	err      error
}

func (f *fakeTestSender) Send(_ context.Context, _ notify.Target, payload map[string]any) error {
	f.payloads = append(f.payloads, payload)
	return f.err
}

func TestWebAlertsChannelTest(t *testing.T) {
	s := newStack(t)
	authSvc := auth.NewService(s.pool)
	orgSvc := org.NewService(s.pool, 1_000_000)
	alertSvc := alert.NewService(s.pool)

	ownerID, ownerCookie := orgSettingsRegister(t, authSvc, "chantest-owner@example.com")
	memberID, memberCookie := orgSettingsRegister(t, authSvc, "chantest-member@example.com")
	o, err := orgSvc.CreateOrg(context.Background(), "chantest-co", "ChanTest Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	if err := orgSvc.AddMember(context.Background(), o.ID, memberID, org.RoleMember); err != nil {
		t.Fatalf("add member: %v", err)
	}
	proj, err := orgSvc.CreateProject(context.Background(), o.ID, "chantest-proj", "ChanTest Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	chID, err := alertSvc.CreateChannel(context.Background(), alert.Channel{
		ProjectID: proj.ID, Kind: alert.ChannelTelegram, Enabled: true, Target: "123", Secret: "tok",
	})
	if err != nil {
		t.Fatalf("create channel: %v", err)
	}

	sender := &fakeTestSender{}
	s.h.NotifyDirect = &notify.Direct{Senders: map[string]notify.Sender{alert.ChannelTelegram: sender}}

	testPath := "/projects/" + strconv.FormatInt(proj.ID, 10) + "/alerts/channels/test"
	form := url.Values{"channel_id": {strconv.FormatInt(chID, 10)}}

	resp := postForm(t, s.srv, testPath, form, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST test status = %d, want 303", resp.StatusCode)
	}
	if len(sender.payloads) != 1 {
		t.Fatalf("payloads = %d, want 1", len(sender.payloads))
	}
	subj, _ := sender.payloads[0]["subject"].(string)
	if !strings.Contains(subj, "[Gotcha]") {
		t.Errorf("subject = %q", subj)
	}

	sender.err = errors.New("telegram: 403 forbidden")
	resp = postForm(t, s.srv, testPath, form, s.srv.URL, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST test (fail) status = %d, want 422", resp.StatusCode)
	}
	if !strings.Contains(string(body), "telegram: 403 forbidden") {
		t.Errorf("страница без причины отказа: %s", body)
	}
	if !strings.Contains(string(body), "от провайдера доставки") {
		t.Errorf("страница не поясняет, что причина — техническая строка от провайдера: %s", body)
	}

	before := len(sender.payloads)
	resp = postForm(t, s.srv, testPath, form, s.srv.URL, memberCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST test (member) status = %d, want 403", resp.StatusCode)
	}

	proj2, err := orgSvc.CreateProject(context.Background(), o.ID, "chantest-proj2", "ChanTest Proj2", "go")
	if err != nil {
		t.Fatalf("create project2: %v", err)
	}
	resp = postForm(t, s.srv, "/projects/"+strconv.FormatInt(proj2.ID, 10)+"/alerts/channels/test",
		form, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("POST test (чужой канал) status = %d, want 404", resp.StatusCode)
	}
	if len(sender.payloads) != before {
		t.Errorf("лишние отправки: %d, want %d", len(sender.payloads), before)
	}
}

func TestWebAlertsOperator(t *testing.T) {
	s := newStack(t)
	authSvc := auth.NewService(s.pool)
	orgSvc := org.NewService(s.pool, 1_000_000)
	alertSvc := alert.NewService(s.pool)
	ob := notify.NewOutbox(s.pool)

	ownerID, ownerCookie := orgSettingsRegister(t, authSvc, "alertsop-owner@example.com")
	opID, opCookie := orgSettingsRegister(t, authSvc, "alertsop-operator@example.com")

	o, err := orgSvc.CreateOrg(context.Background(), "alertsop-co", "AlertsOp Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	if err := orgSvc.AddMember(context.Background(), o.ID, opID, org.RoleMember); err != nil {
		t.Fatalf("add member: %v", err)
	}
	proj, err := orgSvc.CreateProject(context.Background(), o.ID, "alertsop-proj", "AlertsOp Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	addTeamAccess(t, orgSvc, o.ID, proj.ID, opID, "alertsop-team")

	emailChID, err := alertSvc.CreateChannel(context.Background(), alert.Channel{
		ProjectID: proj.ID, Kind: alert.ChannelEmail, Enabled: true, Target: "ops@example.com",
	})
	if err != nil {
		t.Fatalf("create email channel: %v", err)
	}
	if _, err := alertSvc.CreateChannel(context.Background(), alert.Channel{
		ProjectID: proj.ID, Kind: alert.ChannelWebhook, Enabled: true,
		Target: "https://hooks.example.com/T000/B000/secret", Secret: "sig-secret",
	}); err != nil {
		t.Fatalf("create webhook channel: %v", err)
	}
	s.h.NotifyDirect = &notify.Direct{Senders: map[string]notify.Sender{alert.ChannelEmail: &fakeTestSender{}}}

	alertsPath := "/projects/" + strconv.FormatInt(proj.ID, 10) + "/alerts"
	rulesPath := alertsPath + "/rules"
	channelsPath := alertsPath + "/channels"
	channelsUpdatePath := channelsPath + "/update"
	channelsDeletePath := channelsPath + "/delete"
	channelsTestPath := channelsPath + "/test"
	deliveriesPath := alertsPath + "/deliveries"

	resp := getWithCookie(t, s.srv, alertsPath, opCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s (operator) status = %d, want 200: %s", alertsPath, resp.StatusCode, body)
	}
	bodyStr := string(body)
	for _, want := range []string{"o***@example.com", "https://hooks.example.com/…"} {
		if !strings.Contains(bodyStr, want) {
			t.Errorf("GET %s (operator) missing masked target %q: %s", alertsPath, want, bodyStr)
		}
	}
	for _, bad := range []string{"ops@example.com", "https://hooks.example.com/T000/B000/secret", "sig-secret"} {
		if strings.Contains(bodyStr, bad) {
			t.Errorf("GET %s (operator) leaks raw %q: %s", alertsPath, bad, bodyStr)
		}
	}
	for _, bad := range []string{"channel-edit-form", "channel-create-form", "channel-delete-form", "channel-test-form", `href="#new-channel"`, "edit-channel-"} {
		if strings.Contains(bodyStr, bad) {
			t.Errorf("GET %s (operator) still shows channel CRUD %q: %s", alertsPath, bad, bodyStr)
		}
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
	resp = postForm(t, s.srv, rulesPath, validForm, s.srv.URL, opCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST %s (operator) status = %d, want 303", rulesPath, resp.StatusCode)
	}

	resp = postForm(t, s.srv, channelsPath, url.Values{"kind": {"email"}, "target": {"new@example.com"}, "enabled": {"on"}}, s.srv.URL, opCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST %s (operator create) status = %d, want 403", channelsPath, resp.StatusCode)
	}
	resp = postForm(t, s.srv, channelsUpdatePath, url.Values{"channel_id": {strconv.FormatInt(emailChID, 10)}, "target": {"new@example.com"}, "enabled": {"on"}}, s.srv.URL, opCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST %s (operator update) status = %d, want 403", channelsUpdatePath, resp.StatusCode)
	}
	resp = postForm(t, s.srv, channelsDeletePath, url.Values{"confirmed": {"yes"}, "channel_id": {strconv.FormatInt(emailChID, 10)}}, s.srv.URL, opCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST %s (operator delete) status = %d, want 403", channelsDeletePath, resp.StatusCode)
	}
	resp = postForm(t, s.srv, channelsTestPath, url.Values{"channel_id": {strconv.FormatInt(emailChID, 10)}}, s.srv.URL, opCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST %s (operator test) status = %d, want 403", channelsTestPath, resp.StatusCode)
	}
	channels, err := alertSvc.Channels(context.Background(), proj.ID)
	if err != nil || len(channels) != 2 {
		t.Fatalf("channels after rejected operator mutations = %+v err=%v, want 2 unchanged", channels, err)
	}

	if err := ob.Enqueue(context.Background(), emailChID, map[string]any{"title": "boom"}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	jobs, err := ob.Claim(context.Background(), 10)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("claim: %+v err=%v", jobs, err)
	}
	if err := ob.MarkFailed(context.Background(), jobs[0].ID, jobs[0].Attempts, errors.New("notify: smtp rcpt: 550 5.1.1 <ops@example.com>: Recipient address rejected")); err != nil {
		t.Fatalf("mark failed: %v", err)
	}
	resp = getWithCookie(t, s.srv, deliveriesPath, opCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s (operator) status = %d, want 200: %s", deliveriesPath, resp.StatusCode, body)
	}
	bodyStr = string(body)
	if !strings.Contains(bodyStr, "o***@example.com") {
		t.Errorf("GET %s (operator) missing masked target: %s", deliveriesPath, bodyStr)
	}
	if strings.Contains(bodyStr, "ops@example.com") {
		t.Errorf("GET %s (operator) leaks raw target/last_error: %s", deliveriesPath, bodyStr)
	}
	// Тело ответа цели (SSRF-чтение внутренней сети при разрешённом приватном вебхуке) —
	// не только токен/адрес — обязано быть скрыто целиком, не отредактировано частично.
	if strings.Contains(bodyStr, "Recipient address rejected") || strings.Contains(bodyStr, "smtp rcpt") {
		t.Errorf("GET %s (operator) leaks last_error body: %s", deliveriesPath, bodyStr)
	}
	if !strings.Contains(bodyStr, "Скрыто — видно owner/admin") {
		t.Errorf("GET %s (operator) missing hidden-error hint: %s", deliveriesPath, bodyStr)
	}

	resp = getWithCookie(t, s.srv, deliveriesPath, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s (owner) status = %d, want 200: %s", deliveriesPath, resp.StatusCode, body)
	}
	bodyStr = string(body)
	if !strings.Contains(bodyStr, "Recipient address rejected") {
		t.Errorf("GET %s (owner) must still see the full delivery error: %s", deliveriesPath, bodyStr)
	}
}
