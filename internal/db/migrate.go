package db

import (
	"context"
	"embed"
	"errors"
	"fmt"
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

// MigratePG применяет PG-миграции. Идемпотентна.
func MigratePG(dsn string) error {
	return up("migrations/pg", pgMigrations, pgx5URL(dsn))
}

// SchemaVersion возвращает текущую версию PG-схемы и флаг dirty (обёртка над
// golang-migrate m.Version()). Если миграции ещё ни разу не применялись
// (ErrNilVersion), возвращает (0, false, nil): версия 0 корректно означает
// «пусто», а миграции нумеруются с 1 — CheckSchemaCurrent увидит отставание.
func SchemaVersion(dsn string) (version uint, dirty bool, err error) {
	return schemaVersion(pgMigrations, "migrations/pg", pgx5URL(dsn))
}

// schemaVersionCH — CH-аналог SchemaVersion: читает версию из schema_migrations
// в ClickHouse. Внутренний: наружу торчит CheckSchemaCurrentCH.
func schemaVersionCH(dsn string) (version uint, dirty bool, err error) {
	return schemaVersion(chMigrations, "migrations/ch", dsn)
}

// schemaVersion — общая реализация чтения версии схемы, параметризованная
// источником миграций и URL БД. ErrNilVersion (миграции ещё не применялись)
// трактуется как (0,false,nil): версия 0 корректно означает «пусто», а миграции
// нумеруются с 1 — гейт увидит отставание.
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

// CheckSchemaCurrent сверяет применённую версию PG-схемы со встроенным
// максимумом (по именам файлов в embed FS). Возвращает ошибку, если схема
// отстаёт, впереди встроенной или помечена dirty. Предназначена для fail-fast
// при AUTO_MIGRATE=false (RA-8): без гейта отсутствие свежей колонки роняет
// каждый insert телеметрии.
func CheckSchemaCurrent(dsn string) error {
	want, err := maxEmbeddedPGVersion()
	if err != nil {
		return err
	}
	got, dirty, err := SchemaVersion(dsn)
	if err != nil {
		return err
	}
	return schemaGateErr("PG", got, dirty, want)
}

// CheckSchemaCurrentCH — CH-аналог CheckSchemaCurrent (audit3): при
// AUTO_MIGRATE=false сверяет применённую версию CH-схемы (golang-migrate ведёт
// schema_migrations и в ClickHouse) со встроенным максимумом. RA-8 закрыл только
// PG — но отставшая CH-схема так же роняет каждый insert телеметрии. Вызывается
// из main.go при AutoMigrate=false рядом с CheckSchemaCurrent.
func CheckSchemaCurrentCH(dsn string) error {
	want, err := maxEmbeddedCHVersion()
	if err != nil {
		return err
	}
	got, dirty, err := schemaVersionCH(dsn)
	if err != nil {
		return err
	}
	return schemaGateErr("ClickHouse", got, dirty, want)
}

// schemaGateErr — чистая логика version-гейта схемы (общая для PG и CH). label
// («PG»/«ClickHouse») подставляется в текст. Порядок проверок: dirty → отставание
// → впереди встроенной. Ветка got>want (audit3) ловит запуск старого бинаря на
// новой БД: без неё даунгрейд проходит молча, а потом падает на первой вставке в
// новую колонку. Возвращает nil, когда версия ровно совпадает.
func schemaGateErr(label string, got uint, dirty bool, want uint) error {
	if dirty {
		return fmt.Errorf("schema check: %s-база в состоянии dirty на версии %d — "+
			"требуется ручной force перед стартом", label, got)
	}
	if got < want {
		return fmt.Errorf("schema check: версия %s-схемы %d отстаёт от встроенной %d — "+
			"примените миграции (AUTO_MIGRATE=true или migrate up) перед стартом", label, got, want)
	}
	if got > want {
		return fmt.Errorf("schema check: несовместимая %s-схема: база версии %d впереди "+
			"встроенной %d — обновите бинарь gotcha перед стартом", label, got, want)
	}
	return nil
}

// maxEmbeddedPGVersion возвращает максимальный номер встроенной PG-миграции,
// считая его по именам *.up.sql в embed FS.
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

// maxEmbeddedCHVersion возвращает максимальный номер встроенной CH-миграции,
// считая его по именам *.up.sql в embed FS (аналог maxEmbeddedPGVersion).
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

// maxMigrationVersion парсит список имён файлов миграций (golang-migrate:
// <version>_<name>.up.sql / .down.sql) и возвращает максимальный номер версии.
// Учитываются только .up.sql, чтобы не считать версию дважды. Файлы без ведущих
// цифр игнорируются.
func maxMigrationVersion(names []string) uint {
	var max uint
	for _, name := range names {
		if !strings.HasSuffix(name, ".up.sql") {
			continue
		}
		i := 0
		for i < len(name) && name[i] >= '0' && name[i] <= '9' {
			i++
		}
		if i == 0 {
			continue
		}
		n, err := strconv.ParseUint(name[:i], 10, 64)
		if err != nil {
			continue
		}
		if uint(n) > max {
			max = uint(n)
		}
	}
	return max
}

// pgx5URL rewrites a postgres DSN to the URL scheme registered by
// golang-migrate's pgx/v5 driver.
func pgx5URL(dsn string) string {
	switch {
	case strings.HasPrefix(dsn, "postgres://"):
		return "pgx5://" + strings.TrimPrefix(dsn, "postgres://")
	case strings.HasPrefix(dsn, "postgresql://"):
		return "pgx5://" + strings.TrimPrefix(dsn, "postgresql://")
	}
	return dsn
}

// MigrateCH применяет CH-миграции. Идемпотентна.
//
// Драйвер ClickHouse шлёт файл миграции одним Exec: в каждом файле
// migrations/ch ровно один statement. Multi-statement (x-multi-statement)
// намеренно не включаем — драйвер режет файл по любой ';', не разбирая
// строковые литералы, и одна точка с запятой внутри кавычек молча испортила
// бы миграцию. Нужен ещё один statement — заводите ещё один файл.
func MigrateCH(dsn string) error {
	return up("migrations/ch", chMigrations, dsn)
}

func up(dir string, fsys embed.FS, url string) error {
	src, err := iofs.New(fsys, dir)
	if err != nil {
		return fmt.Errorf("migrations source %s: %w", dir, err)
	}
	m, err := migrate.NewWithSourceInstance("iofs", src, url)
	if err != nil {
		return fmt.Errorf("migrate init %s: %w", dir, err)
	}
	defer m.Close()
	return explainMigrateErr(dir, m.Up())
}

// explainMigrateErr классифицирует ошибку m.Up(): nil и ErrNoChange значат
// «применять нечего» → nil. При dirty-состоянии (предыдущая миграция оборвалась
// на полпути и оставила версию помеченной грязной) golang-migrate возвращает
// migrate.ErrDirty — сам golang-migrate из него не восстановится, нужен ручной
// force. Возвращаем внятную обёртку с инструкцией; исходная ошибка сохраняется
// через %w (errors.As достаёт ErrDirty). Прочие ошибки просто оборачиваются.
func explainMigrateErr(dir string, err error) error {
	if err == nil || errors.Is(err, migrate.ErrNoChange) {
		return nil
	}
	var derr migrate.ErrDirty
	if errors.As(err, &derr) {
		return fmt.Errorf("migrate up %s: база в состоянии dirty на версии %d — "+
			"предыдущая миграция оборвалась; проверьте схему и выполните ручной force "+
			"(migrate force %d) перед перезапуском: %w", dir, derr.Version, derr.Version, err)
	}
	return fmt.Errorf("migrate up %s: %w", dir, err)
}

// ApplyRetention выставляет TTL таблиц events и check_results согласно
// конфигу инстанса. Вызывается при каждом старте: ретеншн — свойство
// инсталляции, не миграции.
//
// Таблицы transactions и MV transactions_5m управляются отдельно — см.
// ApplyTransactionRetention (у них своя колонка времени и настраиваемый TTL).
// Спаны ретенируются отдельным числом дней — см. ApplySpanRetention.
func ApplyRetention(ctx context.Context, conn driver.Conn, days int) error {
	return applyTableTTL(ctx, conn, []string{"events", "check_results"}, days)
}

// ApplySpanRetention выставляет TTL таблицы spans на отдельное число дней
// (GOTCHA_SPAN_RETENTION_DAYS): спаны обычно живут короче событий. Тот же
// механизм, что и ApplyRetention (SHOW CREATE TABLE → needsRetention →
// ALTER MODIFY TTL), вынесен в общий applyTableTTL.
func ApplySpanRetention(ctx context.Context, conn driver.Conn, days int) error {
	return applyTableTTL(ctx, conn, []string{"spans"}, days)
}

// ApplyMetricRetention выставляет TTL таблицы metric_points на days дней. Как
// ApplySpanRetention, но по колонке ts (а не timestamp): metric_points своей
// колонкой времени отличается от events/spans, поэтому применяем TTL напрямую,
// а не через applyTableTTL (тот захардкожен на timestamp). Идемпотентна через
// needsRetention.
func ApplyMetricRetention(ctx context.Context, conn driver.Conn, days int) error {
	if days < 1 {
		return fmt.Errorf("apply metric retention: days must be >= 1, got %d", days)
	}
	var ddl string
	if err := conn.QueryRow(ctx, "SHOW CREATE TABLE metric_points").Scan(&ddl); err != nil {
		return fmt.Errorf("apply metric retention: read ddl: %w", err)
	}
	if !needsRetention(ddl, days) {
		return nil
	}
	q := fmt.Sprintf("ALTER TABLE metric_points MODIFY TTL toDateTime(ts) + INTERVAL %d DAY", days)
	if err := conn.Exec(ctx, q); err != nil {
		return fmt.Errorf("apply metric retention: %w", err)
	}
	return nil
}

// ApplyProfileRetention выставляет TTL таблицы profile_samples на days дней
// (ALTER MODIFY TTL по колонке ts, как ApplyMetricRetention). Профили тяжёлые,
// поэтому ретенция по умолчанию короче (7 дней). Идемпотентна через needsRetention.
func ApplyProfileRetention(ctx context.Context, conn driver.Conn, days int) error {
	if days < 1 {
		return fmt.Errorf("apply profile retention: days must be >= 1, got %d", days)
	}
	var ddl string
	if err := conn.QueryRow(ctx, "SHOW CREATE TABLE profile_samples").Scan(&ddl); err != nil {
		return fmt.Errorf("apply profile retention: read ddl: %w", err)
	}
	if !needsRetention(ddl, days) {
		return nil
	}
	q := fmt.Sprintf("ALTER TABLE profile_samples MODIFY TTL toDateTime(ts) + INTERVAL %d DAY", days)
	if err := conn.Exec(ctx, q); err != nil {
		return fmt.Errorf("apply profile retention: %w", err)
	}
	return nil
}

// ApplyTransactionRetention приводит TTL таблицы transactions и MV
// transactions_5m к days дням. transactions хранит время в колонке timestamp
// (DateTime64) — тот же путь, что ApplyRetention; transactions_5m хранит время
// в колонке bucket (уже DateTime, toDateTime не нужен). Раньше TTL transactions
// был захардкожен миграцией на 90 днях, а у transactions_5m TTL не было вовсе.
// Вызывается на старте, как ApplyRetention. Идемпотентна через needsRetention.
func ApplyTransactionRetention(ctx context.Context, conn driver.Conn, days int) error {
	if days < 1 {
		return fmt.Errorf("apply transaction retention: days must be >= 1, got %d", days)
	}
	// transactions: колонка времени timestamp — переиспользуем общий applyTableTTL.
	if err := applyTableTTL(ctx, conn, []string{"transactions"}, days); err != nil {
		return err
	}
	// transactions_5m — MATERIALIZED VIEW без TO-таблицы: TTL нельзя менять на
	// самой вьюхе (Engine MaterializedView doesn't support TTL clause), только на
	// её внутренней storage-таблице (.inner_id.<uuid>). TTL по колонке bucket.
	return applyMVTTL(ctx, conn, "transactions_5m", "bucket", days)
}

// ApplyWebVitalsRetention приводит TTL MV web_vitals_5m к days дням. web_vitals_5m —
// такое же MATERIALIZED VIEW без TO-таблицы, как transactions_5m: TTL живёт на
// скрытой storage-таблице (.inner_id.<uuid>), считается от колонки bucket. Без
// TTL представление растёт вечно, а имя транзакции может нести URL — см. RA-L3.
// Вызывается на старте, как ApplyTransactionRetention. Идемпотентна через
// needsRetention.
func ApplyWebVitalsRetention(ctx context.Context, conn driver.Conn, days int) error {
	if days < 1 {
		return fmt.Errorf("apply web vitals retention: days must be >= 1, got %d", days)
	}
	return applyMVTTL(ctx, conn, "web_vitals_5m", "bucket", days)
}

// applyMVTTL приводит TTL внутренней storage-таблицы MATERIALIZED VIEW (без
// TO-таблицы) к days дням. На самой вьюхе TTL менять нельзя (Engine
// MaterializedView doesn't support TTL clause), да и SHOW CREATE TABLE <mv> его
// не показывает — поэтому и guard-идемпотентность, и ALTER работают по скрытой
// storage-таблице .inner_id.<uuid вьюхи> (Atomic-БД).
func applyMVTTL(ctx context.Context, conn driver.Conn, mv, timeExpr string, days int) error {
	inner, err := mvInnerTable(ctx, conn, mv)
	if err != nil {
		return err
	}
	var ddl string
	if err := conn.QueryRow(ctx, "SHOW CREATE TABLE `"+inner+"`").Scan(&ddl); err != nil {
		return fmt.Errorf("apply retention: read ddl %s: %w", mv, err)
	}
	if !needsRetention(ddl, days) {
		return nil
	}
	q := fmt.Sprintf("ALTER TABLE `%s` MODIFY TTL %s + INTERVAL %d DAY", inner, timeExpr, days)
	if err := conn.Exec(ctx, q); err != nil {
		return fmt.Errorf("apply retention %s: %w", mv, err)
	}
	return nil
}

// mvInnerTable возвращает имя скрытой storage-таблицы MATERIALIZED VIEW без
// TO-таблицы: .inner_id.<uuid вьюхи> в Atomic-БД (движок CH-миграций).
//
// Схема .inner_id.<uuid> существует только в Atomic-БД: там system.tables.uuid
// непустой. В Ordinary-БД uuid нулевой (all-zeros) — inner-таблица называется
// иначе (.inner.<name>), и построенное здесь имя было бы битым. Поэтому при
// пустом/нулевом uuid возвращаем внятную ошибку, а не .inner_id.000...000.
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

// applyTableTTL приводит TTL перечисленных таблиц к days дням по колонке
// timestamp. Идемпотентна (см. applyTableTTLColumn).
func applyTableTTL(ctx context.Context, conn driver.Conn, tables []string, days int) error {
	for _, table := range tables {
		if err := applyTableTTLColumn(ctx, conn, table, "toDateTime(timestamp)", days); err != nil {
			return err
		}
	}
	return nil
}

// applyTableTTLColumn приводит TTL одной таблицы к days дням, считая срок от
// произвольного DateTime-выражения timeExpr (например toDateTime(timestamp) для
// events или bucket для transactions_5m). Идемпотентна: ALTER ... MODIFY TTL
// запускает мутацию таблицы — не дёргаем её на каждом старте, если TTL уже
// совпадает (needsRetention по нормализованному toIntervalDay(days)).
func applyTableTTLColumn(ctx context.Context, conn driver.Conn, table, timeExpr string, days int) error {
	var ddl string
	if err := conn.QueryRow(ctx, "SHOW CREATE TABLE "+table).Scan(&ddl); err != nil {
		return fmt.Errorf("apply retention: read ddl %s: %w", table, err)
	}
	if !needsRetention(ddl, days) {
		return nil
	}
	q := fmt.Sprintf("ALTER TABLE %s MODIFY TTL %s + INTERVAL %d DAY", table, timeExpr, days)
	if err := conn.Exec(ctx, q); err != nil {
		return fmt.Errorf("apply retention %s: %w", table, err)
	}
	return nil
}

// migrationLockKey — ключ PG advisory lock, сериализующего миграции
// (в т.ч. ClickHouse-миграции, у которых нет своего межпроцессного лока).
const migrationLockKey int64 = 0x676f7463686101

// WithMigrationLock выполняет fn под session-level advisory lock в PG.
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
		_, _ = conn.Exec(context.WithoutCancel(ctx),
			"SELECT pg_advisory_unlock($1)", migrationLockKey)
	}()
	return fn()
}

// MigrateDownPG откатывает все PG-миграции. Используется тестами
// up-down-up; в проде не вызывается.
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

// MigrateDownCH откатывает все CH-миграции. Используется тестами
// up-down-up; в проде не вызывается.
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

// needsRetention: TTL в SHOW CREATE TABLE ClickHouse нормализован
// в toIntervalDay(N) — сравниваем с желаемым значением.
func needsRetention(ddl string, days int) bool {
	return !strings.Contains(ddl, fmt.Sprintf("toIntervalDay(%d)", days))
}
