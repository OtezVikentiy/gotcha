package templates

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
)

func TestHeartbeatGraceText(t *testing.T) {
	ctx := ruCtx()
	cases := []struct {
		name string
		d    time.Duration
		want string
	}{
		{"zero", 0, "—"},
		{"negative", -time.Minute, "—"},
		{"minutes only", 15 * time.Minute, "15 мин"},
		{"hour only", time.Hour, "1 ч"},
		{"hour and minutes", 90 * time.Minute, "1 ч 30 мин"},
		{"minutes and seconds", 90 * time.Second, "1 мин 30 с"},
		{"all three", time.Hour + 2*time.Minute + 3*time.Second, "1 ч 2 мин 3 с"},
		{"hour and seconds", time.Hour + 5*time.Second, "1 ч 5 с"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := heartbeatGraceText(ctx, c.d); got != c.want {
				t.Fatalf("heartbeatGraceText(%s) = %q, want %q", c.d, got, c.want)
			}
		})
	}
}

func TestSSLExpiryText(t *testing.T) {
	ctx := ruCtx()
	if got := sslExpiryText(ctx, nil); got != "—" {
		t.Fatalf("sslExpiryText(nil) = %q, want —", got)
	}

	// Истёк 10 часов назад: int(-0.41)==0 раньше давал «осталось 0 дней» вместо «истёк».
	expiredWord := i18n.T(ctx, "uptime.ssl.expired")
	expiredWord = expiredWord[:strings.Index(expiredWord, "(")]
	expiredRecently := time.Now().Add(-10 * time.Hour)
	got := sslExpiryText(ctx, &expiredRecently)
	if !strings.Contains(got, expiredWord) {
		t.Fatalf("sslExpiryText(-10h) = %q, должен содержать %q", got, expiredWord)
	}

	// Истекает через 10 часов: усечение к нулю давало «0 дней», алертинг в это же время
	// уже отправляет days_left=1 (потолок).
	soon := time.Now().Add(10 * time.Hour)
	got = sslExpiryText(ctx, &soon)
	want := i18n.Tf(ctx, "uptime.ssl.days_left", "days", "1", "date", "")
	want = want[:strings.Index(want, "(")]
	if !strings.Contains(got, want) {
		t.Fatalf("sslExpiryText(+10h) = %q, должен содержать %q (потолок должен дать 1 день, не 0)", got, want)
	}

	in30h := time.Now().Add(30 * time.Hour)
	got = sslExpiryText(ctx, &in30h)
	want = i18n.Tf(ctx, "uptime.ssl.days_left", "days", "2", "date", "")
	want = want[:strings.Index(want, "(")]
	if !strings.Contains(got, want) {
		t.Fatalf("sslExpiryText(+30h) = %q, должен содержать %q", got, want)
	}
}

func TestHeartbeatGrace(t *testing.T) {
	cfg, err := json.Marshal(uptime.HeartbeatConfig{GraceSeconds: 900})
	if err != nil {
		t.Fatal(err)
	}
	if got := heartbeatGrace(uptime.Monitor{Config: cfg}); got != 15*time.Minute {
		t.Fatalf("heartbeatGrace(grace_seconds=900) = %s, want 15m", got)
	}
	if got := heartbeatGrace(uptime.Monitor{}); got != 0 {
		t.Fatalf("heartbeatGrace(nil config) = %s, want 0", got)
	}
	if got := heartbeatGrace(uptime.Monitor{Config: json.RawMessage(`not json`)}); got != 0 {
		t.Fatalf("heartbeatGrace(garbage config) = %s, want 0", got)
	}
	if got := heartbeatGraceText(ruCtx(), heartbeatGrace(uptime.Monitor{Config: json.RawMessage(`{}`)})); got != "—" {
		t.Fatalf("grace tile for config without grace_seconds = %q, want dash", got)
	}
}

func TestHeartbeatExpectedByText(t *testing.T) {
	ctx := ruCtx()
	cfg, err := json.Marshal(uptime.HeartbeatConfig{GraceSeconds: 5400})
	if err != nil {
		t.Fatal(err)
	}
	if got := heartbeatExpectedByText(ctx, uptime.Monitor{Config: cfg}); got != "—" {
		t.Fatalf("heartbeatExpectedByText(no beat) = %q, want dash", got)
	}
	beat := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	if got := heartbeatExpectedByText(ctx, uptime.Monitor{Config: cfg, LastBeatAt: &beat}); got != "2026-07-01 13:30 UTC" {
		t.Fatalf("heartbeatExpectedByText(beat 12:00, grace 1h30m) = %q, want 2026-07-01 13:30 UTC", got)
	}
}
