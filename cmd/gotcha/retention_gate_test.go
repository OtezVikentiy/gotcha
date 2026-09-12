package main

import (
	"bytes"
	"context"
	"log/slog"
	"net/url"
	"strings"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/db"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// TestRetentionRolloutGatedByAutoMigrate: раскатка ретенции (запись в PostgreSQL и
// ALTER TABLE в ClickHouse) не идёт, когда оператор выключил автомиграцию.
func TestRetentionRolloutGatedByAutoMigrate(t *testing.T) {
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

	base := Config{
		PostgresDSN:          pgDSN,
		ClickHouseDSN:        chDSN,
		AutoMigrate:          true,
		RetentionDays:        30,
		SpanRetentionDays:    30,
		MetricRetentionDays:  30,
		ProfileRetentionDays: 30,
		LogRetentionDays:     30,
	}
	if _, err := applyMigrations(ctx, base, pg, ch); err != nil {
		t.Fatalf("applyMigrations (сев baseline с AutoMigrate=true): %v", err)
	}

	var before int
	if err := pg.QueryRow(ctx, "SELECT days FROM retention_state WHERE key = 'events'").Scan(&before); err != nil {
		t.Fatalf("read baseline retention_state: %v", err)
	}
	if before != 30 {
		t.Fatalf("baseline retention_state.events = %d, want 30", before)
	}

	disabled := base
	disabled.AutoMigrate = false
	// Другое значение — если гейт сломан, оно попадёт и в PG, и в CH; если карту,
	// лог или метрику собрать из cfg вместо базы, расхождение 90≠30 останется незамеченным.
	disabled.RetentionDays = 90
	disabled.SpanRetentionDays = 90
	disabled.MetricRetentionDays = 90
	disabled.ProfileRetentionDays = 90
	disabled.LogRetentionDays = 90

	var buf bytes.Buffer
	prevLog := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	retention, err := applyMigrations(ctx, disabled, pg, ch)
	slog.SetDefault(prevLog)
	if err != nil {
		t.Fatalf("applyMigrations (AutoMigrate=false) должен пройти без ошибки: %v", err)
	}

	if retention["events"] != 30 {
		t.Errorf("applyMigrations вернула retention[events]=%d при расхождении конфига (90) и базы (30) — "+
			"источник не база, а cfg этой реплики", retention["events"])
	}
	if !strings.Contains(buf.String(), "events_days=30") {
		t.Errorf("лог «retention effective» не показывает значение из базы (30), а не из cfg (90):\n%s", buf.String())
	}

	var after int
	if err := pg.QueryRow(ctx, "SELECT days FROM retention_state WHERE key = 'events'").Scan(&after); err != nil {
		t.Fatalf("read retention_state after disabled run: %v", err)
	}
	if after != 30 {
		t.Errorf("retention_state.events = %d после старта с выключенной автомиграцией, want 30 (запись прошла мимо гейта)", after)
	}

	var ddl string
	if err := ch.QueryRow(ctx, "SHOW CREATE TABLE `events`").Scan(&ddl); err != nil {
		t.Fatalf("show create table events: %v", err)
	}
	if !strings.Contains(ddl, "toIntervalDay(30)") {
		t.Errorf("TTL events в ClickHouse изменился при выключенной автомиграции:\n%s", ddl)
	}
}

// readOnlyDSN добавляет startup-параметр СЕАНСА (не кластера): read-only видят только
// соединения, открытые по этой строке — и golang-migrate, и db.NewPostgres.
func readOnlyDSN(t *testing.T, dsn string) string {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	q := u.Query()
	q.Set("options", "-c default_transaction_read_only=on")
	u.RawQuery = q.Encode()
	return u.String()
}

// TestReadOnlyPostgresStartupBehavior воспроизводит failover managed-PostgreSQL: с
// выключенной автомиграцией старт проходит, со включённой — отказывает с диагнозом.
func TestReadOnlyPostgresStartupBehavior(t *testing.T) {
	pgDSN := testenv.PostgresDSN(t)
	chDSN := testenv.ClickHouseDSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Схема должна быть актуальной ДО переключения на read-only — иначе не отличить
	// «база read-only» от «база не смигрирована».
	if err := db.MigratePG(pgDSN); err != nil {
		t.Fatalf("migrate pg: %v", err)
	}
	if err := db.MigrateCH(chDSN); err != nil {
		t.Fatalf("migrate ch: %v", err)
	}
	ch, err := db.NewClickHouse(ctx, chDSN)
	if err != nil {
		t.Fatalf("connect ch: %v", err)
	}
	defer ch.Close()

	roDSN := readOnlyDSN(t, pgDSN)
	roPG, err := db.NewPostgres(ctx, roDSN)
	if err != nil {
		t.Fatalf("connect read-only pg: %v", err)
	}
	defer roPG.Close()

	// Сеанс действительно read-only — не полагаемся на код гейта, чтобы не спутать
	// «сеанс не read-only» с «гейт не сработал».
	var ro string
	if err := roPG.QueryRow(ctx, "SHOW transaction_read_only").Scan(&ro); err != nil {
		t.Fatalf("check transaction_read_only: %v", err)
	}
	if ro != "on" {
		t.Fatalf("тестовый сеанс не read-only (SHOW transaction_read_only = %q) — настройка не сработала", ro)
	}

	t.Run("AutoMigrate=false starts cleanly", func(t *testing.T) {
		cfg := Config{PostgresDSN: roDSN, ClickHouseDSN: chDSN, AutoMigrate: false}
		if _, err := applyMigrations(ctx, cfg, roPG, ch); err != nil {
			t.Fatalf("applyMigrations должен пройти на read-only PG при выключенной автомиграции: %v", err)
		}
	})

	t.Run("AutoMigrate=true fails with a clear diagnosis, not silently", func(t *testing.T) {
		cfg := Config{PostgresDSN: roDSN, ClickHouseDSN: chDSN, AutoMigrate: true}
		_, err := applyMigrations(ctx, cfg, roPG, ch)
		if err == nil {
			t.Fatal("applyMigrations с AutoMigrate=true на read-only PG должен вернуть ошибку")
		}
		if !strings.Contains(err.Error(), "read-only") {
			t.Fatalf("ошибка не называет настоящую причину (postgres read-only): %v", err)
		}
	})
}
