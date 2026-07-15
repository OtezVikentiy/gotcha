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

// ServiceType — пара (сервис, тип профиля) с данными (для оценщика регрессий).
type ServiceType struct {
	Service string
	Type    string
}

// ServicesWithProfiles — пары (service, profile_type) с профилями за окно.
func (q *Query) ServicesWithProfiles(ctx context.Context, projectID int64, from, to time.Time) ([]ServiceType, error) {
	rows, err := q.conn.Query(ctx, `
		SELECT DISTINCT service, profile_type FROM profile_samples
		WHERE project_id = ? AND ts >= ? AND ts < ?`,
		projectID, from, to)
	if err != nil {
		return nil, fmt.Errorf("profile: services with profiles: %w", err)
	}
	defer rows.Close()
	var out []ServiceType
	for rows.Next() {
		var st ServiceType
		if err := rows.Scan(&st.Service, &st.Type); err != nil {
			return nil, fmt.Errorf("profile: services with profiles scan: %w", err)
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// TopFunctionsBySelfShare — топ-K функций по свежему self-CPU (лист стека) за
// окно; кандидаты на проверку регрессии.
func (q *Query) TopFunctionsBySelfShare(ctx context.Context, projectID int64, service, profileType string, from, to time.Time, k int) ([]string, error) {
	rows, err := q.conn.Query(ctx, `
		SELECT arrayElement(stack, -1) AS fn, sum(value) AS self
		FROM profile_samples
		WHERE project_id = ? AND service = ? AND profile_type = ? AND ts >= ? AND ts < ? AND length(stack) > 0
		GROUP BY fn ORDER BY self DESC LIMIT ?`,
		projectID, service, profileType, from, to, k)
	if err != nil {
		return nil, fmt.Errorf("profile: top functions: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var fn string
		var self uint64
		if err := rows.Scan(&fn, &self); err != nil {
			return nil, fmt.Errorf("profile: top functions scan: %w", err)
		}
		out = append(out, fn)
	}
	return out, rows.Err()
}

// RecentFunctionShare — self-доля функции за окно (self-CPU / total) и число
// сэмплов (total) окна для проверки MinSamples. total==0 → share 0.
func (q *Query) RecentFunctionShare(ctx context.Context, projectID int64, service, profileType, function string, from, to time.Time) (float64, uint64, error) {
	var self, total uint64
	err := q.conn.QueryRow(ctx, `
		SELECT sumIf(value, arrayElement(stack, -1) = ?), sum(value)
		FROM profile_samples
		WHERE project_id = ? AND service = ? AND profile_type = ? AND ts >= ? AND ts < ?`,
		function, projectID, service, profileType, from, to).Scan(&self, &total)
	if err != nil {
		return 0, 0, fmt.Errorf("profile: recent function share: %w", err)
	}
	if total == 0 {
		return 0, 0, nil
	}
	return float64(self) / float64(total), total, nil
}

// BaselineFunctionShare — медиана дневной self-доли функции за baselineDays
// дней (скользящая база). Нет строк → 0.
func (q *Query) BaselineFunctionShare(ctx context.Context, projectID int64, service, profileType, function string, baselineDays int, now time.Time) (float64, error) {
	from := now.AddDate(0, 0, -baselineDays)
	var median float64
	err := q.conn.QueryRow(ctx, `
		SELECT quantileExact(0.5)(daily) FROM (
			SELECT toDate(ts) d, sumIf(value, arrayElement(stack, -1) = ?) / sum(value) AS daily
			FROM profile_samples
			WHERE project_id = ? AND service = ? AND profile_type = ? AND ts >= ? AND ts < ?
			GROUP BY d)`,
		function, projectID, service, profileType, from, now).Scan(&median)
	if err != nil {
		return 0, fmt.Errorf("profile: baseline function share: %w", err)
	}
	return median, nil
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
