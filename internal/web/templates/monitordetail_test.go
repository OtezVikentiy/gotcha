package templates

import (
	"encoding/json"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
)

// TestHeartbeatGraceText — допуск короткими единицами, точно, а не «порядок»:
// каждая ветка функции (часы/минуты/секунды и их сочетания) и прочерк на
// нулевом/отрицательном допуске. Страничные тесты (web/monitordetail_test.go)
// доходят только до «15 мин» и «1 ч 30 мин»: Create отсекает grace < 60, и
// нулевой допуск через страницу не воспроизвести.
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

// TestHeartbeatGrace — допуск читается из config через HeartbeatConfig;
// пустой или не разбираемый config даёт нулевой допуск (и, следовательно,
// прочерк в плитке), а не панику.
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

// TestHeartbeatExpectedByText — срок следующего маячка: last_beat_at + допуск
// в формате humanize.Time (UTC), прочерк, пока маячка не было.
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
