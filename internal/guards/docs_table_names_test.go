package guards

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var createTableRe = regexp.MustCompile(`(?i)CREATE TABLE(?:\s+IF NOT EXISTS)?\s+([a-z_][a-z0-9_]*)`)

func pgTableNames(tree *Tree) map[string]bool {
	out := map[string]bool{}
	for _, f := range tree.MigrationsPG {
		if !strings.HasSuffix(f.Path, ".up.sql") {
			continue
		}
		for _, m := range createTableRe.FindAllStringSubmatch(f.Body, -1) {
			out[strings.ToLower(m[1])] = true
		}
	}
	return out
}

const minPGTables = 50 // 60 таблиц сейчас; запас вниз против сломанного разбора CREATE TABLE

// Кандидат — snake_case из двух и более компонентов: одиночные слова
// (`host`, `status`, `log`...) — обычная проза, а не имя таблицы, и дают
// массовые ложные срабатывания. Имена метрик `gotcha_*` той же формы —
// исключаются отдельно, см. docTableMentions.
var docTableCandidateRe = regexp.MustCompile("`([a-z][a-z0-9]*(?:_[a-z0-9]+)+)`")

type docTableMention struct {
	name string
	file string
}

func docTableMentions(t *testing.T, root, lang string) []docTableMention {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(root, "internal", "docs", lang, "*.md"))
	if err != nil {
		t.Fatalf("glob доков %s: %v", lang, err)
	}
	if len(paths) == 0 {
		t.Fatalf("в internal/docs/%s не найдено ни одного .md — сторож смотрит мимо доков", lang)
	}
	var out []docTableMention
	for _, p := range paths {
		body, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("чтение %s: %v", p, err)
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			t.Fatalf("относительный путь %s: %v", p, err)
		}
		rel = filepath.ToSlash(rel)
		seen := map[string]bool{}
		for _, m := range docTableCandidateRe.FindAllStringSubmatch(string(body), -1) {
			name := m[1]
			if strings.HasPrefix(name, "gotcha_") || seen[name] {
				continue
			}
			seen[name] = true
			out = append(out, docTableMention{name: name, file: rel})
		}
	}
	return out
}

func tableTokens(name string) []string {
	return strings.Split(name, "_")
}

// contiguousSubsequence сообщает, встречается ли small как непрерывный
// отрезок big — по компонентам snake_case, а не по символам: так
// `host_id` не считается похожим на `hosts`, хотя по символам совпадение есть.
func contiguousSubsequence(big, small []string) bool {
	if len(small) > len(big) {
		return false
	}
	for i := 0; i+len(small) <= len(big); i++ {
		match := true
		for j, tok := range small {
			if big[i+j] != tok {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// resemblesTableName сужает кандидатов до тех, что реально совпадают с
// созданной таблицей или составлены из тех же компонентов имени (в любом
// порядке вложенности) — иначе обычные составные слова прозы (случайно
// snake_case) дают ложные срабатывания на исправной документации. Слепая
// зона: имя, не делящее ни одного целого компонента ни с одной таблицей
// (включая однословные имена таблиц — регексп кандидатов требует минимум
// два компонента), кандидатом не станет и проверку не пройдёт вовсе.
func resemblesTableName(name string, tables map[string]bool) bool {
	nameToks := tableTokens(name)
	for table := range tables {
		tableToks := tableTokens(table)
		if contiguousSubsequence(nameToks, tableToks) || contiguousSubsequence(tableToks, nameToks) {
			return true
		}
	}
	return false
}

const minDocTableCandidates = 12 // 18 кандидатов сейчас (9 в каждой локали); запас вниз против дрожания

// Направление одно: имя таблицы из доков обязано существовать среди
// созданных миграциями. Обратное («каждая таблица задокументирована») не
// проверяется — документировать все таблицы никто не обещал.
func TestDocTableNamesExistInMigrations(t *testing.T) {
	tree := Load(t)
	tables := pgTableNames(tree)
	if len(tables) < minPGTables {
		t.Fatalf("в миграциях PostgreSQL найдено %d таблиц при пороге %d — сломан разбор CREATE TABLE, а не миграции", len(tables), minPGTables)
	}

	total := 0
	for _, lang := range []string{"ru", "en"} {
		for _, m := range docTableMentions(t, tree.Root, lang) {
			if !resemblesTableName(m.name, tables) {
				continue
			}
			total++
			t.Logf("%s: кандидат в имя таблицы %q", m.file, m.name)
			if !tables[m.name] {
				t.Errorf("%s: имя таблицы %q не найдено среди созданных миграциями PostgreSQL", m.file, m.name)
			}
		}
	}
	if total < minDocTableCandidates {
		t.Fatalf("из доков отобрано %d кандидатов в имена таблиц при пороге %d — сломан отбор, а не доки", total, minDocTableCandidates)
	}
}
