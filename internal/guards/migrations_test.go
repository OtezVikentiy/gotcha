package guards

import (
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/db"
)

var destructiveForms = []*regexp.Regexp{
	regexp.MustCompile(`\bDROP\s+COLUMN\b`),
	regexp.MustCompile(`\bDROP\s+TABLE\b`),
	regexp.MustCompile(`\bRENAME\s+COLUMN\b`),
	regexp.MustCompile(`\bRENAME\s+TO\b`),
	// Стоит до DROP VIEW ниже для читаемости: регулярки не пересекаются
	// (MATERIALIZED между словами не даёт \bDROP\s+VIEW\b её поймать).
	regexp.MustCompile(`\bDROP\s+MATERIALIZED\s+VIEW\b`),
	// ClickHouse откатывает MaterializedView тем же DROP VIEW, что и обычный VIEW.
	regexp.MustCompile(`\bDROP\s+VIEW\b`),
	// В ClickHouse форма другая — ALTER TABLE … DROP INDEX; та же регулярка
	// ловит и её, ища подстроку, а не требуя DROP в начале оператора.
	regexp.MustCompile(`\bDROP\s+INDEX\b`),
	regexp.MustCompile(`\bDROP\s+CONSTRAINT\b`),
	// НЕ путать со сменой DEFAULT: та не запрещает то, что раньше было можно
	// и разрушительной не является — регулярка требует буквально "NOT NULL".
	regexp.MustCompile(`\bSET\s+NOT\s+NULL\b`),
	// Направление смены типа (сужение/расширение) не разбирается — сторож
	// просто требует объяснения в любом случае смены TYPE.
	regexp.MustCompile(`\bALTER\s+COLUMN\s+\S+\s+TYPE\b`),
	regexp.MustCompile(`\bMODIFY\s+COLUMN\b`),
	// Граница слова на конце обязательна — без неё совпало бы с truncated_at.
	regexp.MustCompile(`\bTRUNCATE\b`),
}

func destructiveSQL(body string) bool {
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "--") {
			continue
		}
		upper := strings.ToUpper(trimmed)
		for _, re := range destructiveForms {
			if re.MatchString(upper) {
				return true
			}
		}
	}
	return false
}

// Дублирует internal/db.parseMigrationVersion (не экспортирована): схема
// нумерации — публичное соглашение golang-migrate, не деталь пакета db.
func migrationVersion(p string) (uint, bool) {
	name := path.Base(p)
	i := 0
	for i < len(name) && name[i] >= '0' && name[i] <= '9' {
		i++
	}
	if i == 0 {
		return 0, false
	}
	n, err := strconv.ParseUint(name[:i], 10, 31)
	if err != nil {
		return 0, false
	}
	return uint(n), true
}

// Статический грep по тексту SQL, а не поведенческий тест «старый бинарь
// переживает миграцию» — не гарантирует совместимость на семантическом уровне.
func TestBreakingMigrationsAreMarkedBreaking(t *testing.T) {
	tree := Load(t)
	pgCompat, err := db.EmbeddedCompatPG()
	if err != nil {
		t.Fatalf("EmbeddedCompatPG: %v", err)
	}
	chCompat, err := db.EmbeddedCompatCH()
	if err != nil {
		t.Fatalf("EmbeddedCompatCH: %v", err)
	}

	check := func(files []File, compat map[uint]bool) {
		for _, f := range files {
			// .down.sql — сам откат, почти всегда содержит разрушительный
			// оператор по природе; маркер стоит в первой строке UP-файла.
			if !strings.HasSuffix(f.Path, ".up.sql") {
				continue
			}
			version, ok := migrationVersion(f.Path)
			if !ok {
				continue
			}
			if compat[version] && destructiveSQL(f.Body) {
				t.Errorf("%s: содержит разрушительную форму SQL, но помечена backward-compatible: yes — откат релиза через эту версию будет разрешён на схему, где старый бинарь сломается", f.Path)
			}
		}
	}
	check(tree.MigrationsPG, pgCompat)
	check(tree.MigrationsCH, chCompat)
}

