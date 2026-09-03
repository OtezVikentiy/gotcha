package main

import (
	"context"
	"log/slog"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/db"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/jackc/pgx/v5/pgxpool"
)

// startupStage логирует начало и конец одного именованного шага при старте
// процесса, с длительностью полем — не вклеенной в текст, потому что запись
// потом читают машины, а не только дежурный. kind называет СЕМЕЙСТВО шага в
// тексте сообщения ("migration stage", "storage poll", ...), stage — его
// конкретное имя в отдельном поле: так строки от разных вызывающих не
// путаются друг с другом, а группировка/поиск по полю stage остаётся общей.
//
// Общий примитив для двух мест, где раньше была одна и та же дыра — минуты
// (миграции) или секунды (синхронный опрос диска при регистрации метрик, см.
// registerStorageMetrics/registerUsedBytesMetric в storagemetrics.go) полной
// тишины в логе между двумя соседними строками, хотя внутри шла настоящая,
// потенциально небыстрая работа: дежурный не мог отличить «работает, потерпи»
// от «висит», а естественная реакция на видимое зависание — перезапуск.
//
// Уровень — информационный: это ожидаемая работа, а не аномалия сама по
// себе. При ошибке шаг сам пишет её на уровне Error вместе с тем, сколько
// успел проработать до отказа, и отдаёт err вызывающему — что тот дальше с
// ней сделает (упасть, как миграции, или проглотить и остаться с NaN, как
// опрос диска) решает он сам, не этот примитив.
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

// migrationStage — startupStage для этапов применения миграций. Текст
// сообщений ("migration stage starting"/"finished"/"failed") — часть
// контракта: TestMigrationStagesAreLogged (migrate_test.go) сверяет его
// дословно, поэтому кастовать сюда произвольный kind нельзя, kind фиксирован
// строкой ниже.
//
// run() добавит "gotcha failed" ещё раз при ошибке — это нормально: верхняя
// запись говорит, что процесс не стартовал, эта — что именно застряло внутри.
func migrationStage(name string, fn func() error) error {
	return startupStage("migration stage", name, fn)
}

// applyMigrations — этапы применения миграций, вынесенные из run() ради
// тестируемости (тот же приём, что и newRootMux/newServer в server.go):
// TestMigrationStagesAreLogged вызывает эту функцию напрямую и проверяет, что
// каждый этап оставляет в логе начало и конец с длительностью, а ожидание
// блокировки идёт отдельной строкой — снаружи оно единственное, что
// неотличимо от зависания.
//
// Вынос механический: код и комментарии внутри — те же, что были в run(),
// каждый шаг просто обёрнут в migrationStage(), плюс добавлена строка
// ожидания лока перед db.WithMigrationLock.
func applyMigrations(ctx context.Context, cfg Config, pg *pgxpool.Pool, ch driver.Conn) error {
	// Сигнал во время миграций не прерывает их (golang-migrate не берёт
	// context) — процесс завершится после текущего шага.
	slog.Info("applying migrations")
	// db.WithMigrationLock делает блокирующий SQL-вызов (SELECT
	// pg_advisory_lock) без промежуточного прогресса: если эта реплика ждёт,
	// пока миграции применяет другая, то единственный способ дежурного
	// отличить ожидание от зависания — увидеть отдельную строку с моментом
	// его начала и сравнить с текущим временем. Конец ожидания фиксирует
	// первая строка внутри переданной функции: раньше неё код в принципе не
	// мог туда попасть.
	slog.Info("waiting for migration lock")
	lockWaitStart := time.Now()
	return db.WithMigrationLock(ctx, pg, func() error {
		slog.Info("migration lock acquired", "waited", time.Since(lockWaitStart))
		// ARCH-M3: авто-миграцию можно отключить (GOTCHA_AUTO_MIGRATE_ENABLED=false) и
		// выносить в отдельный init-job, чтобы app-реплики не клинили все разом.
		if cfg.AutoMigrate {
			if err := migrationStage("postgres schema migration", func() error {
				return db.MigratePG(cfg.PostgresDSN)
			}); err != nil {
				return err
			}
			// Признаки обратной совместимости PG пишутся СРАЗУ здесь — до
			// попытки мигрировать ClickHouse, а не общим хвостом после обеих
			// схем (W3-D, запись 5). Раньше сорванная CH-миграция при
			// успешной PG оставляла PG-версии без единой строки в
			// schema_compat: откат бинаря назад становился невозможен
			// (CheckSchemaCurrent видит "unknown" и отказывает в старте),
			// даже когда сами PG-миграции были безопасно аддитивны и откат
			// ничем не грозил. Пишет тот, кто применял: только он содержит
			// файлы миграций и знает их маркеры. Читает любой бинарь, включая
			// откатившийся назад.
			if err := migrationStage("postgres schema compatibility markers", func() error {
				return db.RecordSchemaCompatPG(ctx, pg)
			}); err != nil {
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
			// Проверка схемы нужна И ЗДЕСЬ, после успешной миграции. Гейт ловит
			// не только отставание, но и ОПЕРЕЖЕНИЕ: схема новее встроенной —
			// значит бинарь откатили после неудачного релиза. Для m.Up() такая
			// схема выглядит как ErrNoChange, то есть старый бинарь стартовал
			// молча и падал уже на вставках. А upgrade.md обещает оператору
			// ровно обратное — внятную ошибку при старте. AUTO_MIGRATE включён
			// по умолчанию, так что до этой правки обещание не выполнялось в
			// самой частой конфигурации.
			if err := db.CheckSchemaCurrent(ctx, pg, cfg.PostgresDSN); err != nil {
				return err
			}
			if err := db.CheckSchemaCurrentCH(ctx, pg, cfg.ClickHouseDSN); err != nil {
				return err
			}
		} else {
			// RA-8: без авто-миграции app не должен стартовать на отставшей схеме
			// (иначе insert падает на каждой вставке → тихий дроп телеметрии).
			// Проверяем и PG, и CH (audit-3: CH-схема тоже нуждается в гейте).
			if err := db.CheckSchemaCurrent(ctx, pg, cfg.PostgresDSN); err != nil {
				return err
			}
			if err := db.CheckSchemaCurrentCH(ctx, pg, cfg.ClickHouseDSN); err != nil {
				return err
			}
		}
		// Расхождение сроков хранения между репликами перестаёт быть невидимым:
		// TTL — свойство инсталляции, а задаётся окружением каждой реплики, и
		// каждый переброс запускает пересчёт по всем кускам таблицы.
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
			// RA-L3 (audit-3): web_vitals_5m тоже должен получать TTL, иначе inner-таблица
			// MV растёт вечно (имя транзакции может нести URL — 152-ФЗ).
			return db.ApplyWebVitalsRetention(ctx, ch, cfg.RetentionDays)
		})
	})
}
