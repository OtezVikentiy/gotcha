package log

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// errCapture — часовой: метод обязан вернуть именно его, иначе перехват
// не сработал и голден-проверка ничего не доказывает.
var errCapture = errors.New("capture")

// captureConn запоминает последний запрос и аргументы. Встроенный nil-интерфейс
// даёт остальные методы driver.Conn: их вызов запаникует, что и нужно —
// тест обязан падать, а не молча проходить, если код пойдёт другим путём.
type captureConn struct {
	driver.Conn
	query string
	args  []any
}

func (c *captureConn) Query(_ context.Context, query string, args ...any) (driver.Rows, error) {
	c.query = query
	c.args = args
	return nil, errCapture
}

// containsArg ищет строковое значение среди перехваченных аргументов запроса.
// Не slices.Contains: go.mod этого модуля фиксирует "go 1.26.6", и в этой
// связке версий инстанцирование slices.Contains по []any (E=any) не проходит
// проверку ограничения comparable на этапе компиляции — не имеет отношения
// к содержимому теста, обходится обычным циклом с явным приведением типа.
func containsArg(args []any, v string) bool {
	for _, a := range args {
		if s, ok := a.(string); ok && s == v {
			return true
		}
	}
	return false
}

// goldenFilter — один и тот же набор условий для всех голден-случаев:
// задействует каждую ветку сборки WHERE.
func goldenFilter() ListFilter {
	return ListFilter{
		From:        time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
		To:          time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC),
		Severity:    []string{SevWarn, SevError},
		Service:     "api",
		Environment: "prod",
		Query:       "timeout",
		Attrs: []AttrFilter{
			{Key: "source", Value: "nginx"},
			{Resource: true, Key: "host", Value: "web-1"},
		},
		TraceID: "abc123",
		Limit:   50,
	}
}

func TestGoldenWhereList(t *testing.T) {
	c := &captureConn{}
	q := NewQuery(c)

	if _, err := q.List(context.Background(), 7, goldenFilter()); !errors.Is(err, errCapture) {
		t.Fatalf("List: ожидалась errCapture, получено %v", err)
	}

	const want = `
		SELECT timestamp, observed_ts, severity, severity_number, severity_text,
			body, trace_id, span_id, log_attributes, resource_attrs, service, environment
		FROM logs
		WHERE project_id = ? AND timestamp >= toDateTime64(?, 3) AND timestamp < toDateTime64(?, 3) AND severity IN (?) AND service = ? AND environment = ? AND positionCaseInsensitiveUTF8(body, ?) > 0 AND log_attributes[?] = ? AND resource_attrs[?] = ? AND trace_id = ?
		ORDER BY timestamp DESC,
			cityHash64(observed_ts, severity_number, severity_text, body, trace_id, span_id,
				toString(log_attributes), toString(resource_attrs), service, environment) DESC
		LIMIT ?
		SETTINGS max_execution_time = 20`
	if c.query != want {
		t.Fatalf("SQL изменился.\nПОЛУЧЕНО:\n%s\nОЖИДАЛОСЬ:\n%s", c.query, want)
	}

	wantArgs := []any{
		uint64(7),
		chTimeArg(goldenFilter().From),
		chTimeArg(goldenFilter().To),
		[]string{SevWarn, SevError},
		"api", "prod", "timeout",
		"source", "nginx",
		"host", "web-1",
		"abc123",
		50,
	}
	if len(c.args) != len(wantArgs) {
		t.Fatalf("аргументов %d, ожидалось %d: %#v", len(c.args), len(wantArgs), c.args)
	}
	for i := range wantArgs {
		if fmt.Sprintf("%#v", c.args[i]) != fmt.Sprintf("%#v", wantArgs[i]) {
			t.Errorf("arg[%d] = %#v, ожидалось %#v", i, c.args[i], wantArgs[i])
		}
	}
}

