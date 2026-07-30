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

// compatMarker — маркер обратной совместимости, первая строка каждого *.up.sql:
//
//	-- backward-compatible: yes (новая таблица)
//	-- backward-compatible: no  (DROP COLUMN)
//
// Признак лежит рядом с тем, к чему относится, а не в отдельном реестре:
// реестр разъехался бы с миграциями на первой же правке. Скобка с причиной
// обязательна — она объясняет решение тому, кто читает файл через год.
var compatMarker = regexp.MustCompile(`^--\s*backward-compatible:\s*(yes|no)\b`)

// parseCompatMarker читает маркер из первой строки миграции.
//
// Первой строки, а не любой: маркер, разрешённый где угодно, рано или поздно
// окажется внутри длинного комментария к чему-то другому.
func parseCompatMarker(content []byte) (compatible bool, ok bool) {
	first, _, _ := strings.Cut(string(content), "\n")
	m := compatMarker.FindStringSubmatch(strings.TrimSpace(first))
	if m == nil {
		return false, false
	}
	return m[1] == "yes", true
}

// embeddedCompat собирает признаки совместимости всех встроенных миграций
// каталога. Отсутствие маркера — ошибка: молча считать миграцию совместимой
// значит разрешить откат через неизвестное.
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
		version := maxMigrationVersion([]string{name})
		if version == 0 {
			continue
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

// EmbeddedCompatPG и EmbeddedCompatCH — признаки встроенных миграций. Наружу
// торчат для теста-стража, который требует маркер у каждого файла.
func EmbeddedCompatPG() (map[uint]bool, error) { return embeddedCompat(pgMigrations, "migrations/pg") }
func EmbeddedCompatCH() (map[uint]bool, error) { return embeddedCompat(chMigrations, "migrations/ch") }

// RecordSchemaCompat записывает признаки применённых миграций обеих схем.
//
// Вызывается СРАЗУ после успешного применения обеих схем — тем, кто их
// применял: только он содержит эти файлы и знает их маркеры. Отсюда и
// отсутствие параметра «до какой версии»: успешный Up означает, что применены
// все встроенные миграции, и любая передаваемая версия была бы вторым,
// расходящимся утверждением о том же.
//
// Идемпотентна: ON CONFLICT DO NOTHING. Перезаписывать существующую строку
// нельзя — она отражает то, что реально применяли к этой базе, а не то, что
// написано в файлах текущего бинаря.
func RecordSchemaCompat(ctx context.Context, pool *pgxpool.Pool) error {
	pgCompat, err := EmbeddedCompatPG()
	if err != nil {
		return err
	}
	chCompat, err := EmbeddedCompatCH()
	if err != nil {
		return err
	}
	for _, s := range []struct {
		target string
		compat map[uint]bool
	}{
		{"pg", pgCompat},
		{"ch", chCompat},
	} {
		for version, compatible := range s.compat {
			if _, err := pool.Exec(ctx,
				`INSERT INTO schema_compat (target, version, backward_compatible)
				 VALUES ($1, $2, $3)
				 ON CONFLICT (target, version) DO NOTHING`,
				s.target, int64(version), compatible); err != nil {
				return fmt.Errorf("schema compat: record %s/%d: %w", s.target, version, err)
			}
		}
	}
	return nil
}

// loadSchemaCompat читает признаки, записанные при применении миграций.
//
// Отсутствие таблицы — не ошибка, а ответ «записей нет»: схему применял бинарь,
// который о ней не знал. Такое состояние гейт трактует как несовместимое.
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

// schemaAheadDecision решает, может ли бинарь работать с базой, которая впереди
// него, и возвращает текст предупреждения для лога.
//
// Работать разрешено, только когда КАЖДАЯ версия из (want, got] помечена
// совместимой. Неизвестная версия запрещает старт: отсутствие записи означает,
// что схему применял бинарь, не знавший о признаке, и утверждать о ней нечего.
// Это правило fail-closed намеренно — ошибиться здесь значит стартовать на
// схеме, где нужной бинарю колонки уже нет, и получить не отказ при старте, а
// ошибку на каждой вставке телеметрии.
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
