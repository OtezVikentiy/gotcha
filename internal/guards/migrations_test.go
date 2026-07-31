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

// destructiveForms — регулярки разрушительных форм SQL, которые обязаны
// требовать backward-compatible: no у up-миграции. Каждая форма — операция,
// после которой старый бинарь (не знающий об этой миграции) начинает падать
// или молча терять данные при обращении к схеме.
//
// Перенесена и расширена из internal/db/compat_internal_test.go (задача 8,
// находка №54 / QA-11): там страж знал ровно четыре формы (DROP COLUMN,
// DROP TABLE, RENAME COLUMN, RENAME TO — они и остались первыми четырьмя
// ниже), а в этом же репозитории уже встречались формы, которых он не видел
// вовсе. Отсюда и цена ошибки: страж молча пропустил
// pg/0029_team_membership_invariant.up.sql (ALTER COLUMN … SET NOT NULL и
// DROP CONSTRAINT) — маркер там оказался верным по счастливой случайности, а
// не потому, что его проверил тест.
var destructiveForms = []*regexp.Regexp{
	// DROP COLUMN — колонка исчезает целиком; старый бинарь, читающий или
	// пишущий её в SELECT/INSERT, получает ошибку на каждой такой операции.
	regexp.MustCompile(`\bDROP\s+COLUMN\b`),
	// DROP TABLE — то же самое, только про таблицу целиком.
	regexp.MustCompile(`\bDROP\s+TABLE\b`),
	// RENAME COLUMN — для старого бинаря неотличимо от DROP COLUMN: колонки
	// под ожидаемым именем больше нет.
	regexp.MustCompile(`\bRENAME\s+COLUMN\b`),
	// RENAME TO — переименование таблицы: старый бинарь ищет её по старому
	// имени.
	regexp.MustCompile(`\bRENAME\s+TO\b`),
	// DROP MATERIALIZED VIEW — стоит ДО DROP VIEW ниже не из-за приоритета
	// (регулярки не пересекаются: между DROP и VIEW тут MATERIALIZED, значит
	// \bDROP\s+VIEW\b её не поймает), а просто для читаемости — сначала более
	// специфичная форма.
	regexp.MustCompile(`\bDROP\s+MATERIALIZED\s+VIEW\b`),
	// DROP VIEW — представление, на которое опирался код/дашборд, исчезает.
	// В этом репозитории обе замеченные аудитом ClickHouse-миграции
	// (ch/0006_transactions_5m.down.sql:1, ch/0008_web_vitals_5m.down.sql:1)
	// удаляют материализованное представление именно командой DROP VIEW —
	// ClickHouse откатывает MaterializedView той же командой, что и обычный
	// VIEW, поэтому DROP MATERIALIZED VIEW выше распознаётся про запас (на
	// случай PostgreSQL-миграции с материализованным представлением), а не
	// потому что он уже где-то встретился.
	regexp.MustCompile(`\bDROP\s+VIEW\b`),
	// DROP INDEX — сам индекс данных не хранит, но у частично уникального
	// индекса (pg/0017_instance_admin.down.sql:1 — one_instance_admin) снятие
	// индекса снимает и инвариант, который он обеспечивал. В ClickHouse форма
	// другая: ALTER TABLE … DROP INDEX (ch/0015..ch/0017 down) — та же
	// регулярка ловит и её: \bDROP\s+INDEX\b ищет подстроку, не требуя, чтобы
	// DROP стояло в начале оператора.
	regexp.MustCompile(`\bDROP\s+INDEX\b`),
	// DROP CONSTRAINT — снятие ограничения молча отменяет инвариант, который
	// оно проверяло. Ровно то, что пропустил старый страж на
	// pg/0029_team_membership_invariant.up.sql:33.
	regexp.MustCompile(`\bDROP\s+CONSTRAINT\b`),
	// ALTER COLUMN … SET NOT NULL — строка со значением NULL, вставленная
	// старым бинарём (который об ограничении ещё не знает), после этой
	// миграции нарушает constraint; INSERT/UPDATE от старого бинаря начинает
	// падать (pg/0029_team_membership_invariant.up.sql — ALTER COLUMN org_id
	// SET NOT NULL). НЕ путать со сменой значения по умолчанию (SET DEFAULT,
	// см. pg/0018_quota_defaults.up.sql и pg/0020_event_quota_default.up.sql)
	// — она не запрещает то, что раньше было можно, и разрушительной не
	// является; регулярка требует буквально "SET NOT NULL", а не любую
	// ALTER COLUMN.
	regexp.MustCompile(`\bSET\s+NOT\s+NULL\b`),
	// ALTER COLUMN … TYPE — смена типа колонки может как расширять диапазон
	// (безопасно), так и сужать или менять представление (разрушительно и
	// молча теряет точность). Определить направление смены значило бы
	// научить стража сравнивать типы, а не искать разрушительную форму — он
	// просто требует объяснения в любом случае. Между "ALTER COLUMN" и "TYPE"
	// в реальном SQL всегда стоит имя колонки, поэтому между двумя словами
	// регулярки — \S+, а не литерал.
	regexp.MustCompile(`\bALTER\s+COLUMN\s+\S+\s+TYPE\b`),
	// MODIFY COLUMN — форма ClickHouse: там ALTER COLUMN … TYPE не
	// существует, смена типа делается через ALTER TABLE … MODIFY COLUMN.
	regexp.MustCompile(`\bMODIFY\s+COLUMN\b`),
	// TRUNCATE — таблица пустеет целиком одной командой. Граница слова
	// (\b) на конце обязательна: без неё регулярка совпала бы с префиксом
	// любого идентификатора вида truncated_at.
	regexp.MustCompile(`\bTRUNCATE\b`),
}

