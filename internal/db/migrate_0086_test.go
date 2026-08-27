package db_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/db"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// TestMigrate0086AddsNotifyOpenChannelsColumn — миграция 0086 (W3-E, правка
// по вердикту ревью): аптайм получает retry для лога шага 0
// (escalation.LogStepChannels, тот же механизм, что у остальных пяти
// источников), и для него нужно ПОМНИТЬ, каким каналам "down" уже реально
// ушёл — notify_open_channels. Пре-существующий открытый инцидент (то самое
// доэскалационное состояние, которое видит любая работающая инсталляция при
// апгрейде) обязан пережить миграцию с NULL — снимка для него никогда не
// было, ретраить нечего.
func TestMigrate0086AddsNotifyOpenChannelsColumn(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	ctx := context.Background()
	dsn := testenv.PostgresDSN(t)
	if err := db.MigratePGTo(dsn, 85); err != nil {
		t.Fatalf("migrate to 85: %v", err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	var orgID, projectID, monitorID, incidentID int64
	mustScan(t, pool, &orgID, "INSERT INTO organizations (slug, name) VALUES ('m86', 'M86') RETURNING id")
	mustScan(t, pool, &projectID, "INSERT INTO projects (org_id, slug, name) VALUES ($1, 'm86', 'M86') RETURNING id", orgID)
	mustScan(t, pool, &monitorID, `
		INSERT INTO monitors (project_id, name, kind, interval_seconds)
		VALUES ($1, 'm86-monitor', 'heartbeat', 60) RETURNING id`, projectID)
	// Открытый инцидент, заведённый ДО миграции — ровно то, что увидит
	// любая работающая инсталляция при апгрейде.
	mustScan(t, pool, &incidentID, `
		INSERT INTO incidents (monitor_id, cause, notified_open)
		VALUES ($1, 'connection refused', true) RETURNING id`, monitorID)

	if err := db.MigratePGTo(dsn, 86); err != nil {
		t.Fatalf("migrate to 86: %v", err)
	}

	exists, err := columnExistsIn(ctx, pool, "public", "incidents", "notify_open_channels")
	if err != nil {
		t.Fatalf("columnExistsIn: %v", err)
	}
	if !exists {
		t.Fatal("column notify_open_channels not found on incidents after migrating to 86")
	}

	var channels []int64
	if err := pool.QueryRow(ctx,
		"SELECT notify_open_channels FROM incidents WHERE id = $1", incidentID).Scan(&channels); err != nil {
		t.Fatalf("select pre-existing incident: %v", err)
	}
	if channels != nil {
		t.Errorf("notify_open_channels for pre-existing incident = %v, want NULL (никогда не было снимка)", channels)
	}

	// Колонка реально пишется как bigint[] — то, чем её будет пользоваться
	// Detector.notifyOpen/retryStepZeroLog (Service.SetNotifyOpenChannels/
	// ClearNotifyOpenChannels).
	if _, err := pool.Exec(ctx,
		"UPDATE incidents SET notify_open_channels = $2 WHERE id = $1", incidentID, []int64{7, 9}); err != nil {
		t.Fatalf("set notify_open_channels: %v", err)
	}
	if err := pool.QueryRow(ctx,
		"SELECT notify_open_channels FROM incidents WHERE id = $1", incidentID).Scan(&channels); err != nil {
		t.Fatalf("select after set: %v", err)
	}
	if len(channels) != 2 || channels[0] != 7 || channels[1] != 9 {
		t.Errorf("notify_open_channels after set = %v, want [7 9]", channels)
	}

	// Очистка обратно в NULL (ClearNotifyOpenChannels) — логирование
	// разрешилось, ретраить больше нечего.
	if _, err := pool.Exec(ctx,
		"UPDATE incidents SET notify_open_channels = NULL WHERE id = $1", incidentID); err != nil {
		t.Fatalf("clear notify_open_channels: %v", err)
	}
	if err := pool.QueryRow(ctx,
		"SELECT notify_open_channels FROM incidents WHERE id = $1", incidentID).Scan(&channels); err != nil {
		t.Fatalf("select after clear: %v", err)
	}
	if channels != nil {
		t.Errorf("notify_open_channels after clear = %v, want NULL", channels)
	}
}

// TestMigrate0086DownDropsColumn — down зеркалит up: колонка обязана уйти
// целиком, старый бинарь (не знающий о ней) продолжает работать как раньше.
func TestMigrate0086DownDropsColumn(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	ctx := context.Background()
	dsn := testenv.PostgresDSN(t)
	if err := db.MigratePGTo(dsn, 86); err != nil {
		t.Fatalf("migrate to 86: %v", err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	if err := db.MigratePGTo(dsn, 85); err != nil {
		t.Fatalf("migrate down to 85: %v", err)
	}

	exists, err := columnExistsIn(ctx, pool, "public", "incidents", "notify_open_channels")
	if err != nil {
		t.Fatalf("columnExistsIn: %v", err)
	}
	if exists {
		t.Fatal("column notify_open_channels still present after migrating down to 85")
	}
}
