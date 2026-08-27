package guards

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// Сторож класса на список ClickHouse-таблиц, живущий копиями в трёх местах:
// в whitelist'е internal/telemetry/purge.go (кто чистится при удалении
// проекта/субъекта), в internal/docs/{ru,en}/backup-restore.md (что снимает
// оператор в бэкап) — а истина лежит в четвёртом месте, в самой схеме
// (internal/db/migrations/ch/*.up.sql), и её никто автоматически не сверял.
//
// Расхождение уже случалось: таблица logs приехала с приёмом логов и не
// попала ни в purge.go, ни в доку бэкапа — удаление проекта оставляло логи в
// ClickHouse навсегда, а восстановление из бэкапа по инструкции молча теряло
// всю историю логов. Класс проблемы тот же, что уже ловили на
// autoBufferCapUnits в buffer_units_test.go: список копируется руками, схема
// меняется в одном месте — копии расходятся молча, потому что расхождение
// остаётся валидным Go и валидным Markdown.
//
// Сторож выводит истину ИЗ СХЕМЫ (CREATE TABLE / CREATE MATERIALIZED VIEW с
// колонкой project_id) и сверяет с обеими копиями:
//   - purge.go: literal projectTables обязан содержать РОВНО все
//     project-scoped таблицы и MV схемы (без пропусков и без лишних имён);
//   - backup-restore.md (ru и en, оба вхождения "for t in ..." в каждом):
//     список обязан быть РОВНО базовыми project-scoped ТАБЛИЦАМИ (без MV — они
//     производные и пересобираются, в бэкап не входят).
//
// Каждое из четырёх вхождений сверяется НАПРЯМУЮ со схемой, а не друг с
// другом: сравнение вхождений между собой делает испорченное первое похожим
// на эталон, а остальные (исправные) — на расхождение с ним, и сообщение об
// ошибке отправляет чинить не то место.

const (
	chMigrationsDir  = "internal/db/migrations/ch"
	purgeFile        = "internal/telemetry/purge.go"
	projectTablesVar = "projectTables"
)

var backupDocs = []string{
	"internal/docs/ru/backup-restore.md",
	"internal/docs/en/backup-restore.md",
}

// Ограничения разбора миграций (сегодня ни одно не стреляет — в схеме нет
// таких случаев, — но следующий читатель обязан знать границы сторожа):
//   - createStmtRe требует "CREATE TABLE"/"CREATE MATERIALIZED VIEW" ЗАГЛАВНЫМИ
//     буквами с начала строки, без префикса имени БД (db.table) и без
//     бэктиков вокруг имени — миграции проекта всегда пишутся так, но формально
//     это не проверяется отдельно;
//   - DROP учитывается (dropStmtRe ниже) для таблицы/MV, удалённой ПОЗДНЕЙ
//     миграцией: имя пропадает из scoped-множеств независимо от того, в каком
//     файле относительно CREATE лежит DROP. Пересоздание той же таблицы ПОСЛЕ
//     DROP (drop → create заново) сторож не отследит верно — такого паттерна
//     в миграциях проекта нет.
var (
	createStmtRe = regexp.MustCompile(`(?im)^CREATE\s+(TABLE|MATERIALIZED\s+VIEW)\s+(?:IF NOT EXISTS\s+)?(\w+)`)
	dropStmtRe   = regexp.MustCompile(`(?im)^DROP\s+(TABLE|MATERIALIZED\s+VIEW)\s+(?:IF EXISTS\s+)?(\w+)`)
	projectIDRe  = regexp.MustCompile(`\bproject_id\b`)
	forLoopRe    = regexp.MustCompile(`for t in ([a-zA-Z0-9_ ]+); do`)
)

func TestProjectScopedCHTablesTracked(t *testing.T) {
	root, err := findRoot()
	if err != nil {
		t.Fatal(err)
	}

	scopedTables, scopedViews, createdBy := scanCHSchema(t, root)

	// Нижняя граница — только на то, что даёт обход схемы: обход, нашедший
	// меньше, сломан сам, и без этой проверки пустой результат совпал бы с
	// пустым списком purgeTables.
	if len(scopedTables) < 7 {
		t.Fatalf("обход миграций ослеп: project-scoped таблиц в схеме найдено %d "+
			"(ожидалось не меньше 7) — сломан сам сторож, а не проверяемая схема",
			len(scopedTables))
	}
	if len(scopedViews) < 2 {
		t.Fatalf("обход миграций ослеп: project-scoped материализованных представлений "+
			"найдено %d (ожидалось не меньше 2) — сломан сам сторож, а не проверяемая схема",
			len(scopedViews))
	}

	wantPurge := map[string]bool{}
	for n := range scopedTables {
		wantPurge[n] = true
	}
	for n := range scopedViews {
		wantPurge[n] = true
	}

	gotPurge := extractProjectTables(t, root)
	gotPurgeSet := map[string]bool{}
	for _, n := range gotPurge {
		gotPurgeSet[n] = true
	}

	for n := range wantPurge {
		if !gotPurgeSet[n] {
			t.Errorf("таблица %s создана миграцией %s, project-scoped, но не входит в "+
				"%s (%s) — удаление проекта её не тронет", n, createdBy[n], projectTablesVar, purgeFile)
		}
	}
	for _, n := range gotPurge {
		if !wantPurge[n] {
			t.Errorf("%s (%s) содержит %q — в схеме %s нет такой project-scoped таблицы или "+
				"MV, лишнее имя в списке", projectTablesVar, purgeFile, n, chMigrationsDir)
		}
	}

	// backup-restore.md: каждое вхождение "for t in ..." сверяется НАПРЯМУЮ со
	// схемой, а не друг с другом. Схема — единственный источник истины: если
	// сверять вхождения между собой, испорченное ПЕРВОЕ вхождение выглядит
	// эталоном, а остальные (исправные) — расходящимися с ним, и сообщение
	// об ошибке указывает разработчику чинить не то. Список — только базовые
	// project-scoped ТАБЛИЦЫ, MV в бэкап не входят (они производные).
	for _, doc := range backupDocs {
		lists := extractBackupLoops(t, root, doc)
		for i, list := range lists {
			got := map[string]bool{}
			for _, n := range list {
				got[n] = true
			}
			for n := range scopedTables {
				if !got[n] {
					t.Errorf("%s, вхождение №%d 'for t in ...': нет таблицы %q, созданной "+
						"миграцией %s — восстановление по инструкции молча потеряет её данные",
						doc, i+1, n, createdBy[n])
				}
			}
			for _, n := range list {
				if !scopedTables[n] {
					t.Errorf("%s, вхождение №%d 'for t in ...': содержит %q — в схеме %s "+
						"нет такой project-scoped базовой таблицы (или это MV, а MV в бэкап "+
						"не входят)", doc, i+1, n, chMigrationsDir)
				}
			}
		}
	}
}