// destructiveSQL — есть ли в миграции разрушительный оператор одной из
// destructiveForms. Строки комментариев отбрасываются: слово DROP в
// объяснении миграцию разрушительной не делает.
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

// migrationVersion разбирает ведущие цифры имени файла миграции
// (golang-migrate: <версия>_<имя>.up.sql / .down.sql).
//
// Дублирует ведущую часть internal/db.parseMigrationVersion: та функция не
// экспортирована, заимствовать её напрямую нельзя, а сама схема нумерации
// (ведущие цифры перед "_") — часть публичного соглашения golang-migrate, а
// не внутренняя деталь пакета db, которая может незаметно разъехаться со
// временем. Собственно разбор МАРКЕРА (backward-compatible: yes|no) не
// дублируется — он берётся из db.EmbeddedCompatPG/CH ниже, той же функции,
// которой пользуется боевой гейт схемы.
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

// TestBreakingMigrationsAreMarkedBreaking — миграции, удаляющие или
// переименовывающие уже существующее, обязаны быть помечены несовместимыми.
//
// Перенесена из internal/db/compat_internal_test.go вместе с destructiveSQL
// (задача 8): признак совместимости берётся из db.EmbeddedCompatPG/CH — той
// же функции, которой пользуется боевой гейт схемы при старте бинаря
// (internal/db/compat.go), а не отдельной копией разбора маркера. Маркер
// либо разбирается ОДИН раз для всех потребителей, либо две копии рано или
// поздно разойдутся, и страж перестанет видеть то же самое, что видит гейт.
//
// Проверка автоматическая, а не «мы внимательно прочитали»: классификация
// делается глазами один раз, ошибиться в ней можно молча, а цена ошибки —
// разрешённый откат на схему, где нужной бинарю колонки уже нет.
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
			// .down.sql — это и есть откат: он почти всегда содержит
			// разрушительный оператор по своей природе (удалить то, что
			// накатил up), и это не находка. Маркер, который решает, разрешён
			// ли откат релиза ЧЕРЕЗ эту версию (см. schemaAheadDecision в
			// internal/db/compat.go), стоит в первой строке UP-файла, поэтому
			// разбирается он, а не down.
			if !strings.HasSuffix(f.Path, ".up.sql") {
				continue
			}
			version, ok := migrationVersion(f.Path)
			if !ok {
				// Имя без разбираемого номера — уже находка другого стража:
				// embeddedCompat (internal/db/compat.go) при сборке того же
				// самого compat-словаря вернул бы на этом файле ошибку, и
				// вызов EmbeddedCompatPG/CH выше уже упал бы через t.Fatalf.
				// Раз мы сюда дошли, каждый .up.sql версию разобрал —
				// оставляем continue защитно, а не паникуем, если это вдруг
				// перестанет быть так.
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

// TestDestructiveSQLRecognizesForms закрепляет распознавание каждой формы из
// destructiveForms таблично — отдельно от факта, что её распознают реальные
// миграции. До этой задачи destructiveSQL не тестировалась вовсе, только
// использовалась: расширение списка форм менялось бы вслепую, а сужение
// регулярки (например, обратно к голой подстроке) не поймал бы ни один тест.
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

// migratePGToCallPattern строит регулярку, ищущую вызов db.MigratePGTo (или
// MigratePGTo — из самого пакета db тоже так можно) с конкретной версией
// вторым аргументом: MigratePGTo(<dsn>, <version>).
//
// Версия зашивается в саму регулярку, а не проверяется числом отдельно от
// вызова: иначе тест, который упоминает нужную версию в комментарии или в
// названии файла (например, следующий по счёту migrate_0030_test.go создан,
// но забыли добавить сам вызов), засчитался бы найденным. Граница перед
// числом (запятая + пробелы) и после (пробелы + закрывающая скобка) не даёт
// "129" или "300" случайно совпасть с версией "29" или "30" — регулярка без
// этих границ поймала бы "29" как подстроку "129", что было бы тихой дырой в
// самой проверке.
func migratePGToCallPattern(version uint) *regexp.Regexp {
	return regexp.MustCompile(fmt.Sprintf(`MigratePGTo\([^,()]+,\s*%d\s*\)`, version))
}

// blockComment — блочный комментарий Go (/* … */). (?s) заставляет "."
// матчить перевод строки — блочный комментарий занимает больше одной строки
// сплошь и рядом (см. лицензионные шапки, но и обычный inline-вид тоже
// возможен). Разбор не отличает "/*" внутри строкового литерала от настоящего
// начала комментария (в отличие от stripTrailingComment ниже, которая для
// "//" считает чётность кавычек) — в Go-исходниках этого репозитория такого
// литерала не встречается, а цена ложного вырезания ниже цены пропущенного
// закомментированного вызова.
var blockComment = regexp.MustCompile(`(?s)/\*.*?\*/`)

// stripGoComments убирает из тела Go-файла комментарии — блочные /* … */ и
// построчные // (через stripTrailingComment, объявленную в i18n_leak_test.go
// и уже переиспользуемую css_classes_test.go — своей копии не пишем) — перед
// тем, как искать в нём вызов db.MigratePGTo.
//
// Без этого шага правило TestLatestMigrationHasDataTest засчитывало бы
// закомментированный вызов — и `// db.MigratePGTo(dsn, 29)`, и
// `/* db.MigratePGTo(dsn, 29) */` совпадали с сырым текстом файла ничуть не
// хуже работающего вызова. Ровно тот случай, ради которого правило и
// писалось: тест на непустой базе гасят как флак-нестабильный или оставляют
// «для примера», а само правило продолжало бы гореть зелёным при отсутствии
// работающей проверки.
func stripGoComments(body string) string {
	body = blockComment.ReplaceAllString(body, "")
	lines := strings.Split(body, "\n")
	for i, line := range lines {
		lines[i] = stripTrailingComment(line)
	}
	return strings.Join(lines, "\n")
}

// TestLatestMigrationHasDataTest — последняя миграция PostgreSQL обязана
// приезжать с тестом, который накатывает её через db.MigratePGTo на базу,
// уже содержащую строки (а не на чистую, как остальной набор миграций
// применяется в CI по умолчанию).
//
// Цена ошибки — ровно то, что случилось в задаче, породившей это правило: DDL
// вида ALTER TABLE … ADD COLUMN … NOT NULL без DEFAULT проходит на пустой
// таблице бесследно и падает на проде с «column contains null values», а гейт
// схемы (internal/db/compat.go) не даёт бинарю стартовать со старой версией
// схемы — простой неизбежен. Тест на непустой базе — единственный способ
// заметить это до релиза; таблично перебирать чистую схему такого не покажет
// в принципе.
//
// Правило смотрит ТОЛЬКО на последнюю миграцию, а не на весь список — и это
// осознанно, не недосмотр. Дописывать такие тесты задним числом всем
// двадцати девяти прошлым миграциям — работа без пользы: каждая из них уже
// накатана на всех работающих установках (иначе они бы не работали), так что
// единственный момент, когда несовместимая миграция ещё можно поймать до
// того, как она наделает вреда, — это её собственный релиз. Первым таким
// тестом стал internal/db/migrate_0029_test.go (написан в соседнем
// подпроекте для миграции 0029_team_membership_invariant, которая как раз
// добавляет NOT NULL на непустую колонку) — это правило лишь закрепляет
// привычку, которая до сих пор держалась только на памяти автора. Если это
// правило когда-нибудь расширят на все миграции разом или снимут как
// непонятное — расширять и снимать не нужно, для этого и написано это
// объяснение.
func TestLatestMigrationHasDataTest(t *testing.T) {
	tree := Load(t)

	// Последняя версия — максимум среди .up.sql И .down.sql: у обеих один и
	// тот же номер, но up вычислять надёжнее, если вдруг когда-то останется
	// только один из парных файлов — на итог это никак не влияет, а версию
	// таким способом можно вывести из любого из них.
	var latest uint
	seen := false
	for _, f := range tree.MigrationsPG {
		version, ok := migrationVersion(f.Path)
		if !ok {
			// Как и в TestBreakingMigrationsAreMarkedBreaking — файл без
			// разбираемой версии сам по себе находка другого стража
			// (embeddedCompat в internal/db/compat.go упал бы раньше), здесь
			// просто пропускаем, чтобы не паниковать вдвойне.
			continue
		}
		if !seen || version > latest {
			latest = version
			seen = true
		}
	}
	if !seen {
		// Если каталог миграций вдруг пуст или обход его не увидел —
		// правило обязано упасть громко, а не молча решить «требований нет»
		// и пройти зелёным. Пустой каталог миграций PostgreSQL в этом
		// продукте — само по себе внутренняя ошибка (без них нет схемы), так
		// что t.Fatalf, а не t.Skip.
		t.Fatalf("не нашли ни одной миграции PostgreSQL в дереве — internal/guards/tree.go сломан или каталог migrations/pg пуст")
	}

	pattern := migratePGToCallPattern(latest)
	for _, f := range tree.GoFiles {
		if !strings.HasSuffix(f.Path, "_test.go") {
			continue
		}
		// internal/guards/ — не в счёт. Этот же файл ниже (в
		// TestStripGoCommentsIgnoresCommentedCalls) держит строковые
		// литералы вида "db.MigratePGTo(dsn, 29)" как тестовые данные для
		// самой регулярки — это не комментарий, который stripGoComments мог
		// бы вырезать, а часть работающего Go-кода. Без этого исключения
		// правило находило бы само себя: свою же фикстуру приняло бы за
		// доказательство существования настоящего теста на непустой базе, и
		// это осталось бы верным даже если реальный тест удалить целиком.
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

// TestStripGoCommentsIgnoresCommentedCalls закрепляет отдельно от факта
// нахождения реального теста: закомментированный вызов (в любой из двух форм
// комментария Go) не должен засчитываться TestLatestMigrationHasDataTest как
// работающий. Ревьюер эмпирически проверил на раунде правок 1, что без
// stripGoComments совпадали и //, и /* */ — этот тест закрепляет починку,
// чтобы регресс сюда же не вернулся незамеченным.
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
