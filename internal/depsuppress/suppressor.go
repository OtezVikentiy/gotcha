package depsuppress

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type pgxPool interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

const cacheTTL = 5 * time.Second

// project_id обязателен: метки типовые (web/prod/db повторяются между тенантами) —
// без сверки проекта label-ребро подавляло бы одноимённые хосты ЧУЖОГО проекта.
type hostLabels struct {
	projectID int64
	env       string
	role      string
}

type snapshot struct {
	edges        []Edge
	downHosts    map[int64]bool
	downMonitors map[int64]bool
	hostLabels   map[int64]hostLabels
	loadedAt     time.Time
}

type Suppressor struct {
	pool     pgxPool
	cacheTTL time.Duration
	now      func() time.Time

	mu    sync.Mutex
	cache *snapshot
}

func NewSuppressor(pool *pgxpool.Pool) *Suppressor {
	return &Suppressor{
		pool:     pool,
		cacheTTL: cacheTTL,
		now:      time.Now,
	}
}

// kind ∈ {"host","monitor"}.
func (s *Suppressor) HasParent(ctx context.Context, kind string, nodeID int64) (bool, error) {
	snap, err := s.getSnapshot(ctx)
	if err != nil {
		return false, err
	}
	return len(matchingParents(snap, kind, nodeID)) > 0, nil
}

// Цикл упавших родителей не образует «чёрную дыру» подавления: без down-корня
// обход возвращает false обоим узлам цикла — они пейджат, а не молчат разом.
func (s *Suppressor) ParentDown(ctx context.Context, kind string, nodeID int64) (bool, error) {
	snap, err := s.getSnapshot(ctx)
	if err != nil {
		return false, err
	}
	return parentDownFromSnapshot(snap, node{kind: kind, id: nodeID}), nil
}

func downParents(snap *snapshot, start node) []node {
	var out []node
	for _, e := range matchingParents(snap, start.kind, start.id) {
		if !parentIsDown(e, snap) {
			continue
		}
		if p := parentNode(e); p != nil {
			out = append(out, *p)
		}
	}
	return out
}

func parentDownFromSnapshot(snap *snapshot, start node) bool {
	visited := map[node]bool{start: true}
	stack := append([]node{}, downParents(snap, start)...)
	for len(stack) > 0 {
		p := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if visited[p] {
			continue
		}
		visited[p] = true
		pp := downParents(snap, p)
		if len(pp) == 0 {
			return true // p — down-корень, якорит подавление start
		}
		stack = append(stack, pp...)
	}
	return false // все пути вверх зациклились, реального корня нет → start пейджит
}

// Для source, отличного от "host", зависимости не резолвятся — молчаливый
// (false, false, nil): uptime резолвит их сам через свой сервис.
func (s *Suppressor) CheckIncident(ctx context.Context, source string, incidentID int64) (hasParent, parentDown bool, err error) {
	if source != "host" {
		return false, false, nil
	}

	var hostID int64
	if err := s.pool.QueryRow(ctx,
		`SELECT host_id FROM host_incidents WHERE id = $1`, incidentID,
	).Scan(&hostID); err != nil {
		// Гонка с закрытием инцидента между OpenUnacked и этим tickOne — не ошибка.
		if errors.Is(err, pgx.ErrNoRows) {
			return false, false, nil
		}
		return false, false, fmt.Errorf("depsuppress: load host_id for host_incident %d: %w", incidentID, err)
	}

	hasParent, err = s.HasParent(ctx, "host", hostID)
	if err != nil {
		return false, false, err
	}
	parentDown, err = s.ParentDown(ctx, "host", hostID)
	if err != nil {
		return false, false, err
	}
	return hasParent, parentDown, nil
}

// Единственный писатель suppressed_by_dep для host-инцидентов. Для source,
// отличного от "host", no-op: uptime-инциденты помечает только uptime.Service.
func (s *Suppressor) MarkSuppressed(ctx context.Context, source string, incidentID int64) error {
	if source != "host" {
		return nil
	}
	if _, err := s.pool.Exec(ctx,
		`UPDATE host_incidents SET suppressed_by_dep = true WHERE id = $1`, incidentID,
	); err != nil {
		return fmt.Errorf("depsuppress: mark host_incident %d suppressed: %w", incidentID, err)
	}
	return nil
}

