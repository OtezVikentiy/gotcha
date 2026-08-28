package guards

import (
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/issue"
)

// TestIssueStatusMatchesCheckConstraint сверяет канон issue.Statuses с
// CHECK-constraint status в 0003_issues.up.sql — седьмая копия перечня
// статусов issue, обнаруженная ревью и не покрытая
// TestNoIssueEnumLiteralCopies (тот сторож разбирает Go AST, до SQL в
// миграциях ему дела нет).
//
// Мутация, которую ловит этот тест: добавление значения "muted" в
// issue.Statuses само по себе не ловится НИЧЕМ — ни сборкой (SQL не
// компилируется вместе с Go), ни issue_enum_contract_test.go (это не Go
// литерал), ни существовавшими до этой задачи тестами; расхождение
// всплыло бы только в рантайме ошибкой Postgres при первой попытке
// записать issue со статусом "muted" — далеко от места, где статус
// добавили в канон.
//
// Миграция 0003_issues.up.sql уже применена на проде — её нельзя менять
// (что бы ни говорил канон, в БД уже колонка с ИМЕННО этим constraint), и
// не только неё: TestBreakingMigrationsAreMarkedBreaking проверяет как раз
// то, что накатанные миграции не переписываются задним числом. Отсюда
// направление сверки: не "исправить миграцию под канон", а "новый статус в
// канон обязан сопровождаться НОВОЙ миграцией", а до тех пор — падать.
//
// Способ сверки — разбор миграции самим сторожем (regexp по
// guards.Load(t).MigrationsPG), а не запрос information_schema у тестовой
// БД: guards.Tree уже читает дерево миграций в память для других сторожей
// того же пакета (TestBreakingMigrationsAreMarkedBreaking и соседи в
// migrations_test.go), и эта проверка — не про поведение живой Postgres
// (тот же constraint из той же миграции будет и в testenv.MigratedPG, раз
// down/up миграции идентичны прод-цепочке), а про соответствие ДВУХ
// ИСХОДНИКОВ: query.go и .up.sql. Гонять ради этого тестовый контейнер —
// добавлять сетевую/процессную зависимость там, где хватает regexp по уже
// прочитанному в памяти файлу.
const issueStatusCheckMigrationPath = "internal/db/migrations/pg/0003_issues.up.sql"

// issueStatusCheckRe вытаскивает список значений внутри
// `CHECK (status IN ('a','b',...))` — форма ограничения в
// 0003_issues.up.sql. Разбор нарочно узкий (не полноценный SQL-парсер):
// область — один конкретный constraint одной конкретной миграции, а не
// произвольный SQL.
var issueStatusCheckRe = regexp.MustCompile(`(?is)CHECK\s*\(\s*status\s+IN\s*\(([^)]*)\)\s*\)`)

// issueStatusCheckValues разбирает список значений внутри CHECK(status IN
// (...)). Возвращает ok=false, если constraint в тексте не найден вовсе
// (регэксп не совпал) или значение внутри списка не удалось разобрать как
// одинарно-кавыченный SQL-литерал, — вызывающий обязан упасть t.Fatalf на
// ok=false, а не молча сверяться с пустым/частичным множеством.
func issueStatusCheckValues(sql string) (values []string, ok bool) {
	m := issueStatusCheckRe.FindStringSubmatch(sql)
	if m == nil {
		return nil, false
	}
	parts := strings.Split(m[1], ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if len(p) < 2 || p[0] != '\'' || p[len(p)-1] != '\'' {
			return nil, false
		}
		// SQL '...' -> Go "..." для strconv.Unquote: значения канона (коды
		// статусов) не содержат ни апострофов, ни экранирования — узкий
		// разбор, достаточный ровно для этого constraint.
		unq, err := strconv.Unquote(`"` + p[1:len(p)-1] + `"`)
		if err != nil {
			return nil, false
		}
		out = append(out, unq)
	}
	return out, true
}

func TestIssueStatusMatchesCheckConstraint(t *testing.T) {
	tree := Load(t)
	var body string
	found := false
	for _, f := range tree.MigrationsPG {
		if f.Path == issueStatusCheckMigrationPath {
			body = f.Body
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("%s не найден в дереве миграций — переехал или переименован; обновить issueStatusCheckMigrationPath (миграция уже на проде, путь/имя файла меняться не должны)", issueStatusCheckMigrationPath)
	}

	checkValues, ok := issueStatusCheckValues(body)
	if !ok {
		t.Fatalf("%s: CHECK (status IN (...)) не распознан регэкспом issueStatusCheckRe — текст миграции изменился непредвиденным образом (а меняться не должен, она на проде)", issueStatusCheckMigrationPath)
	}

	canon := append([]string(nil), issue.Statuses...)
	sort.Strings(canon)
	got := append([]string(nil), checkValues...)
	sort.Strings(got)

	if !reflect.DeepEqual(canon, got) {
		t.Errorf("issue.Statuses %v разошёлся с CHECK-constraint issues.status в %s %v: миграция уже накатана на проде и правке не подлежит — новый статус в issue.Statuses обязан сопровождаться НОВОЙ миграцией (ALTER TABLE issues DROP CONSTRAINT ... ADD CONSTRAINT ... CHECK (status IN (...))), расширяющей constraint",
			issue.Statuses, issueStatusCheckMigrationPath, checkValues)
	}
}

// TestIssueStatusCheckValuesParsing закрепляет разбор issueStatusCheckValues
// отдельно от факта, что он верно разбирает реальную миграцию — по образцу
// TestDestructiveSQLRecognizesForms в migrations_test.go (расширение/сужение
// регэкспа не должно проходить вслепую).
func TestIssueStatusCheckValuesParsing(t *testing.T) {
	cases := []struct {
		name string
		sql  string
		want []string
		ok   bool
	}{
		{
			name: "канон как в 0003_issues.up.sql",
			sql:  "status text NOT NULL DEFAULT 'unresolved'\n    CHECK (status IN ('unresolved','resolved','ignored')),",
			want: []string{"unresolved", "resolved", "ignored"},
			ok:   true,
		},
		{
			name: "пробелы вокруг скобок и запятых",
			sql:  "CHECK ( status IN ( 'a' , 'b' ) )",
			want: []string{"a", "b"},
			ok:   true,
		},
		{
			name: "constraint отсутствует",
			sql:  "status text NOT NULL DEFAULT 'unresolved',",
			want: nil,
			ok:   false,
		},
		{
			name: "значение без кавычек — не разбирается",
			sql:  "CHECK (status IN (unresolved,'resolved'))",
			want: nil,
			ok:   false,
		},
	}
	for _, c := range cases {
		got, ok := issueStatusCheckValues(c.sql)
		if ok != c.ok {
			t.Errorf("%s: ok = %v, want %v (got %v)", c.name, ok, c.ok, got)
			continue
		}
		if ok && !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}