func TestGoldenWhereHistogram(t *testing.T) {
	c := &captureConn{}
	q := NewQuery(c)

	if _, _, err := q.Histogram(context.Background(), 7, goldenFilter(), 24); !errors.Is(err, errCapture) {
		t.Fatalf("Histogram: ожидалась errCapture, получено %v", err)
	}

	const want = `
		SELECT toStartOfInterval(timestamp, INTERVAL ? second) AS t, severity, count() AS c
		FROM logs
		WHERE project_id = ? AND timestamp >= toDateTime64(?, 3) AND timestamp < toDateTime64(?, 3) AND severity IN (?) AND service = ? AND environment = ? AND positionCaseInsensitiveUTF8(body, ?) > 0 AND log_attributes[?] = ? AND resource_attrs[?] = ? AND trace_id = ?
		GROUP BY t, severity
		ORDER BY t
		SETTINGS max_execution_time = 10`
	if c.query != want {
		t.Fatalf("SQL изменился.\nПОЛУЧЕНО:\n%s\nОЖИДАЛОСЬ:\n%s", c.query, want)
	}

	// stepSec = (To-From)/buckets = 86400/24 = 3600, первым в args — именно он,
	// а не project_id: рефактор обязан сохранить этот порядок.
	wantArgs := []any{
		int64(3600),
		uint64(7),
		chTimeArg(goldenFilter().From),
		chTimeArg(goldenFilter().To),
		[]string{SevWarn, SevError},
		"api", "prod", "timeout",
		"source", "nginx",
		"host", "web-1",
		"abc123",
	}
	if len(c.args) != len(wantArgs) {
		t.Fatalf("аргументов %d, ожидалось %d: %#v", len(c.args), len(wantArgs), c.args)
	}
	for i := range wantArgs {
		if fmt.Sprintf("%#v", c.args[i]) != fmt.Sprintf("%#v", wantArgs[i]) {
			t.Errorf("arg[%d] = %#v, ожидалось %#v", i, c.args[i], wantArgs[i])
		}
	}
}

