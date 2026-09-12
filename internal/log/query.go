package log

import (
	"context"
	"fmt"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// Параметризованные запросы — значения только через ?, никогда конкатенацией; WHERE собирается строкой,
// подставляемые значения идут отдельным срезом args в том же порядке.
type Query struct {
	conn driver.Conn
}

func NewQuery(conn driver.Conn) *Query {
	return &Query{conn: conn}
}

// ProjectID не хранится — он известен из пути вызова (List принимает его отдельным параметром).
type LogRow struct {
	Timestamp      time.Time
	ObservedTS     time.Time
	Severity       string
	SeverityNumber uint8
	SeverityText   string
	Body           string
	TraceID        string
	SpanID         string
	LogAttributes  map[string]string
	ResourceAttrs  map[string]string
	Service        string
	Environment    string
}

// Resource=true проверяет resource_attrs, иначе — log_attributes (см. схему таблицы logs).
type AttrFilter struct {
	Resource bool
	Key      string
	Value    string
}

type ListFilter struct {
	From, To time.Time

	Severity    []string
	Service     string
	Environment string
	Query       string // подстрока body, регистронезависимо
	Attrs       []AttrFilter

	// Отрицательные условия (см. predicate.go) — срезом, потому что их произвольное количество по любому полю.
	Not []Predicate

	// Жёсткий скоуп по trace_id: при непустом добавляет "AND trace_id = ?" во ВСЕ запросы, включая подзапрос AttrKeys —
	// единственный фильтр из f, который она применяет (автокомплит ключей обязан быть по трейсу).
	TraceID string

	Limit int

	// Курсор пагинации keyset (см. List). TieSkip — сколько строк с timestamp==Before уже показано ВСЕГО;
	// накапливается вызывающим между страницами с тем же Before, не пересчитывается — иначе будет дубль.
	Before  time.Time
	TieSkip int
}

type FacetValue struct {
	Value string
	Count int64
}

// Сколько строк отдаёт List без явного Limit. maxListLimit — потолок вне зависимости от запрошенного:
// List не должен полагаться только на клампинг вызывающего.
const (
	defaultListLimit = 100
	maxListLimit     = 500
)

// Курсор пагинации — см. поле Before/TieSkip у ListFilter.
func (q *Query) List(ctx context.Context, projectID int64, f ListFilter) ([]LogRow, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = defaultListLimit
	}
	if limit > maxListLimit {
		limit = maxListLimit
	}

	// chTimeArg обходит баг биндинга clickhouse-go — время строкой с миллисекундами (см. её докблок).
	where, args := buildWhere(projectID, f, whereOpts{})

	queryLimit := limit
	if !f.Before.IsZero() {
		// TieSkip приходит из URL и обязан клампиться, как limit: без потолка queryLimit=limit+TieSkip снимает
		// LIMIT фактически, гигантский tskip даёт OOM. Отрицательный tskip обнуляется.
		if f.TieSkip < 0 {
			f.TieSkip = 0
		} else if f.TieSkip > maxListLimit {
			f.TieSkip = maxListLimit
		}
		where += " AND timestamp <= toDateTime64(?, 3)"
		args = append(args, chTimeArg(f.Before))
		queryLimit = limit + f.TieSkip
	}
	args = append(args, queryLimit)

	// cityHash64 по колонкам — второй ключ сортировки, иначе курсор между страницами плывёт.
	// max_execution_time=20: без потолка CH держит соединение до 60с и блокирует другие запросы.
	rows, err := q.conn.Query(ctx, `
		SELECT timestamp, observed_ts, severity, severity_number, severity_text,
			body, trace_id, span_id, log_attributes, resource_attrs, service, environment
		FROM logs
		WHERE `+where+`
		ORDER BY timestamp DESC,
			cityHash64(observed_ts, severity_number, severity_text, body, trace_id, span_id,
				toString(log_attributes), toString(resource_attrs), service, environment) DESC
		LIMIT ?
		SETTINGS max_execution_time = 20`, args...)
	if err != nil {
		return nil, fmt.Errorf("log: list: %w", err)
	}
	defer rows.Close()

	var out []LogRow
	for rows.Next() {
		var r LogRow
		if err := rows.Scan(
			&r.Timestamp, &r.ObservedTS, &r.Severity, &r.SeverityNumber, &r.SeverityText,
			&r.Body, &r.TraceID, &r.SpanID, &r.LogAttributes, &r.ResourceAttrs, &r.Service, &r.Environment,
		); err != nil {
			return nil, fmt.Errorf("log: list: scan: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("log: list: %w", err)
	}

	if !f.Before.IsZero() && f.TieSkip > 0 {
		skip := 0
		for skip < len(out) && skip < f.TieSkip && out[skip].Timestamp.Equal(f.Before) {
			skip++
		}
		out = out[skip:]
	}
	if len(out) > limit {
		out = out[:limit]
	}

	return out, nil
}

// Без курсора/LIMIT — считает по ВСЕМУ окну, не по странице. GROUP BY отдаёт только непустые пары
// (корзина, severity); плоский пивот по всем severity и добивка нулевых корзин делаются здесь, в Go.
func (q *Query) Histogram(ctx context.Context, projectID int64, f ListFilter, buckets int) ([]time.Time, map[string][]int64, error) {
	if buckets <= 0 {
		return nil, nil, fmt.Errorf("log: histogram: buckets must be positive, got %d", buckets)
	}

	stepSec := int64(f.To.Sub(f.From) / time.Duration(buckets) / time.Second)
	if stepSec < 1 {
		stepSec = 1
	}

	// where — 1:1 с List (см. её комментарий про chTimeArg/toDateTime64), но
	// без блока курсора и без LIMIT.
	where, whereArgs := buildWhere(projectID, f, whereOpts{})
	args := append([]any{stepSec}, whereArgs...)

	// max_execution_time=10 — без LIMIT это тяжёлый запрос; logsHistogram при таймауте деградирует (Empty=true).
	rows, err := q.conn.Query(ctx, `
		SELECT toStartOfInterval(timestamp, INTERVAL ? second) AS t, severity, count() AS c
		FROM logs
		WHERE `+where+`
		GROUP BY t, severity
		ORDER BY t
		SETTINGS max_execution_time = 10`, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("log: histogram: %w", err)
	}
	defer rows.Close()

	byBucket := make(map[int64]map[string]int64)
	for rows.Next() {
		var t time.Time
		var sev string
		var c uint64
		if err := rows.Scan(&t, &sev, &c); err != nil {
			return nil, nil, fmt.Errorf("log: histogram: scan: %w", err)
		}
		bucketUnix := t.UTC().Unix()
		if byBucket[bucketUnix] == nil {
			byBucket[bucketUnix] = make(map[string]int64, len(Severities))
		}
		byBucket[bucketUnix][sev] += int64(c)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("log: histogram: %w", err)
	}

	// Сетка по Unix epoch: последняя корзина — та, что СОДЕРЖИТ f.To (не следующая), иначе включит корзину,
	// для которой запрос физически не мог вернуть данных.
	fromUnix := f.From.UTC().Unix()
	toUnix := f.To.UTC().Unix()
	startUnix := (fromUnix / stepSec) * stepSec
	endUnix := ((toUnix - 1) / stepSec) * stepSec
	if endUnix < startUnix {
		endUnix = startUnix
	}

	n := int((endUnix-startUnix)/stepSec) + 1
	times := make([]time.Time, 0, n)
	series := make(map[string][]int64, len(Severities))
	for _, sev := range Severities {
		series[sev] = make([]int64, 0, n)
	}
	for cur := startUnix; cur <= endUnix; cur += stepSec {
		times = append(times, time.Unix(cur, 0).UTC())
		bucket := byBucket[cur] // nil-карта — все severity этой корзины нулевые
		for _, sev := range Severities {
			series[sev] = append(series[sev], bucket[sev])
		}
	}

	return times, series, nil
}

// Whitelist имён колонок для Facet — единственное место, где текст SQL строится из параметра, не из ?.
// col сверяется с картой ДО попадания в текст запроса — подставить произвольную колонку нельзя.
var facetColumns = map[string]bool{
	"severity":    true,
	"service":     true,
	"environment": true,
}

const facetLimit = 10

// exclude-self: для col==severity условие "severity IN (...)" в WHERE не добавляется — иначе счётчик
// уже выбранного уровня (мультивыбор) был бы всегда 0, и клик по нему не сужал бы список.
func (q *Query) Facet(ctx context.Context, projectID int64, f ListFilter, col string) ([]FacetValue, error) {
	if !facetColumns[col] {
		return nil, fmt.Errorf("log: facet: column %q is not in the whitelist", col)
	}

	// where — тот же набор, что у List/Histogram, без курсора/LIMIT, плюс col != '' (пустой атрибут не
	// шумит в топе). OmitNegative по своей колонке — иначе исключённое кликом значение пропадает из фасета.
	opts := whereOpts{
		BaseExtra:    col + " != ''",
		OmitNegative: map[string]bool{col: true},
	}
	if col == FieldSeverity {
		opts.OmitPositive = map[string]bool{FieldSeverity: true}
	}
	where, args := buildWhere(projectID, f, opts)
	args = append(args, facetLimit)

	rows, err := q.conn.Query(ctx, `
		SELECT `+col+`, count() AS c
		FROM logs
		WHERE `+where+`
		GROUP BY `+col+`
		ORDER BY c DESC
		LIMIT ?
		SETTINGS max_execution_time = 5`, args...)
	if err != nil {
		return nil, fmt.Errorf("log: facet: %w", err)
	}
	defer rows.Close()

	var out []FacetValue
	for rows.Next() {
		var v string
		var c uint64
		if err := rows.Scan(&v, &c); err != nil {
			return nil, fmt.Errorf("log: facet: scan: %w", err)
		}
		out = append(out, FacetValue{Value: v, Count: int64(c)})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("log: facet: %w", err)
	}
	return out, nil
}

// Полное окно ARRAY JOIN mapKeys по всем строкам обрывает max_execution_time=5 на целевом трафике —
// ограниченная свежая выборка (50000 строк) даёт представительный набор ключей дешевле.
const attrKeysScanLimit = 50000

// Считается по ОГРАНИЧЕННОЙ выборке (attrKeysScanLimit), не по всему окну — прочие фильтры f игнорируются,
// кроме TraceID: это жёсткий скоуп контекста трейса, а не мягкий фасет.
func (q *Query) AttrKeys(ctx context.Context, projectID int64, f ListFilter, prefix string, limit int) ([]FacetValue, error) {
	if limit <= 0 {
		limit = facetLimit
	}

	subWhere := "project_id = ? AND timestamp >= toDateTime64(?, 3) AND timestamp < toDateTime64(?, 3)"
	args := []any{uint64(projectID), chTimeArg(f.From), chTimeArg(f.To)}
	if f.TraceID != "" {
		// Единственный фильтр f, который AttrKeys чтит: trace_id — жёсткий скоуп
		// (контекст трейса), а не мягкий фасет (см. докблок метода).
		subWhere += " AND trace_id = ?"
		args = append(args, f.TraceID)
	}
	args = append(args, attrKeysScanLimit)

	query := `
		SELECT key, count() AS c
		FROM (
			SELECT log_attributes
			FROM logs
			WHERE ` + subWhere + `
			ORDER BY timestamp DESC
			LIMIT ?
		)
		ARRAY JOIN mapKeys(log_attributes) AS key`
	if prefix != "" {
		query += " WHERE key LIKE concat(?, '%')"
		args = append(args, prefix)
	}
	query += `
		GROUP BY key
		ORDER BY c DESC
		LIMIT ?
		SETTINGS max_execution_time = 5`
	args = append(args, limit)

	rows, err := q.conn.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("log: attr keys: %w", err)
	}
	defer rows.Close()

	var out []FacetValue
	for rows.Next() {
		var v string
		var c uint64
		if err := rows.Scan(&v, &c); err != nil {
			return nil, fmt.Errorf("log: attr keys: scan: %w", err)
		}
		out = append(out, FacetValue{Value: v, Count: int64(c)})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("log: attr keys: %w", err)
	}
	return out, nil
}

// В отличие от AttrKeys считается по ВСЕМУ окну+фильтрам — раз ключ уже выбран (раскрыт в UI), для него
// важна точность, не ограниченная выборка. mapContains, не has(mapKeys(...)) — дешевле, без промежуточного массива.
func (q *Query) AttrValues(ctx context.Context, projectID int64, f ListFilter, resource bool, key string, limit int) ([]FacetValue, error) {
	if limit <= 0 {
		limit = facetLimit
	}
	col := "log_attributes"
	if resource {
		col = "resource_attrs"
	}

	// where — тот же набор, что у List/Facet (включая f.Attrs — другие ключи продолжают сужать выборку).
	// Отрицание по САМОМУ раскрытому ключу пропускается — иначе исключённое значение пропало бы из списка.
	omitKey := FieldAttr
	if resource {
		omitKey = FieldResourceAttr
	}
	where, whereArgs := buildWhere(projectID, f, whereOpts{
		OmitNegative: map[string]bool{omitKey + ":" + key: true},
	})

	// Порядок args обязан идти 1:1 с "?" в запросе: SELECT col[?], where-условия, mapContains(col, ?), LIMIT.
	args := make([]any, 0, len(whereArgs)+3)
	args = append(args, key)
	args = append(args, whereArgs...)
	args = append(args, key)
	args = append(args, limit)

	rows, err := q.conn.Query(ctx, `
		SELECT `+col+`[?] AS v, count() AS c
		FROM logs
		WHERE `+where+` AND mapContains(`+col+`, ?)
		GROUP BY v
		ORDER BY c DESC
		LIMIT ?
		SETTINGS max_execution_time = 5`, args...)
	if err != nil {
		return nil, fmt.Errorf("log: attr values: %w", err)
	}
	defer rows.Close()

	var out []FacetValue
	for rows.Next() {
		var v string
		var c uint64
		if err := rows.Scan(&v, &c); err != nil {
			return nil, fmt.Errorf("log: attr values: scan: %w", err)
		}
		out = append(out, FacetValue{Value: v, Count: int64(c)})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("log: attr values: %w", err)
	}
	return out, nil
}

// Точность до миллисекунды для toDateTime64(?,3): голый time.Time аргументом "?" драйвер биндит только до
// секунды (bindPositional жёстко TimeUnit=Seconds), теряя миллисекунды молча. UTC — соглашение продукта.
func chTimeArg(t time.Time) string {
	return t.UTC().Format("2006-01-02 15:04:05.000")
}