func TestDestructiveSQLRecognizesForms(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"DROP COLUMN — да", "ALTER TABLE t DROP COLUMN c;", true},
		{"DROP COLUMN — нет (ADD COLUMN)", "ALTER TABLE t ADD COLUMN c int;", false},

		{"DROP TABLE — да", "DROP TABLE t;", true},
		{"DROP TABLE — нет (CREATE TABLE)", "CREATE TABLE t (id int);", false},

		{"RENAME COLUMN — да", "ALTER TABLE t RENAME COLUMN a TO b;", true},
		{"RENAME COLUMN — нет (ADD COLUMN)", "ALTER TABLE t ADD COLUMN b int;", false},

		{"RENAME TO — да", "ALTER TABLE t RENAME TO t2;", true},
		{"RENAME TO — нет (обычный ALTER)", "ALTER TABLE t ADD COLUMN c int;", false},

		{"DROP MATERIALIZED VIEW — да", "DROP MATERIALIZED VIEW IF EXISTS mv;", true},
		{"DROP MATERIALIZED VIEW — нет (CREATE)", "CREATE MATERIALIZED VIEW mv AS SELECT 1;", false},

		{"DROP VIEW — да", "DROP VIEW IF EXISTS v;", true},
		{"DROP VIEW — нет (CREATE)", "CREATE VIEW v AS SELECT 1;", false},

		{"DROP INDEX — да", "DROP INDEX idx_foo;", true},
		{"DROP INDEX — нет (CREATE)", "CREATE INDEX idx_foo ON t (c);", false},

		{"DROP CONSTRAINT — да", "ALTER TABLE t DROP CONSTRAINT fk_foo;", true},
		{"DROP CONSTRAINT — нет (ADD CONSTRAINT)", "ALTER TABLE t ADD CONSTRAINT fk_foo FOREIGN KEY (c) REFERENCES o(id);", false},

		{"SET NOT NULL — да", "ALTER TABLE t ALTER COLUMN c SET NOT NULL;", true},
		{"SET NOT NULL — нет (SET DEFAULT)", "ALTER TABLE t ALTER COLUMN c SET DEFAULT 0;", false},

		{"ALTER COLUMN ... TYPE — да", "ALTER TABLE t ALTER COLUMN c TYPE bigint;", true},
		{"ALTER COLUMN ... TYPE — нет (SET DEFAULT)", "ALTER TABLE t ALTER COLUMN c SET DEFAULT 0;", false},

		{"MODIFY COLUMN — да (ClickHouse)", "ALTER TABLE t MODIFY COLUMN c UInt32;", true},
		{"MODIFY COLUMN — нет (ADD COLUMN)", "ALTER TABLE t ADD COLUMN c UInt32;", false},

		{"TRUNCATE — да", "TRUNCATE TABLE events;", true},
		{"TRUNCATE — нет (идентификатор truncated_at — граница слова)", "SELECT truncated_at FROM events;", false},

		{"в комментарии не считается", "-- historical note: ALTER TABLE t DROP CONSTRAINT fk_old\nSELECT 1;", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := destructiveSQL(tc.body); got != tc.want {
				t.Errorf("destructiveSQL(%q) = %v, want %v", tc.body, got, tc.want)
			}
		})
	}
}

// Версия зашита в саму регулярку: иначе комментарий или имя файла с версией без вызова
// засчитались бы найденными; границы числа не дают "129" совпасть с "29".
func migratePGToCallPattern(version uint) *regexp.Regexp {
	return regexp.MustCompile(fmt.Sprintf(`MigratePGTo\([^,()]+,\s*%d\s*\)`, version))
}

// В отличие от stripTrailingComment, не отличает "/*" внутри строкового
// литерала от настоящего начала комментария — такого литерала в дереве нет.
var blockComment = regexp.MustCompile(`(?s)/\*.*?\*/`)

func stripGoComments(body string) string {
	body = blockComment.ReplaceAllString(body, "")
	lines := strings.Split(body, "\n")
	for i, line := range lines {
		lines[i] = stripTrailingComment(line)
	}
	return strings.Join(lines, "\n")
}

// Смотрит только на последнюю миграцию, не на весь список: прошлые уже накатаны везде,
// несовместимость можно поймать только на собственном релизе миграции.
func TestLatestMigrationHasDataTest(t *testing.T) {
	tree := Load(t)

	var latest uint
	seen := false
	for _, f := range tree.MigrationsPG {
		version, ok := migrationVersion(f.Path)
		if !ok {
			continue
		}
		if !seen || version > latest {
			latest = version
			seen = true
		}
	}
	if !seen {
		t.Fatalf("не нашли ни одной миграции PostgreSQL в дереве — internal/guards/tree.go сломан или каталог migrations/pg пуст")
	}

	pattern := migratePGToCallPattern(latest)
	for _, f := range tree.GoFiles {
		if !strings.HasSuffix(f.Path, "_test.go") {
			continue
		}
		// Этот же файл держит строковые литералы вида "db.MigratePGTo(dsn, 29)"
		// как тестовые данные — без исключения правило находило бы само себя.
		if strings.HasPrefix(f.Path, "internal/guards/") {
			continue
		}
		if pattern.MatchString(stripGoComments(f.Body)) {
			return
		}
	}
	t.Errorf("последняя миграция PostgreSQL — версия %04d, но среди *_test.go нет теста, который вызывает "+
		"db.MigratePGTo(..., %d): следующая миграция обязана приезжать с тестом на непустой базе "+
		"(см. internal/db/migrate_0029_test.go как образец)", latest, latest)
}

func TestStripGoCommentsIgnoresCommentedCalls(t *testing.T) {
	pattern := migratePGToCallPattern(29)
	cases := []struct {
		name string
		body string
		want bool
	}{
		{
			"рабочий вызов — да",
			`if err := db.MigratePGTo(dsn, 29); err != nil {`,
			true,
		},
		{
			"закомментирован // — нет",
			`// if err := db.MigratePGTo(dsn, 29); err != nil {`,
			false,
		},
		{
			"закомментирован /* */ на одной строке — нет",
			`/* if err := db.MigratePGTo(dsn, 29); err != nil { */`,
			false,
		},
		{
			"закомментирован /* */ на нескольких строках — нет",
			"/*\nif err := db.MigratePGTo(dsn, 29); err != nil {\n}\n*/",
			false,
		},
		{
			"URL-литерал с // не должен ложно резать код после него",
			`u := "https://example.com"; _ = u; if err := db.MigratePGTo(dsn, 29); err != nil {`,
			true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := pattern.MatchString(stripGoComments(tc.body)); got != tc.want {
				t.Errorf("stripGoComments+match(%q) = %v, want %v", tc.body, got, tc.want)
			}
		})
	}
}