func TestGoldenWhereFacet(t *testing.T) {
	cases := []struct {
		col      string
		want     string
		wantArgs []any
	}{
		{
			// severity: собственное условие "severity IN (?)" НЕ добавляется —
			// фасет обязан показывать распределение по всем уровням (см. докблок
			// Facet). Это поведение фиксируется здесь наравне с текстом запроса.
			col: "severity",
			want: `
		SELECT severity, count() AS c
		FROM logs
		WHERE project_id = ? AND timestamp >= toDateTime64(?, 3) AND timestamp < toDateTime64(?, 3) AND severity != '' AND service = ? AND environment = ? AND positionCaseInsensitiveUTF8(body, ?) > 0 AND log_attributes[?] = ? AND resource_attrs[?] = ? AND trace_id = ?
		GROUP BY severity
		ORDER BY c DESC
		LIMIT ?
		SETTINGS max_execution_time = 5`,
			wantArgs: []any{
				uint64(7),
				chTimeArg(goldenFilter().From),
				chTimeArg(goldenFilter().To),
				"api", "prod", "timeout",
				"source", "nginx",
				"host", "web-1",
				"abc123",
				facetLimit,
			},
		},
		{
			col: "service",
			want: `
		SELECT service, count() AS c
		FROM logs
		WHERE project_id = ? AND timestamp >= toDateTime64(?, 3) AND timestamp < toDateTime64(?, 3) AND service != '' AND severity IN (?) AND service = ? AND environment = ? AND positionCaseInsensitiveUTF8(body, ?) > 0 AND log_attributes[?] = ? AND resource_attrs[?] = ? AND trace_id = ?
		GROUP BY service
		ORDER BY c DESC
		LIMIT ?
		SETTINGS max_execution_time = 5`,
			wantArgs: []any{
				uint64(7),
				chTimeArg(goldenFilter().From),
				chTimeArg(goldenFilter().To),
				[]string{SevWarn, SevError},
				"api", "prod", "timeout",
				"source", "nginx",
				"host", "web-1",
				"abc123",
				facetLimit,
			},
		},
		{
			col: "environment",
			want: `
		SELECT environment, count() AS c
		FROM logs
		WHERE project_id = ? AND timestamp >= toDateTime64(?, 3) AND timestamp < toDateTime64(?, 3) AND environment != '' AND severity IN (?) AND service = ? AND environment = ? AND positionCaseInsensitiveUTF8(body, ?) > 0 AND log_attributes[?] = ? AND resource_attrs[?] = ? AND trace_id = ?
		GROUP BY environment
		ORDER BY c DESC
		LIMIT ?
		SETTINGS max_execution_time = 5`,
			wantArgs: []any{
				uint64(7),
				chTimeArg(goldenFilter().From),
				chTimeArg(goldenFilter().To),
				[]string{SevWarn, SevError},
				"api", "prod", "timeout",
				"source", "nginx",
				"host", "web-1",
				"abc123",
				facetLimit,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.col, func(t *testing.T) {
			c := &captureConn{}
			q := NewQuery(c)

			if _, err := q.Facet(context.Background(), 7, goldenFilter(), tc.col); !errors.Is(err, errCapture) {
				t.Fatalf("Facet(%s): ожидалась errCapture, получено %v", tc.col, err)
			}

			if c.query != tc.want {
				t.Fatalf("SQL изменился.\nПОЛУЧЕНО:\n%s\nОЖИДАЛОСЬ:\n%s", c.query, tc.want)
			}

			if len(c.args) != len(tc.wantArgs) {
				t.Fatalf("аргументов %d, ожидалось %d: %#v", len(c.args), len(tc.wantArgs), c.args)
			}
			for i := range tc.wantArgs {
				if fmt.Sprintf("%#v", c.args[i]) != fmt.Sprintf("%#v", tc.wantArgs[i]) {
					t.Errorf("arg[%d] = %#v, ожидалось %#v", i, c.args[i], tc.wantArgs[i])
				}
			}
		})
	}
}

func TestGoldenWhereListNegated(t *testing.T) {
	c := &captureConn{}
	q := NewQuery(c)

	f := goldenFilter()
	f.Not = []Predicate{
		{Field: FieldBody, Op: OpNotContains, Value: "buffered to a temporary file"},
		{Field: FieldSeverity, Op: OpNeq, Value: SevDebug},
		{Field: FieldSeverity, Op: OpNeq, Value: SevTrace},
		{Field: FieldService, Op: OpNeq, Value: "cron"},
		{Field: FieldAttr, Key: "source", Op: OpNeq, Value: "nginx"},
	}

	if _, err := q.List(context.Background(), 7, f); !errors.Is(err, errCapture) {
		t.Fatalf("List: ожидалась errCapture, получено %v", err)
	}

	const want = `
		SELECT timestamp, observed_ts, severity, severity_number, severity_text,
			body, trace_id, span_id, log_attributes, resource_attrs, service, environment
		FROM logs
		WHERE project_id = ? AND timestamp >= toDateTime64(?, 3) AND timestamp < toDateTime64(?, 3) AND severity IN (?) AND service = ? AND environment = ? AND positionCaseInsensitiveUTF8(body, ?) > 0 AND log_attributes[?] = ? AND resource_attrs[?] = ? AND trace_id = ? AND severity NOT IN (?) AND positionCaseInsensitiveUTF8(body, ?) = 0 AND service != ? AND NOT (log_attributes[?] = ?)
		ORDER BY timestamp DESC,
			cityHash64(observed_ts, severity_number, severity_text, body, trace_id, span_id,
				toString(log_attributes), toString(resource_attrs), service, environment) DESC
		LIMIT ?
		SETTINGS max_execution_time = 20`
	if c.query != want {
		t.Fatalf("SQL изменился.\nПОЛУЧЕНО:\n%s\nОЖИДАЛОСЬ:\n%s", c.query, want)
	}

	// Порядок отрицаний детерминирован: сначала собранное severity NOT IN,
	// затем остальные по порядку следования f.Not.
	wantArgs := []any{
		uint64(7),
		chTimeArg(f.From),
		chTimeArg(f.To),
		[]string{SevWarn, SevError},
		"api", "prod", "timeout",
		"source", "nginx",
		"host", "web-1",
		"abc123",
		[]string{SevDebug, SevTrace},
		"buffered to a temporary file",
		"cron",
		"source", "nginx",
		50,
	}
	if len(c.args) != len(wantArgs) {
		t.Fatalf("аргументов %d, ожидалось %d: %#v", len(c.args), len(wantArgs), c.args)
	}
	for i := range wantArgs {
		if fmt.Sprintf("%#v", c.args[i]) != fmt.Sprintf("%#v", wantArgs[i]) {
			t.Errorf("arg[%d] = %#v, ожидалось %#v", i, c.args[i], wantArgs[i])
		}
	}
}

func TestGoldenWhereAttrKeys(t *testing.T) {
	c := &captureConn{}
	q := NewQuery(c)

	if _, err := q.AttrKeys(context.Background(), 7, goldenFilter(), "src", 10); !errors.Is(err, errCapture) {
		t.Fatalf("AttrKeys: ожидалась errCapture, получено %v", err)
	}

	const want = `
		SELECT key, count() AS c
		FROM (
			SELECT log_attributes
			FROM logs
			WHERE project_id = ? AND timestamp >= toDateTime64(?, 3) AND timestamp < toDateTime64(?, 3) AND trace_id = ?
			ORDER BY timestamp DESC
			LIMIT ?
		)
		ARRAY JOIN mapKeys(log_attributes) AS key WHERE key LIKE concat(?, '%')
		GROUP BY key
		ORDER BY c DESC
		LIMIT ?
		SETTINGS max_execution_time = 5`
	if c.query != want {
		t.Fatalf("SQL изменился.\nПОЛУЧЕНО:\n%s\nОЖИДАЛОСЬ:\n%s", c.query, want)
	}

	// AttrKeys чтит из f только TraceID (см. докблок): прочие фильтры
	// (Severity/Service/Environment/Query/Attrs) в args не попадают.
	wantArgs := []any{
		uint64(7),
		chTimeArg(goldenFilter().From),
		chTimeArg(goldenFilter().To),
		"abc123",
		attrKeysScanLimit,
		"src",
		10,
	}
	if len(c.args) != len(wantArgs) {
		t.Fatalf("аргументов %d, ожидалось %d: %#v", len(c.args), len(wantArgs), c.args)
	}
	for i := range wantArgs {
		if fmt.Sprintf("%#v", c.args[i]) != fmt.Sprintf("%#v", wantArgs[i]) {
			t.Errorf("arg[%d] = %#v, ожидалось %#v", i, c.args[i], wantArgs[i])
		}
	}
}

func TestGoldenWhereAttrValues(t *testing.T) {
	c := &captureConn{}
	q := NewQuery(c)

	if _, err := q.AttrValues(context.Background(), 7, goldenFilter(), false, "source", 10); !errors.Is(err, errCapture) {
		t.Fatalf("AttrValues: ожидалась errCapture, получено %v", err)
	}

	const want = `
		SELECT log_attributes[?] AS v, count() AS c
		FROM logs
		WHERE project_id = ? AND timestamp >= toDateTime64(?, 3) AND timestamp < toDateTime64(?, 3) AND severity IN (?) AND service = ? AND environment = ? AND positionCaseInsensitiveUTF8(body, ?) > 0 AND log_attributes[?] = ? AND resource_attrs[?] = ? AND trace_id = ? AND mapContains(log_attributes, ?)
		GROUP BY v
		ORDER BY c DESC
		LIMIT ?
		SETTINGS max_execution_time = 5`
	if c.query != want {
		t.Fatalf("SQL изменился.\nПОЛУЧЕНО:\n%s\nОЖИДАЛОСЬ:\n%s", c.query, want)
	}

	// Порядок: key (для SELECT col[?]), затем весь where (как у List), затем
	// key ещё раз (для mapContains), затем limit.
	wantArgs := []any{
		"source",
		uint64(7),
		chTimeArg(goldenFilter().From),
		chTimeArg(goldenFilter().To),
		[]string{SevWarn, SevError},
		"api", "prod", "timeout",
		"source", "nginx",
		"host", "web-1",
		"abc123",
		"source",
		10,
	}
	if len(c.args) != len(wantArgs) {
		t.Fatalf("аргументов %d, ожидалось %d: %#v", len(c.args), len(wantArgs), c.args)
	}
	for i := range wantArgs {
		if fmt.Sprintf("%#v", c.args[i]) != fmt.Sprintf("%#v", wantArgs[i]) {
			t.Errorf("arg[%d] = %#v, ожидалось %#v", i, c.args[i], wantArgs[i])
		}
	}
}

// TestGoldenFacetOmitsOwnNegation: фасет по колонке не должен применять
// собственное отрицание — иначе исключённое значение пропадёт из счётчиков
// вместе с возможностью снять исключение обратным кликом. Отрицание по
// ДРУГОМУ полю при этом обязано остаться. Прогоняется по ВСЕМ трём колонкам
// facetColumns (не только service): омит-ключ обязан браться из параметра
// col, а не совпадать с ним случайно на одной проверенной колонке — иначе
// хардкод вида `map[string]bool{"service": true}` вместо `{col: true}`
// прошёл бы незамеченным на service и молча тёк для environment/severity.
func TestGoldenFacetOmitsOwnNegation(t *testing.T) {
	cases := []struct {
		col       string
		ownNot    Predicate
		ownWant   string // подстрока собственного отрицания — обязана отсутствовать
		otherNot  Predicate
		otherWant string // подстрока чужого отрицания — обязана присутствовать
	}{
		{
			col:       FieldService,
			ownNot:    Predicate{Field: FieldService, Op: OpNeq, Value: "cron"},
			ownWant:   "service != ?",
			otherNot:  Predicate{Field: FieldEnvironment, Op: OpNeq, Value: "staging"},
			otherWant: "environment != ?",
		},
		{
			col:       FieldEnvironment,
			ownNot:    Predicate{Field: FieldEnvironment, Op: OpNeq, Value: "staging"},
			ownWant:   "environment != ?",
			otherNot:  Predicate{Field: FieldService, Op: OpNeq, Value: "cron"},
			otherWant: "service != ?",
		},
		{
			col:       FieldSeverity,
			ownNot:    Predicate{Field: FieldSeverity, Op: OpNeq, Value: SevDebug},
			ownWant:   "severity NOT IN (?)",
			otherNot:  Predicate{Field: FieldService, Op: OpNeq, Value: "cron"},
			otherWant: "service != ?",
		},
	}

	for _, tc := range cases {
		t.Run(tc.col, func(t *testing.T) {
			f := goldenFilter()
			f.Not = []Predicate{tc.ownNot, tc.otherNot}

			c := &captureConn{}
			q := NewQuery(c)
			if _, err := q.Facet(context.Background(), 7, f, tc.col); !errors.Is(err, errCapture) {
				t.Fatalf("Facet(%s): ожидалась errCapture, получено %v", tc.col, err)
			}

			if strings.Contains(c.query, tc.ownWant) {
				t.Fatalf("фасет по %s применил собственное отрицание:\n%s", tc.col, c.query)
			}
			if !strings.Contains(c.query, tc.otherWant) {
				t.Fatalf("фасет по %s обязан применять отрицание по другому полю:\n%s", tc.col, c.query)
			}
		})
	}
}

// TestGoldenAttrValuesOmitsOwnNegation: раскрытый ключ атрибута не
// применяет своё отрицание, но отрицание по чужому ключу продолжает
// сужать выборку. Два случая — обычный атрибут и ресурсный, с РАЗНЫМИ
// ключами (не "source" в обоих): омит-ключ обязан собираться из
// resource+key параметров вызова, а не совпадать с одним захардкоженным
// значением случайно на единственной проверенной комбинации.
func TestGoldenAttrValuesOmitsOwnNegation(t *testing.T) {
	cases := []struct {
		name       string
		resource   bool
		key        string
		ownNot     Predicate
		otherNot   Predicate
		ownField   string // NOT(<col>[?] = ?) — колонка собственного отрицания
		otherValue string // значение чужого отрицания — обязано остаться в args
		ownValue   string // значение собственного отрицания — обязано пропасть из args
	}{
		{
			name:       "attr",
			resource:   false,
			key:        "source",
			ownNot:     Predicate{Field: FieldAttr, Key: "source", Op: OpNeq, Value: "nginx"},
			otherNot:   Predicate{Field: FieldAttr, Key: "env", Op: OpNeq, Value: "dev"},
			ownField:   "log_attributes",
			otherValue: "dev",
			ownValue:   "nginx",
		},
		{
			name:       "resource_attr",
			resource:   true,
			key:        "host",
			ownNot:     Predicate{Field: FieldResourceAttr, Key: "host", Op: OpNeq, Value: "web-2"},
			otherNot:   Predicate{Field: FieldResourceAttr, Key: "az", Op: OpNeq, Value: "us-east"},
			ownField:   "resource_attrs",
			otherValue: "us-east",
			ownValue:   "web-2",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := goldenFilter()
			f.Attrs = nil
			f.Not = []Predicate{tc.ownNot, tc.otherNot}

			c := &captureConn{}
			q := NewQuery(c)
			if _, err := q.AttrValues(context.Background(), 7, f, tc.resource, tc.key, 10); !errors.Is(err, errCapture) {
				t.Fatalf("AttrValues: ожидалась errCapture, получено %v", err)
			}

			wantClause := "NOT (" + tc.ownField + "[?] = ?)"
			if got := strings.Count(c.query, wantClause); got != 1 {
				t.Fatalf("ожидалось ровно одно отрицание по чужому ключу, найдено %d:\n%s", got, c.query)
			}
			// Аргументы докажут, что осталось именно чужое значение, а не
			// собственное. Сам ключ раскрытого атрибута в args есть всегда
			// (параметр SELECT-проекции и mapContains), поэтому различает
			// только значение отрицания, а не ключ.
			if !containsArg(c.args, tc.otherValue) || containsArg(c.args, tc.ownValue) {
				t.Fatalf("отрицание по раскрытому ключу не отброшено: %#v", c.args)
			}
		})
	}
}
