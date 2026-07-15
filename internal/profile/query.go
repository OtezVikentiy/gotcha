package profile

import (
	"context"
	"fmt"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// Query читает агрегаты профилей из profile_samples (аналог metric.Query).
type Query struct {
	conn driver.Conn
}

func NewQuery(conn driver.Conn) *Query { return &Query{conn: conn} }

// ServiceInfo — группа профилей (сервис/тип/транзакция) с суммарным весом.
type ServiceInfo struct {
	Service     string
	Type        string
	Transaction string
	Samples     uint64
}

// ListServices возвращает группы профилей проекта за период (для обзора/фильтров).
func (q *Query) ListServices(ctx context.Context, projectID int64, environment string, from, to time.Time) ([]ServiceInfo, error) {
	rows, err := q.conn.Query(ctx, `
		SELECT service, profile_type, transaction, sum(value)
		FROM profile_samples
		WHERE project_id = ? AND ts >= ? AND ts < ? AND (? = '' OR environment = ?)
		GROUP BY service, profile_type, transaction
		ORDER BY sum(value) DESC
		LIMIT 200`,
		projectID, from, to, environment, environment)
	if err != nil {
		return nil, fmt.Errorf("profile: list services: %w", err)
	}
	defer rows.Close()
	var out []ServiceInfo
	for rows.Next() {
		var s ServiceInfo
		if err := rows.Scan(&s.Service, &s.Type, &s.Transaction, &s.Samples); err != nil {
			return nil, fmt.Errorf("profile: list services scan: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// FlameNode — узел flamegraph-дерева.
type FlameNode struct {
	Name     string
	Value    uint64
	Children []*FlameNode
}

// Flame агрегирует стеки за период + фильтры и строит flamegraph-дерево. Корень
// синтетический («all») с суммарным value; каждый стек прибавляется проходом
// корень→лист.
func (q *Query) Flame(ctx context.Context, projectID int64, service, environment, profileType, transaction string, from, to time.Time) (*FlameNode, error) {
	rows, err := q.conn.Query(ctx, `
		SELECT stack, sum(value)
		FROM profile_samples
		WHERE project_id = ? AND profile_type = ? AND service = ?
		  AND (? = '' OR environment = ?) AND (? = '' OR transaction = ?)
		  AND ts >= ? AND ts < ?
		GROUP BY stack`,
		projectID, profileType, service, environment, environment, transaction, transaction, from, to)
	if err != nil {
		return nil, fmt.Errorf("profile: flame: %w", err)
	}
	defer rows.Close()
	return buildFlame(rows)
}

// HasProfileForTrace сообщает, есть ли профиль, привязанный к trace_id
// (profiling-in-context, этап 8). Пустой traceID → false без запроса.
func (q *Query) HasProfileForTrace(ctx context.Context, projectID int64, traceID string) (bool, error) {
	if traceID == "" {
		return false, nil
	}
	var n uint64
	err := q.conn.QueryRow(ctx,
		"SELECT count() FROM profile_samples WHERE project_id = ? AND trace_id = ?",
		projectID, traceID).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("profile: has profile for trace: %w", err)
	}
	return n > 0, nil
}

// FlameForTrace строит flamegraph по всем профилям, привязанным к trace_id
// (без окна/сервиса/типа — trace_id сам ограничивает выборку).
func (q *Query) FlameForTrace(ctx context.Context, projectID int64, traceID string) (*FlameNode, error) {
	rows, err := q.conn.Query(ctx, `
		SELECT stack, sum(value)
		FROM profile_samples
		WHERE project_id = ? AND trace_id = ?
		GROUP BY stack`,
		projectID, traceID)
	if err != nil {
		return nil, fmt.Errorf("profile: flame for trace: %w", err)
	}
	defer rows.Close()
	return buildFlame(rows)
}

// buildFlame собирает дерево из строк (stack Array(String), sum(value)). Корень
// синтетический («all»); каждый стек прибавляется проходом корень→лист.
func buildFlame(rows driver.Rows) (*FlameNode, error) {
	root := &FlameNode{Name: "all"}
	for rows.Next() {
		var stack []string
		var total uint64
		if err := rows.Scan(&stack, &total); err != nil {
			return nil, fmt.Errorf("profile: flame scan: %w", err)
		}
		root.Value += total
		node := root
		for _, name := range stack {
			node = node.child(name)
			node.Value += total
		}
	}
	return root, rows.Err()
}

// child находит/создаёт ребёнка по имени кадра.
func (n *FlameNode) child(name string) *FlameNode {
	for _, c := range n.Children {
		if c.Name == name {
			return c
		}
	}
	c := &FlameNode{Name: name}
	n.Children = append(n.Children, c)
	return c
}
