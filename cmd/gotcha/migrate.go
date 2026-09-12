package main

import (
	"context"
	"log/slog"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/db"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/jackc/pgx/v5/pgxpool"
)

func startupStage(kind, stage string, fn func() error) error {
	slog.Info(kind+" starting", "stage", stage)
	start := time.Now()
	err := fn()
	if err != nil {
		slog.Error(kind+" failed", "stage", stage, "duration", time.Since(start), "error", err)
		return err
	}
	slog.Info(kind+" finished", "stage", stage, "duration", time.Since(start))
	return nil
}

// Текст "migration stage starting/finished/failed" — контракт:
// TestMigrationStagesAreLogged сверяет его дословно.
func migrationStage(name string, fn func() error) error {
	return startupStage("migration stage", name, fn)
}

func applyMigrations(ctx context.Context, cfg Config, pg *pgxpool.Pool, ch driver.Conn) error {
	// Сигнал во время миграций не прерывает их (golang-migrate не берёт
	// context) — процесс завершится после текущего шага.
	slog.Info("applying migrations")
	// db.WithMigrationLock блокируется без промежуточного прогресса — отдельная
	// строка лога отличает ожидание лока от зависания.
	slog.Info("waiting for migration lock")
	lockWaitStart := time.Now()
	return db.WithMigrationLock(ctx, pg, func() error {
		slog.Info("migration lock acquired", "waited", time.Since(lockWaitStart))
		// Гейт опережения PG — до MigratePG: иначе golang-migrate падает своей
		// невнятной ошибкой вместо подготовленного текста гейта.
		if err := db.CheckSchemaAhead(ctx, pg, cfg.PostgresDSN); err != nil {
			return err
		}
		// Автомиграцию можно отключить (GOTCHA_AUTO_MIGRATE_ENABLED=false) и
		// вынести в отдельный init-job, чтобы app-реплики не клинили все разом.
		if cfg.AutoMigrate {
			if err := migrationStage("postgres schema migration", func() error {
				return db.MigratePG(cfg.PostgresDSN)
			}); err != nil {
				return err
			}
			// Пишутся сразу здесь, до CH: иначе сорванная CH-миграция сделает
			// откат бинаря назад невозможным — CheckSchemaCurrent увидит "unknown".
			if err := migrationStage("postgres schema compatibility markers", func() error {
				return db.RecordSchemaCompatPG(ctx, pg)
			}); err != nil {
				return err
			}
			// Симметрично PG-гейту выше: иначе откат на опередившую CH-схему
			// падает той же невнятной ошибкой golang-migrate.
			if err := db.CheckSchemaAheadCH(ctx, pg, cfg.ClickHouseDSN); err != nil {
				return err
			}
			if err := migrationStage("clickhouse schema migration", func() error {
				return db.MigrateCH(cfg.ClickHouseDSN)
			}); err != nil {
				return err
			}
			// Симметрично PG-маркерам выше: пишутся сразу за CH-миграцией, не
			// откладываясь до конца функции.
			if err := migrationStage("clickhouse schema compatibility markers", func() error {
				return db.RecordSchemaCompatCH(ctx, pg)
			}); err != nil {
				return err
			}
			// Нужна и здесь: отставание и dirty по-прежнему ловит только эта
			// проверка, опережение уже отсечено гейтами выше.
			if err := db.CheckSchemaCurrent(ctx, pg, cfg.PostgresDSN); err != nil {
				return err
			}
			if err := db.CheckSchemaCurrentCH(ctx, pg, cfg.ClickHouseDSN); err != nil {
				return err
			}
		} else {
			// Без автомиграции app не должен стартовать на отставшей схеме —
			// иначе insert тихо теряет телеметрию на каждой вставке.
			if err := db.CheckSchemaCurrent(ctx, pg, cfg.PostgresDSN); err != nil {
				return err
			}
			if err := db.CheckSchemaCurrentCH(ctx, pg, cfg.ClickHouseDSN); err != nil {
				return err
			}
		}
		// TTL — свойство инсталляции, а задаётся окружением каждой реплики;
		// расхождение запускает пересчёт по всем кускам таблицы.
		return migrationStage("retention rollout", func() error {
			if changes, err := db.RecordRetention(ctx, pg, map[string]int{
				"events":   cfg.RetentionDays,
				"spans":    cfg.SpanRetentionDays,
				"metrics":  cfg.MetricRetentionDays,
				"profiles": cfg.ProfileRetentionDays,
				"logs":     cfg.LogRetentionDays,
			}); err != nil {
				return err
			} else {
				for _, c := range changes {
					if c.Changed() {
						slog.Warn("retention changed for the whole instance; ALTER TABLE MODIFY TTL "+
							"rewrites every part. If replicas disagree, they will flip it back and forth",
							"retention", c.Key, "previous_days", c.Previous, "new_days", c.Current)
					}
				}
			}
			if err := db.ApplyRetention(ctx, ch, cfg.RetentionDays); err != nil {
				return err
			}
			if err := db.ApplySpanRetention(ctx, ch, cfg.SpanRetentionDays); err != nil {
				return err
			}
			if err := db.ApplyMetricRetention(ctx, ch, cfg.MetricRetentionDays); err != nil {
				return err
			}
			if err := db.ApplyProfileRetention(ctx, ch, cfg.ProfileRetentionDays); err != nil {
				return err
			}
			if err := db.ApplyTransactionRetention(ctx, ch, cfg.RetentionDays); err != nil {
				return err
			}
			if err := db.ApplyLogRetention(ctx, ch, cfg.LogRetentionDays); err != nil {
				return err
			}
			// web_vitals_5m тоже получает TTL, иначе inner-таблица MV растёт
			// вечно (имя транзакции может нести URL — 152-ФЗ).
			return db.ApplyWebVitalsRetention(ctx, ch, cfg.RetentionDays)
		})
	})
}
