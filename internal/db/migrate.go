package db

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/clickhouse" // driver: clickhouse://
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"     // driver: pgx5://
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5/pgxpool"

	_ "github.com/ClickHouse/clickhouse-go/v2" // регистрирует database/sql драйвер "clickhouse"
)

//go:embed migrations/pg/*.sql
var pgMigrations embed.FS

//go:embed migrations/ch/*.sql
var chMigrations embed.FS

// Идемпотентна.
func MigratePG(dsn string) error {
	return up("migrations/pg", pgMigrations, pgx5URL(dsn))
}

// Нужна тестам на непустой базе: мигрируем до N-1, засеваем строки, применяем N. Продовый путь
// всегда «до конца» (MigratePG) — гейт схемы требует актуальной версии.
func MigratePGTo(dsn string, version uint) error {
	return upTo("migrations/pg", pgMigrations, pgx5URL(dsn), version)
}

// ErrNilVersion (миграции не применялись) даёт (0,false,nil) — 0 корректно значит «пусто».
func SchemaVersion(dsn string) (version uint, dirty bool, err error) {
	return schemaVersion(pgMigrations, "migrations/pg", pgx5URL(dsn))
}

// Внутренний — наружу торчит CheckSchemaCurrentCH.
func schemaVersionCH(dsn string) (version uint, dirty bool, err error) {
	return schemaVersion(chMigrations, "migrations/ch", dsn)
}

func schemaVersion(fsys embed.FS, dir, url string) (version uint, dirty bool, err error) {
	src, err := iofs.New(fsys, dir)
	if err != nil {
		return 0, false, fmt.Errorf("schema version: migrations source %s: %w", dir, err)
	}
	m, err := migrate.NewWithSourceInstance("iofs", src, url)
	if err != nil {
		return 0, false, fmt.Errorf("schema version: migrate init %s: %w", dir, err)
	}
	defer m.Close()
	version, dirty, err = m.Version()
	if errors.Is(err, migrate.ErrNilVersion) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("schema version: %w", err)
	}
	return version, dirty, nil
}

// Разрешены только текущая версия и текущая−1 — только в этих состояниях может застрять оборвавшаяся
// миграция; Force снимает признак незавершённости, а не доделывает миграцию.
func ForcePG(dsn string, target uint) error {
	return force(pgMigrations, "migrations/pg", pgx5URL(dsn), target)
}

func ForceCH(dsn string, target uint) error {
	return force(chMigrations, "migrations/ch", dsn, target)
}

// target ≥ 1 — dirty на версии 1 с ручным откатом означает пустую базу, там честнее пересоздать том.
func force(fsys embed.FS, dir, url string, target uint) error {
	if target < 1 {
		return fmt.Errorf("migrate force %s: target version must be >= 1, got %d", dir, target)
	}
	m, err := newMigrateInstance(dir, fsys, url)
	if err != nil {
		return err
	}
	defer m.Close()
	version, dirty, err := m.Version()
	if errors.Is(err, migrate.ErrNilVersion) {
		return fmt.Errorf("migrate force %s: миграции ещё не применялись — снимать нечего", dir)
	}
	if err != nil {
		return fmt.Errorf("migrate force %s: read version: %w", dir, err)
	}
	if !dirty {
		return fmt.Errorf("migrate force %s: схема на версии %d not dirty — снимать нечего", dir, version)
	}
	if target != version && target != version-1 {
		return fmt.Errorf("migrate force %s: запрошена версия %d, а dirty-схема стоит на %d — "+
			"разрешены только %d (миграция доделана руками) и %d (миграция откачена руками)",
			dir, target, version, version, version-1)
	}
	if err := m.Force(int(target)); err != nil {
		return fmt.Errorf("migrate force %s: %w", dir, err)
	}
	return nil
}