// Пишем в s.cache, только если наш снимок не старше текущего — иначе вытесненная
// планировщиком горутина могла бы откатить более свежую запись назад во времени.
func (s *Suppressor) getSnapshot(ctx context.Context) (*snapshot, error) {
	s.mu.Lock()
	cache := s.cache
	fresh := cache != nil && s.now().Sub(cache.loadedAt) < s.cacheTTL
	s.mu.Unlock()
	if fresh {
		return cache, nil
	}

	snap, err := s.loadSnapshot(ctx)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	if s.cache == nil || snap.loadedAt.After(s.cache.loadedAt) {
		s.cache = snap
	}
	s.mu.Unlock()
	return snap, nil
}

func (s *Suppressor) loadSnapshot(ctx context.Context) (*snapshot, error) {
	edges, err := s.loadEdges(ctx)
	if err != nil {
		return nil, err
	}
	downHosts, err := s.loadDownHosts(ctx)
	if err != nil {
		return nil, err
	}
	downMonitors, err := s.loadDownMonitors(ctx)
	if err != nil {
		return nil, err
	}
	labels, err := s.loadHostLabels(ctx)
	if err != nil {
		return nil, err
	}
	return &snapshot{
		edges:        edges,
		downHosts:    downHosts,
		downMonitors: downMonitors,
		hostLabels:   labels,
		loadedAt:     s.now(),
	}, nil
}

// Тянет рёбра всех проектов сразу — набор невелик, резолвинг всё равно идёт
// по конкретному узлу, фильтровать по проекту здесь не нужно.
func (s *Suppressor) loadEdges(ctx context.Context) ([]Edge, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, project_id, parent_host_id, parent_monitor_id,
		       child_host_id, child_monitor_id, child_label_scope, child_label_value
		FROM alert_dependencies`)
	if err != nil {
		return nil, fmt.Errorf("depsuppress: load edges: %w", err)
	}
	defer rows.Close()

	var out []Edge
	for rows.Next() {
		var e Edge
		if err := rows.Scan(
			&e.ID, &e.ProjectID, &e.ParentHostID, &e.ParentMonitorID,
			&e.ChildHostID, &e.ChildMonitorID, &e.ChildLabelScope, &e.ChildLabelValue,
		); err != nil {
			return nil, fmt.Errorf("depsuppress: scan edge row: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("depsuppress: load edges: %w", err)
	}
	return out, nil
}

// «Хост упал» здесь — это молчание (kind='silent'), а не диск/память/нагрузка.
func (s *Suppressor) loadDownHosts(ctx context.Context) (map[int64]bool, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT DISTINCT host_id FROM host_incidents WHERE status = 'open' AND kind = 'silent'`)
	if err != nil {
		return nil, fmt.Errorf("depsuppress: load down hosts: %w", err)
	}
	defer rows.Close()

	out := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("depsuppress: scan down host row: %w", err)
		}
		out[id] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("depsuppress: load down hosts: %w", err)
	}
	return out, nil
}

func (s *Suppressor) loadDownMonitors(ctx context.Context) (map[int64]bool, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT DISTINCT monitor_id FROM incidents WHERE resolved_at IS NULL`)
	if err != nil {
		return nil, fmt.Errorf("depsuppress: load down monitors: %w", err)
	}
	defer rows.Close()

	out := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("depsuppress: scan down monitor row: %w", err)
		}
		out[id] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("depsuppress: load down monitors: %w", err)
	}
	return out, nil
}

func (s *Suppressor) loadHostLabels(ctx context.Context) (map[int64]hostLabels, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, project_id, environment, role FROM hosts`)
	if err != nil {
		return nil, fmt.Errorf("depsuppress: load host labels: %w", err)
	}
	defer rows.Close()

	out := map[int64]hostLabels{}
	for rows.Next() {
		var id int64
		var lbl hostLabels
		if err := rows.Scan(&id, &lbl.projectID, &lbl.env, &lbl.role); err != nil {
			return nil, fmt.Errorf("depsuppress: scan host label row: %w", err)
		}
		out[id] = lbl
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("depsuppress: load host labels: %w", err)
	}
	return out, nil
}

// Self-match исключается: ребро пропускается, если его родитель — тот же
// узел, иначе узел с label-ребром на собственную группу подавлял бы сам себя.
func matchingParents(snap *snapshot, kind string, nodeID int64) []Edge {
	var out []Edge
	for _, e := range snap.edges {
		if isSelfMatchParent(e, kind, nodeID) {
			continue
		}
		if edgeMatchesChild(e, snap, kind, nodeID) {
			out = append(out, e)
		}
	}
	return out
}

func isSelfMatchParent(e Edge, kind string, nodeID int64) bool {
	switch kind {
	case "host":
		return e.ParentHostID != nil && *e.ParentHostID == nodeID
	case "monitor":
		return e.ParentMonitorID != nil && *e.ParentMonitorID == nodeID
	default:
		return false
	}
}

