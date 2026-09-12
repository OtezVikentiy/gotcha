package guards

import (
	"path/filepath"
	"regexp"
	"testing"
)

const subjectColumnTablesVar = "subjectColumnTables"

var subjectColumnRe = regexp.MustCompile(`\buser_id\b|\buser_email\b|\buser_ip\b`)

// Появись в CH новая таблица с прямой колонкой субъекта, PurgeSubject мог бы её молча
// не тронуть — scanCHSchema/extractStringListVar общие с TestProjectScopedCHTablesTracked.
func TestSubjectScopedCHTablesTracked(t *testing.T) {
	root, err := findRoot()
	if err != nil {
		t.Fatal(err)
	}

	scoped, _, createdBy := scanCHSchema(t, root, subjectColumnRe)
	if len(scoped) < 2 {
		t.Fatalf("обход миграций ослеп: таблиц с колонкой субъекта найдено %d "+
			"(ожидалось не меньше 2) — сломан сам сторож, а не проверяемая схема", len(scoped))
	}

	got := extractStringListVar(t, filepath.Join(root, purgeFile), subjectColumnTablesVar)
	gotSet := map[string]bool{}
	for _, n := range got {
		gotSet[n] = true
	}

	for n := range scoped {
		if !gotSet[n] {
			t.Errorf("таблица %s (миграция %s) несёт колонку субъекта (user_id/user_email/"+
				"user_ip), но не входит в %s (%s) — стирание субъекта её не тронет",
				n, createdBy[n], subjectColumnTablesVar, purgeFile)
		}
	}
	for _, n := range got {
		if !scoped[n] {
			t.Errorf("%s (%s) содержит %q — в схеме %s нет такой таблицы с колонкой субъекта",
				subjectColumnTablesVar, purgeFile, n, chMigrationsDir)
		}
	}
}
