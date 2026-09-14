package main

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/db"
	"gitflic.ru/otezvikentiy/gotcha/internal/selfmetrics"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

func TestRetentionDisplay(t *testing.T) {
	if got := retentionDisplay(0); got != "forever" {
		t.Errorf("retentionDisplay(0) = %v, want %q — 0 обязан читаться как «вечно», а не как отключённый лимит", got, "forever")
	}
	if got := retentionDisplay(30); got != 30 {
		t.Errorf("retentionDisplay(30) = %v, want 30", got)
	}
}

func TestRegisterRetentionMetrics(t *testing.T) {
	var reg selfmetrics.Registry
	registerRetentionMetrics(&reg, map[string]int{
		"events": 0, "spans": 7, "metrics": 30, "profiles": 3, "logs": 14,
	})
	out := reg.Gather()
	for _, want := range []string{
		`gotcha_retention_days{dataset="events"} 0`,
		`gotcha_retention_days{dataset="spans"} 7`,
		`gotcha_retention_days{dataset="metrics"} 30`,
		`gotcha_retention_days{dataset="profiles"} 3`,
		`gotcha_retention_days{dataset="logs"} 14`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("нет метрики действующей ретенции %q:\n%s", want, out)
		}
	}
}

// TestRetentionEffectiveIsObservable проверяет, что действующая ретенция видна
// после старта — «0» в логе читается как «forever», а не как «0».
func TestRetentionEffectiveIsObservable(t *testing.T) {
	pgDSN := testenv.PostgresDSN(t)
	chDSN := testenv.ClickHouseDSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := db.MigratePG(pgDSN); err != nil {
		t.Fatalf("migrate pg: %v", err)
	}
	if err := db.MigrateCH(chDSN); err != nil {
		t.Fatalf("migrate ch: %v", err)
	}
	pg, err := db.NewPostgres(ctx, pgDSN)
	if err != nil {
		t.Fatalf("connect pg: %v", err)
	}
	defer pg.Close()
	ch, err := db.NewClickHouse(ctx, chDSN)
	if err != nil {
		t.Fatalf("connect ch: %v", err)
	}
	defer ch.Close()

	cfg := Config{
		PostgresDSN:          pgDSN,
		ClickHouseDSN:        chDSN,
		AutoMigrate:          true,
		RetentionDays:        0,
		SpanRetentionDays:    7,
		MetricRetentionDays:  30,
		ProfileRetentionDays: 3,
		LogRetentionDays:     14,
	}

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	retention, err := applyMigrations(ctx, cfg, pg, ch)
	if err != nil {
		t.Fatalf("applyMigrations: %v", err)
	}
	if retention["events"] != 0 || retention["spans"] != 7 {
		t.Fatalf("action retention = %+v, want events=0 spans=7", retention)
	}

	out := buf.String()
	if !strings.Contains(out, "events_days=forever") {
		t.Errorf("действующая ретенция events=0 не отображена как «forever» в логе старта:\n%s", out)
	}
	if !strings.Contains(out, "spans_days=7") {
		t.Errorf("действующая ретенция spans=7 не отображена в логе старта:\n%s", out)
	}
}
