package alert_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/alert"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// newProject: прямые вставки — alert-пакет не зависит от org.
func newProject(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	ctx := context.Background()
	var orgID, projectID int64
	if err := pool.QueryRow(ctx,
		"INSERT INTO organizations (slug, name, event_quota) VALUES ('alertorg','Alert Org',1000000) RETURNING id").Scan(&orgID); err != nil {
		t.Fatalf("org: %v", err)
	}
	if err := pool.QueryRow(ctx,
		"INSERT INTO projects (org_id, slug, name) VALUES ($1,'api','API') RETURNING id", orgID).Scan(&projectID); err != nil {
		t.Fatalf("project: %v", err)
	}
	return projectID
}

func TestRuleCRUDAndUpsert(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := alert.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pid := newProject(t, pool)

	id, err := svc.UpsertRule(ctx, alert.Rule{
		ProjectID: pid, Kind: alert.KindNewIssue, Enabled: true, ThrottleMinutes: 15,
	})
	if err != nil || id == 0 {
		t.Fatalf("UpsertRule: id=%d err=%v", id, err)
	}

	rules, err := svc.Rules(ctx, pid)
	if err != nil || len(rules) != 1 {
		t.Fatalf("Rules: %+v err=%v", rules, err)
	}
	if rules[0].ID != id || rules[0].Kind != alert.KindNewIssue || rules[0].ThrottleMinutes != 15 {
		t.Errorf("Rules[0] = %+v, want id=%d kind=new_issue throttle=15", rules[0], id)
	}

	// UNIQUE(project_id, kind): второй UpsertRule с тем же kind обновляет,
	// а не создаёт новую строку.
	id2, err := svc.UpsertRule(ctx, alert.Rule{
		ProjectID: pid, Kind: alert.KindNewIssue, Enabled: false, ThrottleMinutes: 60,
	})
	if err != nil || id2 != id {
		t.Fatalf("UpsertRule (update): id2=%d err=%v, want id=%d", id2, err, id)
	}
	rules, err = svc.Rules(ctx, pid)
	if err != nil || len(rules) != 1 {
		t.Fatalf("Rules after update: %+v err=%v", rules, err)
	}
	if rules[0].Enabled || rules[0].ThrottleMinutes != 60 {
		t.Errorf("Rules[0] after update = %+v, want enabled=false throttle=60", rules[0])
	}

	if err := svc.DeleteRule(ctx, id); err != nil {
		t.Fatalf("DeleteRule: %v", err)
	}
	if err := svc.DeleteRule(ctx, id); !errors.Is(err, alert.ErrNotFound) {
		t.Fatalf("DeleteRule (already gone): got %v, want ErrNotFound", err)
	}
}

func TestRuleValidation(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := alert.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pid := newProject(t, pool)

	if _, err := svc.UpsertRule(ctx, alert.Rule{ProjectID: pid, Kind: "bogus"}); !errors.Is(err, alert.ErrInvalidRule) {
		t.Fatalf("bad kind: got %v, want ErrInvalidRule", err)
	}
	// spike требует Threshold>0 и WindowMinutes>0.
	cases := []alert.Rule{
		{ProjectID: pid, Kind: alert.KindSpike, Threshold: 0, WindowMinutes: 10},
		{ProjectID: pid, Kind: alert.KindSpike, Threshold: 5, WindowMinutes: 0},
	}
	for _, r := range cases {
		if _, err := svc.UpsertRule(ctx, r); !errors.Is(err, alert.ErrInvalidRule) {
			t.Errorf("spike %+v: got %v, want ErrInvalidRule", r, err)
		}
	}
	if _, err := svc.UpsertRule(ctx, alert.Rule{
		ProjectID: pid, Kind: alert.KindSpike, Threshold: 5, WindowMinutes: 10,
	}); err != nil {
		t.Errorf("valid spike: unexpected err %v", err)
	}
}

func TestRuleNegativeThrottle(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := alert.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pid := newProject(t, pool)

	// ThrottleMinutes -1 should reject.
	if _, err := svc.UpsertRule(ctx, alert.Rule{
		ProjectID: pid, Kind: alert.KindNewIssue, ThrottleMinutes: -1,
	}); !errors.Is(err, alert.ErrInvalidRule) {
		t.Fatalf("ThrottleMinutes=-1: got %v, want ErrInvalidRule", err)
	}

	// Valid rule with ThrottleMinutes 0 should work (0 means no throttle).
	if _, err := svc.UpsertRule(ctx, alert.Rule{
		ProjectID: pid, Kind: alert.KindNewIssue, ThrottleMinutes: 0,
	}); err != nil {
		t.Fatalf("ThrottleMinutes=0: unexpected err %v", err)
	}
}