// scanCHSchema разбирает internal/db/migrations/ch/*.up.sql и возвращает имена
// project-scoped таблиц и материализованных представлений отдельно (таблица
// или MV считается project-scoped, если слово project_id встречается в тексте
// её CREATE-выражения — как колонка в CREATE TABLE, как элемент SELECT/ORDER BY
// в CREATE MATERIALIZED VIEW), а также createdBy — имя файла миграции,
// создавшей каждую из них (для сообщений об ошибках, «что делать»). Таблица
// или MV, удалённая более поздней миграцией (DROP), из scoped-множеств
// убирается: она больше не часть текущей схемы.
func scanCHSchema(t *testing.T, root string) (tables, views map[string]bool, createdBy map[string]string) {
	t.Helper()
	dir := filepath.Join(root, chMigrationsDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("чтение %s: %v", chMigrationsDir, err)
	}

	var files []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".up.sql") {
			files = append(files, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(files)
	if len(files) == 0 {
		t.Fatalf("%s: не найдено ни одного .up.sql — сторож ослеп, а не миграции пропали", chMigrationsDir)
	}

	tables = map[string]bool{}
	views = map[string]bool{}
	createdBy = map[string]string{}
	var dropped []string
	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("чтение %s: %v", f, err)
		}
		text := string(src)
		base := filepath.Base(f)

		locs := createStmtRe.FindAllStringSubmatchIndex(text, -1)
		for i, loc := range locs {
			end := len(text)
			if i+1 < len(locs) {
				end = locs[i+1][0]
			}
			body := text[loc[0]:end]
			kind := text[loc[2]:loc[3]]
			name := text[loc[4]:loc[5]]
			if !projectIDRe.MatchString(body) {
				continue
			}
			if strings.Contains(strings.ToUpper(kind), "VIEW") {
				views[name] = true
			} else {
				tables[name] = true
			}
			createdBy[name] = base
		}

		for _, m := range dropStmtRe.FindAllStringSubmatch(text, -1) {
			dropped = append(dropped, m[2])
		}
	}

	// DROP применяется после полного обхода: имя таблицы/MV уникально в схеме
	// проекта, и в каком файле относительно её CREATE лежит DROP — не важно,
	// важно только то, что в итоговой схеме её больше нет.
	for _, name := range dropped {
		delete(tables, name)
		delete(views, name)
	}

	return tables, views, createdBy
}

// extractProjectTables достаёт AST-разбором literal []string из объявления
// projectTables в purge.go. Пустой результат недопустим — это ослепший
// сторож, а не пустой whitelist (PurgeProject с пустым списком не удалял бы
// вообще ничего, такой код не мог бы существовать).
func extractProjectTables(t *testing.T, root string) []string {
	t.Helper()
	fset := token.NewFileSet()
	path := filepath.Join(root, purgeFile)
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("разбор %s: %v", purgeFile, err)
	}

	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.VAR {
			continue
		}
		for _, sp := range gd.Specs {
			vs, ok := sp.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, name := range vs.Names {
				if name.Name != projectTablesVar || i >= len(vs.Values) {
					continue
				}
				cl, ok := vs.Values[i].(*ast.CompositeLit)
				if !ok {
					continue
				}
				var out []string
				for _, el := range cl.Elts {
					lit, ok := el.(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						continue
					}
					v, err := strconv.Unquote(lit.Value)
					if err != nil {
						continue
					}
					out = append(out, v)
				}
				return out
			}
		}
	}
	t.Fatalf("%s: не найден литерал %s — сторож ослеп, а не код исправился", purgeFile, projectTablesVar)
	return nil
}

// extractBackupLoops находит в доке все вхождения "for t in X Y Z; do" и
// возвращает списки таблиц по порядку появления. Пустой результат — ослепший
// сторож: сама инструкция бэкапа без единого такого цикла быть не может.
func extractBackupLoops(t *testing.T, root, relPath string) [][]string {
	t.Helper()
	path := filepath.Join(root, relPath)
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("чтение %s: %v", relPath, err)
	}
	matches := forLoopRe.FindAllStringSubmatch(string(src), -1)
	if len(matches) == 0 {
		t.Fatalf("%s: не найдено ни одного 'for t in ...; do' — сторож ослеп, а не документация исправилась", relPath)
	}
	out := make([][]string, 0, len(matches))
	for _, m := range matches {
		out = append(out, strings.Fields(m[1]))
	}
	return out
}