// Вызывается ДО миграций: golang-migrate на схеме впереди максимума падает невнятной ошибкой
// ("no migration found..."), раньше чем дойдёт до CheckSchemaCurrent. Остальное — не её работа.
func CheckSchemaAhead(ctx context.Context, pool *pgxpool.Pool, dsn string) error {
	want, err := maxEmbeddedPGVersion()
	if err != nil {
		return err
	}
	got, dirty, err := SchemaVersion(dsn)
	if err != nil {
		return err
	}
	return checkSchemaAhead(ctx, pool, "PG", "pg", got, dirty, want)
}

func CheckSchemaAheadCH(ctx context.Context, pool *pgxpool.Pool, dsn string) error {
	want, err := maxEmbeddedCHVersion()
	if err != nil {
		return err
	}
	got, dirty, err := schemaVersionCH(dsn)
	if err != nil {
		return err
	}
	return checkSchemaAhead(ctx, pool, "ClickHouse", "ch", got, dirty, want)
}

// nil на отставании/равенстве/dirty — не «всё хорошо», а «это не моя проверка»: их ловят
// MigratePG/MigrateCH или CheckSchemaCurrent.
func checkSchemaAhead(ctx context.Context, pool *pgxpool.Pool, label, target string, got uint, dirty bool, want uint) error {
	if dirty || got <= want {
		return nil
	}
	compat, err := loadSchemaCompat(ctx, pool, target)
	if err != nil {
		return err
	}
	warning, err := schemaAheadDecision(label, got, want, compat)
	if err != nil {
		return err
	}
	if warning != "" {
		slog.Warn(warning)
	}
	return nil
}

// Fail-fast при AUTO_MIGRATE=false — без гейта отсутствующая колонка роняет каждый insert.
func CheckSchemaCurrent(ctx context.Context, pool *pgxpool.Pool, dsn string) error {
	want, err := maxEmbeddedPGVersion()
	if err != nil {
		return err
	}
	got, dirty, err := SchemaVersion(dsn)
	if err != nil {
		return err
	}
	return checkSchema(ctx, pool, "PG", "pg", got, dirty, want)
}

// Отставшая CH-схема так же роняет каждый insert телеметрии — вызывается в main.go рядом с
// CheckSchemaCurrent при AutoMigrate=false.
func CheckSchemaCurrentCH(ctx context.Context, pool *pgxpool.Pool, dsn string) error {
	want, err := maxEmbeddedCHVersion()
	if err != nil {
		return err
	}
	got, dirty, err := schemaVersionCH(dsn)
	if err != nil {
		return err
	}
	return checkSchema(ctx, pool, "ClickHouse", "ch", got, dirty, want)
}

// Признаки читаются, только когда база впереди бинаря. Предупреждение идёт в лог, не молчит —
// администратор должен видеть, что инстанс работает не на своей версии схемы.
func checkSchema(ctx context.Context, pool *pgxpool.Pool, label, target string, got uint, dirty bool, want uint) error {
	var compat map[uint]bool
	if got > want && !dirty {
		var err error
		compat, err = loadSchemaCompat(ctx, pool, target)
		if err != nil {
			return err
		}
	}
	warning, err := schemaGateErr(label, got, dirty, want, compat)
	if err != nil {
		return err
	}
	if warning != "" {
		slog.Warn(warning)
	}
	return nil
}

// У ClickHouse свой флаг --migrate-force-ch.
func forceFlagSuffix(label string) string {
	if label == "ClickHouse" {
		return "-ch"
	}
	return ""
}

