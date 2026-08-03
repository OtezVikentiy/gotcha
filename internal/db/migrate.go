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

// MigratePG применяет PG-миграции. Идемпотентна.
func MigratePG(dsn string) error {
	return up("migrations/pg", pgMigrations, pgx5URL(dsn))
}

// MigratePGTo применяет миграции PostgreSQL до указанной версии включительно.
//
// Нужна тестам, проверяющим миграцию на НЕПУСТОЙ базе: мигрируем до N-1,
// засеваем строки, применяем N. Продовый путь этой функцией не пользуется —
// там всегда «до конца» (MigratePG), потому что гейт схемы требует актуальной
// версии.
func MigratePGTo(dsn string, version uint) error {
	return upTo("migrations/pg", pgMigrations, pgx5URL(dsn), version)
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

// ForcePG снимает флаг dirty с PG-схемы (аналог migrate force). Разрешены
// только два целевых номера: текущая версия (миграция доделана руками) и
// текущая−1 (миграция откачена руками) — это единственные состояния, в
// которых может застрять оборвавшаяся миграция; всё остальное — опечатка,
// которая молча сдвинула бы точку отсчёта всех будущих миграций.
// Force НЕ доделывает миграцию: он снимает признак незавершённости.
func ForcePG(dsn string, target uint) error {
	return force(pgMigrations, "migrations/pg", pgx5URL(dsn), target)
}

// ForceCH — CH-аналог ForcePG: снимает флаг dirty со схемы ClickHouse.
func ForceCH(dsn string, target uint) error {
	return force(chMigrations, "migrations/ch", dsn, target)
}

// force — общая реализация ForcePG/ForceCH. Нижняя граница target ≥ 1:
// номера миграций начинаются с 1, а dirty на версии 1 с ручным откатом —
// это пустая база, там честнее пересоздать том.
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

// CheckSchemaCurrent сверяет применённую версию PG-схемы со встроенным
// максимумом (по именам файлов в embed FS). Возвращает ошибку, если схема
// отстаёт, впереди встроенной или помечена dirty. Предназначена для fail-fast
// при AUTO_MIGRATE=false (RA-8): без гейта отсутствие свежей колонки роняет
// каждый insert телеметрии.
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

// CheckSchemaCurrentCH — CH-аналог CheckSchemaCurrent (audit3): при
// AUTO_MIGRATE=false сверяет применённую версию CH-схемы (golang-migrate ведёт
// schema_migrations и в ClickHouse) со встроенным максимумом. RA-8 закрыл только
// PG — но отставшая CH-схема так же роняет каждый insert телеметрии. Вызывается
// из main.go при AutoMigrate=false рядом с CheckSchemaCurrent.
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

// checkSchema — гейт схемы с чтением окна совместимости. label
// («PG»/«ClickHouse») идёт в текст ошибки, target («pg»/«ch») — в запрос к
// schema_compat. Признаки читаются, только когда база впереди бинаря: в
// остальных случаях они ни на что не влияют.
//
// Предупреждение о работе на более новой схеме пишется в лог, а не молчит:
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

// forceFlagSuffix подбирает суффикс флага --migrate-force по метке базы в
// тексте ошибки гейта: у ClickHouse свой флаг --migrate-force-ch.
func forceFlagSuffix(label string) string {
	if label == "ClickHouse" {
		return "-ch"
	}
	return ""
}

// schemaGateErr — чистая логика version-гейта схемы (общая для PG и CH). label
// («PG»/«ClickHouse») подставляется в текст. Порядок проверок: dirty → отставание
// → впереди встроенной. Возвращает пустое предупреждение и nil, когда версия
// ровно совпадает.
//
// Ветка got>want ловит запуск старого бинаря на новой БД: без неё даунгрейд
// проходит молча, а потом падает на первой вставке в новую колонку. Раньше она
// отказывала всегда, и следствием был невозможный откат релиза — вернуть можно
// было только из бэкапа. Теперь отказ зависит от того, что миграции сделали со
// схемой: аддитивные пропускаются с предупреждением, ломающие и неизвестные —
// нет (см. schemaAheadDecision).
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

// maxSchemaVersion — потолок номера миграции.
//
// Номер разбирается из имени файла, живёт в uint (его разрядность зависит от
// платформы) и уезжает в bigint-колонку schema_compat. 2^31-1 — самая узкая из
// этих трёх границ, поэтому ни одно превращение номера не усекает значение ни
// на одной платформе; сквозной нумерации golang-migrate (0001, 0002, …),
// которой пользуется проект, потолка хватает с большим запасом.
const maxSchemaVersion = 1<<31 - 1

// parseMigrationVersion разбирает ведущие цифры имени файла миграции
// (golang-migrate: <version>_<name>.up.sql / .down.sql).
//
// ok=false — цифр нет вовсе либо номер больше maxSchemaVersion.
func parseMigrationVersion(name string) (uint, bool) {
	i := 0
	for i < len(name) && name[i] >= '0' && name[i] <= '9' {
		i++
	}
	if i == 0 {
		return 0, false
	}
	// bitSize = 31: номер заведомо помещается и в uint, и в int64 на любой
	// платформе, поэтому дальнейшие превращения безопасны по построению
	// (закрывает CodeQL go/incorrect-integer-conversion).
	n, err := strconv.ParseUint(name[:i], 10, 31)
	if err != nil {
		return 0, false
	}
	return uint(n), true
}

// maxMigrationVersion возвращает максимальный номер версии в списке имён файлов
// миграций. Учитываются только .up.sql, чтобы не считать версию дважды. Файлы
// без разбираемого номера игнорируются.
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
	m, err := newMigrateInstance(dir, fsys, url)
	if err != nil {
		return err
	}
	defer m.Close()
	return explainMigrateErr(dir, m.Up())
}

// upTo — как up, но останавливается на конкретной версии вместо «до конца»
// (m.Migrate(version) вместо m.Up()). Используется MigratePGTo.
func upTo(dir string, fsys embed.FS, url string, version uint) error {
	m, err := newMigrateInstance(dir, fsys, url)
	if err != nil {
		return err
	}
	defer m.Close()
	return explainMigrateErr(dir, m.Migrate(version))
}

// newMigrateInstance открывает источник миграций и строит *migrate.Migrate —
// общая часть up и upTo, чтобы явно не заводить вторую копию обработки ошибок
// инициализации (она разошлась бы с первой при следующей правке).
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