func edgeMatchesChild(e Edge, snap *snapshot, kind string, nodeID int64) bool {
	switch kind {
	case "host":
		if e.ChildHostID != nil {
			return *e.ChildHostID == nodeID
		}
		if e.ChildLabelScope != nil && e.ChildLabelValue != nil {
			lbl, ok := snap.hostLabels[nodeID]
			if !ok || lbl.projectID != e.ProjectID {
				return false
			}
			switch *e.ChildLabelScope {
			case "env":
				return lbl.env == *e.ChildLabelValue
			case "role":
				return lbl.role == *e.ChildLabelValue
			}
		}
		return false
	case "monitor":
		return e.ChildMonitorID != nil && *e.ChildMonitorID == nodeID
	default:
		return false
	}
}

func parentIsDown(e Edge, snap *snapshot) bool {
	if e.ParentHostID != nil {
		return snap.downHosts[*e.ParentHostID]
	}
	if e.ParentMonitorID != nil {
		return snap.downMonitors[*e.ParentMonitorID]
	}
	return false
}

// При нескольких верхних корнях детерминизм: host прежде monitor, затем меньший id.
func (s *Suppressor) DownRoot(ctx context.Context, kind string, nodeID int64) (rootKind string, rootID int64, found bool, err error) {
	snap, err := s.getSnapshot(ctx)
	if err != nil {
		return "", 0, false, err
	}
	root, ok := downRootFromSnapshot(snap, node{kind: kind, id: nodeID})
	if !ok {
		return "", 0, false, nil
	}
	return root.kind, root.id, true, nil
}

func downRootFromSnapshot(snap *snapshot, start node) (node, bool) {
	var roots []node
	visited := map[node]bool{start: true}
	stack := append([]node{}, downParents(snap, start)...)
	for len(stack) > 0 {
		p := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if visited[p] {
			continue
		}
		visited[p] = true
		pp := downParents(snap, p)
		if len(pp) == 0 {
			roots = append(roots, p) // p — down-корень
			continue
		}
		stack = append(stack, pp...)
	}
	if len(roots) == 0 {
		if nodeIsDown(snap, start) {
			return start, true // сам узел упал, упавших предков нет — корень он
		}
		return node{}, false
	}
	sort.Slice(roots, func(i, j int) bool {
		if roots[i].kind != roots[j].kind {
			return roots[i].kind < roots[j].kind // "host" < "monitor"
		}
		return roots[i].id < roots[j].id
	})
	return roots[0], true
}

func nodeIsDown(snap *snapshot, n node) bool {
	switch n.kind {
	case "host":
		return snap.downHosts[n.id]
	case "monitor":
		return snap.downMonitors[n.id]
	default:
		return false
	}
}

// Без сброса только что открытый корневой инцидент не виден снимку возрастом
// до cacheTTL, и перебор кандидатов по нему молча пропустил бы всех членов.
func (s *Suppressor) Invalidate() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cache = nil
}

// Считает декларированных детей одного уровня, не «затронутых» транзитивно
// и без учёта текущего состояния — для строки «Зависимых узлов: N».
func (s *Suppressor) DeclaredChildrenCount(ctx context.Context, kind string, nodeID int64) (int, error) {
	snap, err := s.getSnapshot(ctx)
	if err != nil {
		return 0, err
	}
	return declaredChildrenFromSnapshot(snap, node{kind: kind, id: nodeID}), nil
}

func declaredChildrenFromSnapshot(snap *snapshot, self node) int {
	seen := map[node]bool{}
	for _, e := range snap.edges {
		p := parentNode(e)
		if p == nil || *p != self {
			continue
		}
		switch {
		case e.ChildHostID != nil:
			c := node{kind: "host", id: *e.ChildHostID}
			if c != self {
				seen[c] = true
			}
		case e.ChildMonitorID != nil:
			c := node{kind: "monitor", id: *e.ChildMonitorID}
			if c != self {
				seen[c] = true
			}
		case e.ChildLabelScope != nil && e.ChildLabelValue != nil:
			for hid, lbl := range snap.hostLabels {
				if lbl.projectID != e.ProjectID {
					continue
				}
				var v string
				switch *e.ChildLabelScope {
				case "env":
					v = lbl.env
				case "role":
					v = lbl.role
				}
				if v != *e.ChildLabelValue {
					continue
				}
				c := node{kind: "host", id: hid}
				if c != self {
					seen[c] = true
				}
			}
		}
	}
	return len(seen)
}