func TestChannelCRUDAndValidation(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := alert.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pid := newProject(t, pool)

	id, err := svc.CreateChannel(ctx, alert.Channel{
		ProjectID: pid, Kind: alert.ChannelEmail, Enabled: true, Target: "ops@example.com",
	})
	if err != nil || id == 0 {
		t.Fatalf("CreateChannel (email): id=%d err=%v", id, err)
	}
	if _, err := svc.CreateChannel(ctx, alert.Channel{
		ProjectID: pid, Kind: alert.ChannelEmail, Target: "not-an-email",
	}); !errors.Is(err, alert.ErrInvalidChannel) {
		t.Errorf("bad email: got %v, want ErrInvalidChannel", err)
	}

	if _, err := svc.CreateChannel(ctx, alert.Channel{
		ProjectID: pid, Kind: alert.ChannelWebhook, Target: "https://example.com/hook",
	}); err != nil {
		t.Errorf("valid webhook: unexpected err %v", err)
	}
	for _, target := range []string{"not-a-url", "ftp://example.com/x", ""} {
		if _, err := svc.CreateChannel(ctx, alert.Channel{
			ProjectID: pid, Kind: alert.ChannelWebhook, Target: target,
		}); !errors.Is(err, alert.ErrInvalidChannel) {
			t.Errorf("bad webhook %q: got %v, want ErrInvalidChannel", target, err)
		}
	}

	if _, err := svc.CreateChannel(ctx, alert.Channel{
		ProjectID: pid, Kind: alert.ChannelTelegram, Target: "12345", Secret: "bot-token",
	}); err != nil {
		t.Errorf("valid telegram: unexpected err %v", err)
	}
	if _, err := svc.CreateChannel(ctx, alert.Channel{
		ProjectID: pid, Kind: alert.ChannelTelegram, Target: "", Secret: "bot-token",
	}); !errors.Is(err, alert.ErrInvalidChannel) {
		t.Errorf("telegram missing chat_id: got %v, want ErrInvalidChannel", err)
	}
	if _, err := svc.CreateChannel(ctx, alert.Channel{
		ProjectID: pid, Kind: alert.ChannelTelegram, Target: "12345", Secret: "",
	}); !errors.Is(err, alert.ErrInvalidChannel) {
		t.Errorf("telegram missing secret: got %v, want ErrInvalidChannel", err)
	}

	channels, err := svc.Channels(ctx, pid)
	if err != nil || len(channels) != 3 {
		t.Fatalf("Channels: %+v err=%v, want 3", channels, err)
	}

	if err := svc.DeleteChannel(ctx, id); err != nil {
		t.Fatalf("DeleteChannel: %v", err)
	}
	if err := svc.DeleteChannel(ctx, id); !errors.Is(err, alert.ErrNotFound) {
		t.Fatalf("DeleteChannel (already gone): got %v, want ErrNotFound", err)
	}
	channels, err = svc.Channels(ctx, pid)
	if err != nil || len(channels) != 2 {
		t.Fatalf("Channels after delete: %+v err=%v, want 2", channels, err)
	}
}

func TestEnsureDefaultRules(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := alert.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pid := newProject(t, pool)

	if err := svc.EnsureDefaultRules(ctx, pid); err != nil {
		t.Fatalf("EnsureDefaultRules: %v", err)
	}
	rules, err := svc.Rules(ctx, pid)
	if err != nil || len(rules) != 2 {
		t.Fatalf("Rules: %+v err=%v, want 2", rules, err)
	}
	byKind := map[string]alert.Rule{}
	for _, r := range rules {
		byKind[r.Kind] = r
	}
	for _, kind := range []string{alert.KindNewIssue, alert.KindRegression} {
		r, ok := byKind[kind]
		if !ok || !r.Enabled || r.ThrottleMinutes != 30 {
			t.Errorf("default rule %s = %+v (ok=%v), want enabled throttle=30", kind, r, ok)
		}
	}

	// Идемпотентна и не перезаписывает уже настроенное вручную правило.
	if _, err := svc.UpsertRule(ctx, alert.Rule{
		ProjectID: pid, Kind: alert.KindNewIssue, Enabled: false, ThrottleMinutes: 99,
	}); err != nil {
		t.Fatalf("UpsertRule (manual override): %v", err)
	}
	if err := svc.EnsureDefaultRules(ctx, pid); err != nil {
		t.Fatalf("EnsureDefaultRules (second call): %v", err)
	}
	rules, err = svc.Rules(ctx, pid)
	if err != nil || len(rules) != 2 {
		t.Fatalf("Rules after second EnsureDefaultRules: %+v err=%v, want 2", rules, err)
	}
	for _, r := range rules {
		if r.Kind == alert.KindNewIssue && (r.Enabled || r.ThrottleMinutes != 99) {
			t.Errorf("manual override clobbered: %+v", r)
		}
	}
}
