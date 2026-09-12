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

// Миграция уже применена на проде и правке не подлежит: новый статус в
// issue.Statuses обязан сопровождаться НОВОЙ миграцией, а не правкой этой.
const issueStatusCheckMigrationPath = "internal/db/migrations/pg/0003_issues.up.sql"

var issueStatusCheckRe = regexp.MustCompile(`(?is)CHECK\s*\(\s*status\s+IN\s*\(([^)]*)\)\s*\)`)

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
		// SQL '...' -> Go "..." для strconv.Unquote: значения канона не содержат апострофов
		// и экранирования — узкий разбор, достаточный для этого constraint.
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
