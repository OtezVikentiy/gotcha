package db

import (
	"embed"
	"strings"
	"testing"
)

// TestEveryMigrationDeclaresCompatMarker — страж: у каждого *.up.sql в первой
// строке есть маркер обратной совместимости.
//
// Забыть маркер нельзя: без него откат релиза через эту версию будет запрещён
// (гейт трактует неизвестное как несовместимое), и узнать об этом при следующем
// откате — худший момент для новости.
func TestEveryMigrationDeclaresCompatMarker(t *testing.T) {
	for _, tc := range []struct {
		name string
		dir  string
		load func() (map[uint]bool, error)
	}{
		{"pg", "migrations/pg", EmbeddedCompatPG},
		{"ch", "migrations/ch", EmbeddedCompatCH},
	} {
		t.Run(tc.name, func(t *testing.T) {
			compat, err := tc.load()
			if err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			var fsys = pgMigrations
			if tc.name == "ch" {
				fsys = chMigrations
			}
			entries, err := fsys.ReadDir(tc.dir)
			if err != nil {
				t.Fatalf("read %s: %v", tc.dir, err)
			}
			var ups int
			for _, e := range entries {
				if strings.HasSuffix(e.Name(), ".up.sql") {
					ups++
				}
			}
			if ups == 0 {
				t.Fatalf("в %s не найдено ни одной up-миграции — страж проверял бы пустоту", tc.dir)
			}
			if len(compat) != ups {
				t.Errorf("маркеры разобраны у %d из %d миграций в %s", len(compat), ups, tc.dir)
			}
		})
	}
}

// TestParseCompatMarker закрепляет разбор маркера: он читается только из первой
// строки и только в объявленной форме.
func TestParseCompatMarker(t *testing.T) {
	cases := []struct {
		name       string
		content    string
		wantOK     bool
		wantCompat bool
	}{
		{"yes с причиной", "-- backward-compatible: yes (новая таблица)\nCREATE TABLE t ();", true, true},
		{"no с причиной", "-- backward-compatible: no  (DROP COLUMN)\nALTER TABLE t DROP COLUMN c;", true, false},
		{"без маркера", "-- обычный комментарий\nCREATE TABLE t ();", false, false},
		{"маркер не в первой строке", "-- заголовок\n-- backward-compatible: yes\nCREATE TABLE t ();", false, false},
		{"мусор вместо значения", "-- backward-compatible: maybe\nCREATE TABLE t ();", false, false},
		{"пустой файл", "", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			compatible, ok := parseCompatMarker([]byte(tc.content))
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && compatible != tc.wantCompat {
				t.Errorf("compatible = %v, want %v", compatible, tc.wantCompat)
			}
		})
	}
}

// TestBreakingMigrationsAreMarkedBreaking — миграции, удаляющие или
// переименовывающие уже существующее, обязаны быть помечены несовместимыми.
//
// Проверка автоматическая, а не «мы внимательно прочитали»: классификация
// делается глазами один раз, ошибиться в ней можно молча, а цена ошибки —
// разрешённый откат на схему, где нужной бинарю колонки уже нет.
func TestBreakingMigrationsAreMarkedBreaking(t *testing.T) {
	for _, tc := range []struct {
		dir   string
		fsys  embed.FS
		load  func() (map[uint]bool, error)
		label string
	}{
		{"migrations/pg", pgMigrations, EmbeddedCompatPG, "pg"},
		{"migrations/ch", chMigrations, EmbeddedCompatCH, "ch"},
	} {
		t.Run(tc.label, func(t *testing.T) {
			compat, err := tc.load()
			if err != nil {
				t.Fatalf("compat: %v", err)
			}
			entries, err := tc.fsys.ReadDir(tc.dir)
			if err != nil {
				t.Fatalf("read %s: %v", tc.dir, err)
			}
			for _, e := range entries {
				name := e.Name()
				if !strings.HasSuffix(name, ".up.sql") {
					continue
				}
				body, err := tc.fsys.ReadFile(tc.dir + "/" + name)
				if err != nil {
					t.Fatalf("read %s: %v", name, err)
				}
				if !destructiveSQL(string(body)) {
					continue
				}
				if compat[maxMigrationVersion([]string{name})] {
					t.Errorf("%s удаляет или переименовывает существующее, но помечена совместимой", name)
				}
			}
		})
	}
}

// destructiveSQL — есть ли в миграции разрушительный оператор. Строки
// комментариев отбрасываются: слово DROP в объяснении миграцию разрушительной
// не делает.
func destructiveSQL(body string) bool {
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "--") {
			continue
		}
		upper := strings.ToUpper(trimmed)
		for _, op := range []string{"DROP COLUMN", "DROP TABLE", "RENAME COLUMN", "RENAME TO"} {
			if strings.Contains(upper, op) {
				return true
			}
		}
	}
	return false
}
