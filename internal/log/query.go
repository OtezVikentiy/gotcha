package log

import (
	"context"
	"fmt"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// Query — чтение логов из ClickHouse. По образцу internal/trace/query.go:
// параметризованные запросы (значения только через ?, никогда не
// конкатенируются в текст), WHERE собирается конкатенацией строк-условий, а
// подставляемые значения идут отдельным срезом args в том же порядке.
type Query struct {
	conn driver.Conn
}

func NewQuery(conn driver.Conn) *Query {
	return &Query{conn: conn}
}

// LogRow — одна строка результата List. ProjectID не хранится: он известен
// из пути вызова (List принимает его отдельным параметром).
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

// AttrFilter — фильтр по одному атрибуту записи: Resource=true проверяет
// resource_attrs, иначе — log_attributes (см. схему таблицы logs).
type AttrFilter struct {
	Resource bool
	Key      string
	Value    string
}

// ListFilter — параметры List.
type ListFilter struct {
	From, To time.Time

	Severity    []string
	Service     string
	Environment string
	Query       string // подстрока body, регистронезависимо
	Attrs       []AttrFilter

	Limit int

	// Before/TieSkip — курсор пагинации keyset, см. List. TieSkip — сколько
	// строк с timestamp == Before ВСЕГО уже показано вызывающему. Если
	// хвостовой Before НЕ меняется между вызовами (тай тянется больше одной
	// страницы), TieSkip накапливается вызывающим (прибавляется), а не
	// пересчитывается заново по последней странице — иначе следующая
	// страница переспросит уже показанные строки этой тай-группы и вернёт
	// дубль. Референс правильной логики накопления — тестовый цикл
	// постраничного обхода в query_test.go.
	Before  time.Time
	TieSkip int
}

// FacetValue — значение фасета с числом вхождений. Тип объявлен здесь для
// будущего метода Facet (следующая задача); List его не использует.
type FacetValue struct {
	Value string
	Count int64
}

// defaultListLimit — сколько строк отдаёт List, если Limit не задан.
// maxListLimit — верхняя граница вне зависимости от запрошенного Limit:
// защита от случайно огромного значения из внешнего слоя (парсер запроса
// тоже клампит, но List не должен полагаться только на вызывающего).
const (
	defaultListLimit = 100
	maxListLimit     = 500
)

// List возвращает страницу логов проекта за [From, To), отфильтрованных по f,
// отсортированных newest-first, не более Limit штук (после клампа).
//
// Курсор пагинации (Before, TieSkip): timestamp в logs — DateTime64(3), на
// высоком rps у нескольких строк одна и та же миллисекунда. Строгое условие
// "timestamp < Before" на границе страницы теряет строки с тем же
// timestamp, что и последняя показанная (они физически могут идти ПОСЛЕ неё
// в результате следующего запроса и не соответствовать "<"); "timestamp <=
// Before" без поправки, наоборот, дублирует уже показанные. Вместо этого:
// условие "timestamp <= Before", лимит запроса увеличен на TieSkip
// (сколько строк с timestamp == Before уже показано предыдущей страницей),
// а после скана в Go среди строк с Timestamp == Before пропускаются первые
// TieSkip штук — это они и есть. Затем результат обрезается до Limit.
func (q *Query) List(ctx context.Context, projectID int64, f ListFilter) ([]LogRow, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = defaultListLimit
	}
	if limit > maxListLimit {
		limit = maxListLimit
	}

	where := "project_id = ? AND timestamp >= ? AND timestamp < ?"
	args := []any{uint64(projectID), f.From, f.To}

	if len(f.Severity) > 0 {
		where += " AND severity IN (?)"
		args = append(args, f.Severity)
	}
	if f.Service != "" {
		where += " AND service = ?"
		args = append(args, f.Service)
	}
	if f.Environment != "" {
		where += " AND environment = ?"
		args = append(args, f.Environment)
	}
	if f.Query != "" {
		where += " AND positionCaseInsensitiveUTF8(body, ?) > 0"
		args = append(args, f.Query)
	}
	for _, a := range f.Attrs {
		col := "log_attributes"
		if a.Resource {
			col = "resource_attrs"
		}
		where += " AND " + col + "[?] = ?"
		args = append(args, a.Key, a.Value)
	}

	queryLimit := limit
	if !f.Before.IsZero() {
		where += " AND timestamp <= ?"
		args = append(args, f.Before)
		queryLimit = limit + f.TieSkip
	}
	args = append(args, queryLimit)

	// Второй ключ сортировки обязателен для устойчивости курсора: "ORDER BY
	// timestamp DESC" без него не гарантирует ОДИНАКОВЫЙ относительный порядок
	// строк с равным timestamp между двумя разными запросами (страница 1 без
	// "AND timestamp <= ?" и страница 2 с ним — разные планы выполнения, CH
	// вправе перемешать тай по-своему). Без второго ключа TieSkip пропускает
	// не те строки и дублирует/теряет их на границе. cityHash64 — чистая
	// функция от значений самой строки, поэтому детерминирована между любыми
	// запросами по неизменным данным (строка в таблице не меняется между
	// вызовами); хэшируем ВСЕ 12 колонок результата, включая log_attributes/
	// resource_attrs через toString(Map) — иначе две строки, различающиеся
	// только атрибутами, схлопывались бы в один и тот же хэш. Коллизия
	// (а с ней риск дубля/потери одной строки на границе страницы) остаётся
	// только для строк, идентичных БУКВАЛЬНО по всем 12 колонкам в одну и ту
	// же миллисекунду — у logs нет уникального id, и такие строки неотличимы
	// друг от друга по содержимому, так что дубль/потеря одной из них не
	// заметны: контент на экране тот же.
	rows, err := q.conn.Query(ctx, `
		SELECT timestamp, observed_ts, severity, severity_number, severity_text,
			body, trace_id, span_id, log_attributes, resource_attrs, service, environment
		FROM logs
		WHERE `+where+`
		ORDER BY timestamp DESC,
			cityHash64(observed_ts, severity_number, severity_text, body, trace_id, span_id,
				toString(log_attributes), toString(resource_attrs), service, environment) DESC
		LIMIT ?`, args...)
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
