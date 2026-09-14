package profile

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

type Query struct {
	conn driver.Conn
}

func NewQuery(conn driver.Conn) *Query { return &Query{conn: conn} }

type ServiceInfo struct {
	Service      string
	Type         string
	Transaction  string
	Weight       uint64
	Unit         string
	Samples      uint64
	Environments []string
}

func (q *Query) ListServices(ctx context.Context, projectID int64, environment string, from, to time.Time) ([]ServiceInfo, error) {
	rows, err := q.conn.Query(ctx, `
		SELECT service, profile_type, transaction,
			sum(value) AS weight,
			-- Единица одна на группу (профиль одного типа), поэтому берём
			-- любую непустую: max() пропускает пустые строки старых записей.
			max(unit) AS unit,
			count() AS samples,
			arraySort(groupUniqArray(environment)) AS envs
		FROM profile_samples
		WHERE project_id = ? AND ts >= ? AND ts < ? AND (? = '' OR environment = ?)
		GROUP BY service, profile_type, transaction
		ORDER BY weight DESC
		LIMIT 200`,
		projectID, from, to, environment, environment)
	if err != nil {
		return nil, fmt.Errorf("profile: list services: %w", err)
	}
	defer rows.Close()
	var out []ServiceInfo
	for rows.Next() {
		var s ServiceInfo
		if err := rows.Scan(&s.Service, &s.Type, &s.Transaction, &s.Weight, &s.Unit, &s.Samples, &s.Environments); err != nil {
			return nil, fmt.Errorf("profile: list services scan: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

type FlameNode struct {
	Name     string
	Value    uint64
	Children []*FlameNode
}

// Запас в разы над типичными 5-20 тыс. уникальных стеков нагруженного сервиса;
// усечение при превышении режет по убыванию веса — самые лёгкие стеки первыми.
const maxFlameStacks = 50_000

func (q *Query) Flame(ctx context.Context, projectID int64, service, environment, profileType, transaction string, from, to time.Time) (*FlameNode, error) {
	rows, err := q.conn.Query(ctx, `
		SELECT stack, sum(value) AS total
		FROM profile_samples
		WHERE project_id = ? AND profile_type = ? AND service = ?
		  AND (? = '' OR environment = ?) AND (? = '' OR transaction = ?)
		  AND ts >= ? AND ts < ?
		GROUP BY stack
		ORDER BY total DESC
		LIMIT ?
		SETTINGS max_execution_time = 10`,
		projectID, profileType, service, environment, environment, transaction, transaction, from, to, maxFlameStacks)
	if err != nil {
		return nil, fmt.Errorf("profile: flame: %w", err)
	}
	defer rows.Close()
	return buildFlame(rows)
}

func (q *Query) HasProfileForTrace(ctx context.Context, projectID int64, traceID string) (bool, error) {
	if traceID == "" {
		return false, nil
	}
	var one uint8
	// LIMIT 1 вместо count(): нужен только факт наличия, count() читает все гранулы.
	err := q.conn.QueryRow(ctx,
		"SELECT 1 FROM profile_samples WHERE project_id = ? AND trace_id = ? LIMIT 1",
		projectID, traceID).Scan(&one)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("profile: has profile for trace: %w", err)
	}
	return true, nil
}

func (q *Query) FlameForTrace(ctx context.Context, projectID int64, traceID string) (*FlameNode, error) {
	rows, err := q.conn.Query(ctx, `
		SELECT stack, sum(value) AS total
		FROM profile_samples
		WHERE project_id = ? AND trace_id = ?
		GROUP BY stack
		ORDER BY total DESC
		LIMIT ?
		SETTINGS max_execution_time = 10`,
		projectID, traceID, maxFlameStacks)
	if err != nil {
		return nil, fmt.Errorf("profile: flame for trace: %w", err)
	}
	defer rows.Close()
	return buildFlame(rows)
}

func buildFlame(rows driver.Rows) (*FlameNode, error) {
	root := &FlameNode{Name: "all"}
	// Индекс детей по имени: линейный перебор Children делает сборку широкого
	// узла квадратичной по числу его детей.
	index := map[*FlameNode]map[string]*FlameNode{}
	child := func(n *FlameNode, name string) *FlameNode {
		kids := index[n]
		if c, ok := kids[name]; ok {
			return c
		}
		if kids == nil {
			kids = map[string]*FlameNode{}
			index[n] = kids
		}
		c := &FlameNode{Name: name}
		n.Children = append(n.Children, c)
		kids[name] = c
		return c
	}
	for rows.Next() {
		var stack []string
		var total uint64
		if err := rows.Scan(&stack, &total); err != nil {
			return nil, fmt.Errorf("profile: flame scan: %w", err)
		}
		root.Value += total
		node := root
		for _, name := range stack {
			node = child(node, name)
			node.Value += total
		}
	}
	return root, rows.Err()
}

type ServiceType struct {
	Service string
	Type    string
}

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

type ProjectService struct {
	ProjectID int64
	Service   string
	Type      string
}

func (q *Query) ActiveServices(ctx context.Context, from, to time.Time) ([]ProjectService, error) {
	rows, err := q.conn.Query(ctx, `
		SELECT DISTINCT project_id, service, profile_type FROM profile_samples
		WHERE ts >= ? AND ts < ?`, from, to)
	if err != nil {
		return nil, fmt.Errorf("profile: active services: %w", err)
	}
	defer rows.Close()
	var out []ProjectService
	for rows.Next() {
		var ps ProjectService
		var projectID uint64
		if err := rows.Scan(&projectID, &ps.Service, &ps.Type); err != nil {
			return nil, fmt.Errorf("profile: active services scan: %w", err)
		}
		ps.ProjectID = int64(projectID)
		out = append(out, ps)
	}
	return out, rows.Err()
}

type FunctionShare struct {
	Function string
	Share    float64
	// Число строк САМОЙ этой функции в recent-окне (не всего окна и не сумма value).
	Samples uint64
}

func (q *Query) TopFunctionShares(ctx context.Context, projectID int64, service, profileType string, from, to time.Time, k int) ([]FunctionShare, error) {
	rows, err := q.conn.Query(ctx, `
		SELECT fn, self, total, samples FROM (
			SELECT arrayElement(stack, -1) AS fn,
			       sum(value) AS self,
			       sum(sum(value)) OVER () AS total,
			       count() AS samples
			FROM profile_samples
			WHERE project_id = ? AND service = ? AND profile_type = ? AND ts >= ? AND ts < ?
			GROUP BY fn
		)
		WHERE fn != '' AND total > 0
		ORDER BY self DESC
		LIMIT ?`,
		projectID, service, profileType, from, to, k)
	if err != nil {
		return nil, fmt.Errorf("profile: top function shares: %w", err)
	}
	defer rows.Close()
	var out []FunctionShare
	for rows.Next() {
		var fn string
		var self, total, samples uint64
		if err := rows.Scan(&fn, &self, &total, &samples); err != nil {
			return nil, fmt.Errorf("profile: top function shares scan: %w", err)
		}
		if total == 0 {
			continue
		}
		out = append(out, FunctionShare{Function: fn, Share: float64(self) / float64(total), Samples: samples})
	}
	return out, rows.Err()
}

// Тот же total, что в TopFunctionShares, но по конкретным именам, без ORDER BY/LIMIT.
// Функция без единой строки в окне просто отсутствует в результате.
func (q *Query) FunctionSharesFor(ctx context.Context, projectID int64, service, profileType string, functions []string, from, to time.Time) (map[string]FunctionShare, error) {
	out := make(map[string]FunctionShare, len(functions))
	if len(functions) == 0 {
		return out, nil
	}
	rows, err := q.conn.Query(ctx, `
		SELECT fn, self, total, samples FROM (
			SELECT arrayElement(stack, -1) AS fn,
			       sum(value) AS self,
			       sum(sum(value)) OVER () AS total,
			       count() AS samples
			FROM profile_samples
			WHERE project_id = ? AND service = ? AND profile_type = ? AND ts >= ? AND ts < ?
			GROUP BY fn
		)
		WHERE fn IN ? AND total > 0`,
		projectID, service, profileType, from, to, functions)
	if err != nil {
		return nil, fmt.Errorf("profile: function shares for: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var fn string
		var self, total, samples uint64
		if err := rows.Scan(&fn, &self, &total, &samples); err != nil {
			return nil, fmt.Errorf("profile: function shares for scan: %w", err)
		}
		out[fn] = FunctionShare{Function: fn, Share: float64(self) / float64(total), Samples: samples}
	}
	return out, rows.Err()
}

type BaselineShare struct {
	Share float64
	// Число строк этой функции, СУММА по всем дням базового окна (не строк одного дня).
	Samples uint64
}

func (q *Query) BaselineFunctionShares(ctx context.Context, projectID int64, service, profileType string, functions []string, baselineDays int, now time.Time) (map[string]BaselineShare, error) {
	out := make(map[string]BaselineShare, len(functions))
	if len(functions) == 0 {
		return out, nil
	}
	from := now.AddDate(0, 0, -baselineDays)
	rows, err := q.conn.Query(ctx, `
		SELECT fn, quantileExact(0.5)(share) AS median, sum(cnt) AS samples FROM (
			SELECT d, fn, self, cnt, self / day_total AS share FROM (
				SELECT toDate(ts) AS d,
				       arrayElement(stack, -1) AS fn,
				       sum(value) AS self,
				       count() AS cnt,
				       sum(sum(value)) OVER (PARTITION BY toDate(ts)) AS day_total
				FROM profile_samples
				WHERE project_id = ? AND service = ? AND profile_type = ? AND ts >= ? AND ts < ?
				GROUP BY d, fn
			)
			WHERE day_total > 0
		)
		WHERE fn IN ?
		GROUP BY fn`,
		projectID, service, profileType, from, now, functions)
	if err != nil {
		return nil, fmt.Errorf("profile: baseline function shares: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var fn string
		var b BaselineShare
		if err := rows.Scan(&fn, &b.Share, &b.Samples); err != nil {
			return nil, fmt.Errorf("profile: baseline function shares scan: %w", err)
		}
		out[fn] = b
	}
	return out, rows.Err()
}

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