// Порядок проверок: dirty → отставание → впереди. got>want ловит старый бинарь на новой БД —
// без этой ветки даунгрейд шёл бы молча, а падал бы на первой вставке в новую колонку.
func schemaGateErr(label string, got uint, dirty bool, want uint, compat map[uint]bool) (warning string, err error) {
	if dirty {
		return "", fmt.Errorf("schema check: %s-база в состоянии dirty на версии %d — "+
			"снимите флаг перед стартом: docker compose run --rm gotcha --migrate-force%s=%d "+
			"(подробности: /docs/upgrade, раздел про dirty)",
			label, got, forceFlagSuffix(label), got)
	}
	if got < want {
		return "", fmt.Errorf("schema check: версия %s-схемы %d отстаёт от встроенной %d — "+
			"примените миграции (AUTO_MIGRATE=true или migrate up) перед стартом", label, got, want)
	}
	if got > want {
		return schemaAheadDecision(label, got, want, compat)
	}
	return "", nil
}

func maxEmbeddedPGVersion() (uint, error) {
	entries, err := pgMigrations.ReadDir("migrations/pg")
	if err != nil {
		return 0, fmt.Errorf("schema check: read migrations dir: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	max := maxMigrationVersion(names)
	if max == 0 {
		return 0, errors.New("schema check: не найдено ни одной встроенной PG-миграции")
	}
	return max, nil
}

func maxEmbeddedCHVersion() (uint, error) {
	entries, err := chMigrations.ReadDir("migrations/ch")
	if err != nil {
		return 0, fmt.Errorf("schema check: read migrations dir: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	max := maxMigrationVersion(names)
	if max == 0 {
		return 0, errors.New("schema check: не найдено ни одной встроенной CH-миграции")
	}
	return max, nil
}

// 2^31-1 — самая узкая из границ uint(32-бит)/int64/bigint: ни одно преобразование номера миграции
// не усекает значение ни на одной платформе.
const maxSchemaVersion = 1<<31 - 1

// ok=false — цифр нет вовсе либо номер больше maxSchemaVersion.
func parseMigrationVersion(name string) (uint, bool) {
	i := 0
	for i < len(name) && name[i] >= '0' && name[i] <= '9' {
		i++
	}
	if i == 0 {
		return 0, false
	}
	// bitSize=31 — номер помещается и в uint, и в int64 на любой платформе.
	n, err := strconv.ParseUint(name[:i], 10, 31)
	if err != nil {
		return 0, false
	}
	return uint(n), true
}

// Только .up.sql — иначе версия считалась бы дважды (у .down.sql тот же номер).
func maxMigrationVersion(names []string) uint {
	var max uint
	for _, name := range names {
		if !strings.HasSuffix(name, ".up.sql") {
			continue
		}
		if v, ok := parseMigrationVersion(name); ok && v > max {
			max = v
		}
	}
	return max
}

// Golang-migrate регистрирует драйвер под схемой pgx5://, а не postgres://.
func pgx5URL(dsn string) string {
	switch {
	case strings.HasPrefix(dsn, "postgres://"):
		return "pgx5://" + strings.TrimPrefix(dsn, "postgres://")
	case strings.HasPrefix(dsn, "postgresql://"):
		return "pgx5://" + strings.TrimPrefix(dsn, "postgresql://")
	}
	return dsn
}

// Идемпотентна. Multi-statement (x-multi-statement) не включаем — драйвер режет файл по любой ';',
// не разбирая литералы; для второго statement заводите отдельный файл.
func MigrateCH(dsn string) error {
	return up("migrations/ch", chMigrations, dsn)
}

func up(dir string, fsys embed.FS, url string) error {
	m, err := newMigrateInstance(dir, fsys, url)
	if err != nil {
		return err
	}
	defer m.Close()
	return explainMigrateErr(dir, m.Up())
}

func upTo(dir string, fsys embed.FS, url string, version uint) error {
	m, err := newMigrateInstance(dir, fsys, url)
	if err != nil {
		return err
	}
	defer m.Close()
	return explainMigrateErr(dir, m.Migrate(version))
}

// Общая часть up/upTo — не заводим вторую копию обработки ошибок инициализации.
func newMigrateInstance(dir string, fsys embed.FS, url string) (*migrate.Migrate, error) {
	src, err := iofs.New(fsys, dir)
	if err != nil {
		return nil, fmt.Errorf("migrations source %s: %w", dir, err)
	}
	m, err := migrate.NewWithSourceInstance("iofs", src, url)
	if err != nil {
		return nil, fmt.Errorf("migrate init %s: %w", dir, err)
	}
	return m, nil
}

// nil/ErrNoChange → nil («применять нечего»). migrate.ErrDirty оборачивается во внятную инструкцию
// force, сохраняя исходную ошибку через %w (errors.As достаёт ErrDirty).
func explainMigrateErr(dir string, err error) error {
	if err == nil || errors.Is(err, migrate.ErrNoChange) {
		return nil
	}
	var derr migrate.ErrDirty
	if errors.As(err, &derr) {
		flag := "--migrate-force"
		if strings.HasSuffix(dir, "/ch") {
			flag = "--migrate-force-ch"
		}
		return fmt.Errorf("migrate up %s: база в состоянии dirty на версии %d — "+
			"предыдущая миграция оборвалась; проверьте схему и снимите флаг: "+
			"docker compose run --rm gotcha %s=%d (подробности: /docs/upgrade, "+
			"раздел про dirty): %w", dir, derr.Version, flag, derr.Version, err)
	}
	return fmt.Errorf("migrate up %s: %w", dir, err)
}

// Вызывается на каждом старте — ретеншн задаётся инсталляцией, а не миграцией.
// Transactions и spans ретенируются отдельно (ApplyTransactionRetention/ApplySpanRetention).
func ApplyRetention(ctx context.Context, conn driver.Conn, days int) error {
	return applyTableTTL(ctx, conn, []string{"events", "check_results"}, days)
}

// Отдельный срок — спаны обычно живут короче событий.
func ApplySpanRetention(ctx context.Context, conn driver.Conn, days int) error {
	return applyTableTTL(ctx, conn, []string{"spans"}, days)
}

// ts, не timestamp — metric_points использует своё имя колонки времени. 0 снимает TTL.
func ApplyMetricRetention(ctx context.Context, conn driver.Conn, days int) error {
	return applyTableTTLColumn(ctx, conn, "metric_points", "toDateTime(ts)", days)
}

// Профили тяжёлые — ретенция по умолчанию короче (7 дней).
func ApplyProfileRetention(ctx context.Context, conn driver.Conn, days int) error {
	return applyTableTTLColumn(ctx, conn, "profile_samples", "toDateTime(ts)", days)
}

// Логи объёмны — дефолт короче спанов.
func ApplyLogRetention(ctx context.Context, conn driver.Conn, days int) error {
	return applyTableTTLColumn(ctx, conn, "logs", "toDateTime(timestamp)", days)
}

func ApplyTransactionRetention(ctx context.Context, conn driver.Conn, days int) error {
	if days < 0 {
		return fmt.Errorf("apply transaction retention: days must be >= 0, got %d", days)
	}
	if err := applyTableTTL(ctx, conn, []string{"transactions"}, days); err != nil {
		return err
	}
	// transactions_5m — MV без TO-таблицы: TTL меняется не на вьюхе (не поддерживается), а на скрытой
	// storage-таблице (.inner_id.<uuid>), по колонке bucket.
	return applyMVTTL(ctx, conn, "transactions_5m", "bucket", days)
}

// Без TTL вьюха растёт вечно, а имя транзакции может нести URL — хранить нужно ограниченный срок.
func ApplyWebVitalsRetention(ctx context.Context, conn driver.Conn, days int) error {
	if days < 0 {
		return fmt.Errorf("apply web vitals retention: days must be >= 0, got %d", days)
	}
	return applyMVTTL(ctx, conn, "web_vitals_5m", "bucket", days)
}

// TTL на самой MV менять нельзя (MaterializedView TTL не поддерживается), и SHOW CREATE TABLE
// его не покажет — работаем со скрытой storage-таблицей .inner_id.<uuid> (Atomic-БД).
func applyMVTTL(ctx context.Context, conn driver.Conn, mv, timeExpr string, days int) error {
	if days < 0 {
		return fmt.Errorf("apply retention %s: days must be >= 0, got %d", mv, days)
	}
	inner, err := mvInnerTable(ctx, conn, mv)
	if err != nil {
		return err
	}
	var ddl string
	if err := conn.QueryRow(ctx, "SHOW CREATE TABLE `"+inner+"`").Scan(&ddl); err != nil {
		return fmt.Errorf("apply retention: read ddl %s: %w", mv, err)
	}
	// days == 0 снимает TTL — НЕ MODIFY TTL + INTERVAL 0 DAY, это немедленно удалит все данные.
	if days == 0 {
		if !hasTTL(ddl) {
			return nil
		}
		if err := conn.Exec(ctx, "ALTER TABLE `"+inner+"` REMOVE TTL"); err != nil {
			return fmt.Errorf("remove ttl %s (mv %s): %w", inner, mv, err)
		}
		return nil
	}
	warnStaleBeyondRetention(ctx, conn, mv, inner, timeExpr, days)
	if !needsRetention(ddl, days) {
		return nil
	}
	q := fmt.Sprintf("ALTER TABLE `%s` MODIFY TTL %s + INTERVAL %d DAY", inner, timeExpr, days)
	if err := conn.Exec(ctx, q); err != nil {
		return fmt.Errorf("apply retention %s: %w", mv, err)
	}
	return nil
}

// Схема .inner_id.<uuid> существует только в Atomic-БД; в Ordinary uuid нулевой, и это имя было бы
// битым — при пустом/нулевом uuid возвращаем ошибку, а не .inner_id.000...000.
func mvInnerTable(ctx context.Context, conn driver.Conn, mv string) (string, error) {
	var uuid string
	err := conn.QueryRow(ctx,
		"SELECT toString(uuid) FROM system.tables "+
			"WHERE database = currentDatabase() AND name = ?", mv).Scan(&uuid)
	if err != nil {
		return "", fmt.Errorf("apply retention %s: resolve inner table: %w", mv, err)
	}
	if uuid == "" || uuid == "00000000-0000-0000-0000-000000000000" {
		return "", fmt.Errorf("apply retention %s: MV inner table requires Atomic database engine "+
			"(system.tables.uuid пуст — движок Ordinary не поддерживается)", mv)
	}
	return ".inner_id." + uuid, nil
}

func applyTableTTL(ctx context.Context, conn driver.Conn, tables []string, days int) error {
	for _, table := range tables {
		if err := applyTableTTLColumn(ctx, conn, table, "toDateTime(timestamp)", days); err != nil {
			return err
		}
	}
	return nil
}

// ALTER MODIFY TTL запускает мутацию таблицы — не дёргаем её на каждом старте, если TTL уже
// совпадает (needsRetention); REMOVE TTL — только когда TTL в DDL есть (hasTTL).
func applyTableTTLColumn(ctx context.Context, conn driver.Conn, table, timeExpr string, days int) error {
	if days < 0 {
		return fmt.Errorf("apply retention %s: days must be >= 0, got %d", table, days)
	}
	var ddl string
	if err := conn.QueryRow(ctx, "SHOW CREATE TABLE `"+table+"`").Scan(&ddl); err != nil {
		return fmt.Errorf("apply retention: read ddl %s: %w", table, err)
	}
	// days == 0 снимает TTL — НЕ MODIFY TTL + INTERVAL 0 DAY, это немедленно удалит все данные.
	if days == 0 {
		if !hasTTL(ddl) {
			return nil
		}
		if err := conn.Exec(ctx, "ALTER TABLE `"+table+"` REMOVE TTL"); err != nil {
			return fmt.Errorf("remove ttl %s: %w", table, err)
		}
		return nil
	}
	warnStaleBeyondRetention(ctx, conn, table, table, timeExpr, days)
	if !needsRetention(ddl, days) {
		return nil
	}
	q := fmt.Sprintf("ALTER TABLE `%s` MODIFY TTL %s + INTERVAL %d DAY", table, timeExpr, days)
	if err := conn.Exec(ctx, q); err != nil {
		return fmt.Errorf("apply retention %s: %w", table, err)
	}
	return nil
}

// ALTER MODIFY TTL — метаданные: уже устаревшие строки исчезают молча при следующем
// мердже, не сразу — предупреждаем явно, а не оставляем узнавать по факту.
func warnStaleBeyondRetention(ctx context.Context, conn driver.Conn, label, physicalTable, timeExpr string, days int) {
	if days <= 0 {
		return
	}
	var stale uint64
	q := fmt.Sprintf("SELECT count() FROM `%s` WHERE %s < now() - INTERVAL %d DAY", physicalTable, timeExpr, days)
	if err := conn.QueryRow(ctx, q).Scan(&stale); err != nil {
		slog.Warn("retention: stale data check failed", "table", label, "error", err)
		return
	}
	if stale > 0 {
		slog.Warn("retention: rows already older than the active window exist and will be removed "+
			"by the next TTL merge without further notice; if this follows a restore, see backup-restore.md",
			"table", label, "rows", stale, "retention_days", days)
	}
}

// Общий и для CH-миграций — у них нет своего межпроцессного лока.
const migrationLockKey int64 = 0x676f7463686101

// Реплики, стартующие одновременно, применяют миграции строго по очереди.
func WithMigrationLock(ctx context.Context, pool *pgxpool.Pool, fn func() error) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("migration lock: acquire conn: %w", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", migrationLockKey); err != nil {
		return fmt.Errorf("migration lock: %w", err)
	}
	defer func() {
		unlockCtx := context.WithoutCancel(ctx)
		if _, err := conn.Exec(unlockCtx, "SELECT pg_advisory_unlock($1)", migrationLockKey); err != nil {
			slog.Warn("db: migrate: снятие advisory lock", "err", err)
			// Если снять лок не удалось, обычный Release() отдал бы чужой лок следующему пользователю пула —
			// закрываем соединение вместо этого, чтобы pgxpool не вернул его в пул.
			conn.Conn().Close(unlockCtx)
		}
	}()
	return fn()
}

// Используется тестами up-down-up — в проде не вызывается.
func MigrateDownPG(dsn string) error {
	src, err := iofs.New(pgMigrations, "migrations/pg")
	if err != nil {
		return fmt.Errorf("migrations source pg: %w", err)
	}
	m, err := migrate.NewWithSourceInstance("iofs", src, pgx5URL(dsn))
	if err != nil {
		return fmt.Errorf("migrate init pg: %w", err)
	}
	defer m.Close()
	if err := m.Down(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("migrate down pg: %w", err)
	}
	return nil
}

// Используется тестами up-down-up — в проде не вызывается.
func MigrateDownCH(dsn string) error {
	src, err := iofs.New(chMigrations, "migrations/ch")
	if err != nil {
		return fmt.Errorf("migrations source ch: %w", err)
	}
	m, err := migrate.NewWithSourceInstance("iofs", src, dsn)
	if err != nil {
		return fmt.Errorf("migrate init ch: %w", err)
	}
	defer m.Close()
	if err := m.Down(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("migrate down ch: %w", err)
	}
	return nil
}

// TTL в SHOW CREATE TABLE нормализован в toIntervalDay(N) — сравниваем с этим форматом.
func needsRetention(ddl string, days int) bool {
	return !strings.Contains(ddl, fmt.Sprintf("toIntervalDay(%d)", days))
}

// SHOW CREATE TABLE может перенести TTL на отдельную строку — DDL нормализуем по пробелам.
func hasTTL(ddl string) bool {
	return strings.Contains(" "+strings.Join(strings.Fields(ddl), " ")+" ", " TTL ")
}
