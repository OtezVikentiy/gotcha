package db

import (
	"context"
	"embed"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Первая строка каждого *.up.sql: `-- backward-compatible: yes` или `-- backward-compatible: no`.
// Признак — рядом с файлом, не в общем реестре, чтобы не разъезжались при правках миграций.
var compatMarker = regexp.MustCompile(`^--\s*backward-compatible:\s*(yes|no)\b`)

// Только первая строка — иначе маркер рискует затеряться внутри обычного комментария к чему-то другому.
func parseCompatMarker(content []byte) (compatible bool, ok bool) {
	first, _, _ := strings.Cut(string(content), "\n")
	m := compatMarker.FindStringSubmatch(strings.TrimSpace(first))
	if m == nil {
		return false, false
	}
	return m[1] == "yes", true
}

// Отсутствие маркера — ошибка: молчаливая совместимость разрешила бы откат через неизвестное.
func embeddedCompat(fsys embed.FS, dir string) (map[uint]bool, error) {
	entries, err := fsys.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("schema compat: read %s: %w", dir, err)
	}
	out := make(map[uint]bool, len(entries))
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".up.sql") {
			continue
		}
		// Номер версии обязателен — без него нечего записать в schema_compat, а гейт трактует пропуск
		// как «старт запрещён»; тихий скип файла отложил бы отказ до чужого запуска.
		version, ok := parseMigrationVersion(name)
		if !ok {
			return nil, fmt.Errorf("schema compat: имя миграции %s без номера версии "+
				"(ожидается <номер>_<имя>.up.sql, номер не больше %d)", name, maxSchemaVersion)
		}
		content, err := fsys.ReadFile(dir + "/" + name)
		if err != nil {
			return nil, fmt.Errorf("schema compat: read %s: %w", name, err)
		}
		compatible, ok := parseCompatMarker(content)
		if !ok {
			return nil, fmt.Errorf("schema compat: миграция %s без маркера "+
				"«-- backward-compatible: yes|no» в первой строке", name)
		}
		out[version] = compatible
	}
	return out, nil
}

// Экспортированы для теста-стража, который требует маркер у каждого файла.
func EmbeddedCompatPG() (map[uint]bool, error) { return embeddedCompat(pgMigrations, "migrations/pg") }
func EmbeddedCompatCH() (map[uint]bool, error) { return embeddedCompat(chMigrations, "migrations/ch") }

// Идемпотентна (ON CONFLICT DO NOTHING), но не перезаписывает — строка отражает то, что реально
// применили к базе, а не то, что в файлах текущего бинаря.
// Версии обходятся по возрастанию — иначе номер в ошибке при отказе одной из вставок
// зависит от порядка обхода Go-карты и меняется от рестарта к рестарту.
func recordCompat(ctx context.Context, pool *pgxpool.Pool, target string, compat map[uint]bool) error {
	versions := make([]uint, 0, len(compat))
	for version := range compat {
		versions = append(versions, version)
	}
	sort.Slice(versions, func(i, j int) bool { return versions[i] < versions[j] })
	for _, version := range versions {
		if _, err := pool.Exec(ctx,
			`INSERT INTO schema_compat (target, version, backward_compatible)
			 VALUES ($1, $2, $3)
			 ON CONFLICT (target, version) DO NOTHING`,
			target, int64(version), compat[version]); err != nil {
			return fmt.Errorf("schema compat: record %s/%d: %w", target, version, err)
		}
	}
	return nil
}

// Пишется сразу после PG-миграции, до попытки CH: иначе сорванная CH-миграция при успешной PG
// оставляла бы PG без записи в schema_compat, и откат бинаря назад становился бы невозможен.
func RecordSchemaCompatPG(ctx context.Context, pool *pgxpool.Pool) error {
	compat, err := EmbeddedCompatPG()
	if err != nil {
		return err
	}
	return recordCompat(ctx, pool, "pg", compat)
}

// Симметрична RecordSchemaCompatPG — вызывается сразу после успешной CH-миграции.
func RecordSchemaCompatCH(ctx context.Context, pool *pgxpool.Pool) error {
	compat, err := EmbeddedCompatCH()
	if err != nil {
		return err
	}
	return recordCompat(ctx, pool, "ch", compat)
}

// Только для тестов/разовых скриптов — migrate.go её не вызывает: там PG и CH мигрируют раздельно,
// каждая пишет свой признак сразу после своей миграции (см. RecordSchemaCompatPG).
func RecordSchemaCompat(ctx context.Context, pool *pgxpool.Pool) error {
	if err := RecordSchemaCompatPG(ctx, pool); err != nil {
		return err
	}
	return RecordSchemaCompatCH(ctx, pool)
}

// Отсутствие таблицы — не ошибка, а «записей нет»: применял бинарь, не знавший о schema_compat.
func loadSchemaCompat(ctx context.Context, pool *pgxpool.Pool, target string) (map[uint]bool, error) {
	var exists bool
	if err := pool.QueryRow(ctx, "SELECT to_regclass('schema_compat') IS NOT NULL").Scan(&exists); err != nil {
		return nil, fmt.Errorf("schema compat: probe table: %w", err)
	}
	if !exists {
		return map[uint]bool{}, nil
	}
	rows, err := pool.Query(ctx,
		"SELECT version, backward_compatible FROM schema_compat WHERE target = $1", target)
	if err != nil {
		return nil, fmt.Errorf("schema compat: load %s: %w", target, err)
	}
	defer rows.Close()
	out := map[uint]bool{}
	for rows.Next() {
		var version int64
		var compatible bool
		if err := rows.Scan(&version, &compatible); err != nil {
			return nil, fmt.Errorf("schema compat: scan %s: %w", target, err)
		}
		if version > 0 {
			out[uint(version)] = compatible
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("schema compat: load %s: %w", target, err)
	}
	return out, nil
}

// Работать можно, только когда каждая версия из (want, got] помечена совместимой; неизвестная
// версия запрещает старт (fail-closed) — иначе бинарь может недосчитаться нужной колонки.
func schemaAheadDecision(label string, got, want uint, compat map[uint]bool) (warning string, err error) {
	var breaking, unknown, ahead []uint
	for v := want + 1; v <= got; v++ {
		compatible, ok := compat[v]
		switch {
		case !ok:
			unknown = append(unknown, v)
		case !compatible:
			breaking = append(breaking, v)
		default:
			ahead = append(ahead, v)
		}
	}
	if len(breaking) > 0 {
		return "", fmt.Errorf("schema check: несовместимая %s-схема: база версии %d впереди "+
			"встроенной %d, и версия %s меняет схему обратно-несовместимо — "+
			"обновите бинарь gotcha или восстановите базу из бэкапа", label, got, want,
			joinVersions(breaking))
	}
	if len(unknown) > 0 {
		return "", fmt.Errorf("schema check: несовместимая %s-схема: база версии %d впереди "+
			"встроенной %d, а о версии %s в schema_compat нет записи — "+
			"признак совместимости неизвестен, старт запрещён; обновите бинарь gotcha", label, got, want,
			joinVersions(unknown))
	}
	return fmt.Sprintf("schema check: %s-схема версии %d впереди встроенной %d; "+
		"версия %s помечена обратно-совместимой, работаем на ней",
		label, got, want, joinVersions(ahead)), nil
}

func joinVersions(vs []uint) string {
	sort.Slice(vs, func(i, j int) bool { return vs[i] < vs[j] })
	parts := make([]string, 0, len(vs))
	for _, v := range vs {
		parts = append(parts, fmt.Sprint(v))
	}
	return strings.Join(parts, ", ")
}
