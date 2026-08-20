package depsuppress

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// cacheTTL — время жизни снимка состояния зависимостей/инцидентов (кеш-на-
// тик): резолвинг HasParent/ParentDown не бьёт в БД на каждый вызов —
// снимок {edges, downHosts, downMonitors, hostLabels} переиспользуется, пока
// не устареет старше cacheTTL, и тогда перезагружается целиком одним
// набором запросов. Резолвинг конкретного узла по снимку — работа в памяти,
// без дополнительных SQL-запросов (никакого N+1 по узлам).
const cacheTTL = 5 * time.Second

// hostLabels — метки хоста (env/role, волна B1) плюс его project_id, нужны
// для резолвинга label-селекторов ребра (child_label_scope/child_label_
// value). project_id обязателен: метки типовые (web/prod/db повторяются
// между тенантами), и без сверки проекта label-ребро одного проекта
// подавляло бы инциденты одноимённых хостов ЧУЖОГО проекта (межпроектная
// утечка подавления — находка ревью).
type hostLabels struct {
	projectID int64
	env       string
	role      string
}

// snapshot — единый срез состояния на момент loadedAt, по которому
// резолвятся HasParent/ParentDown без дополнительных запросов к БД.
type snapshot struct {
	edges        []Edge
	downHosts    map[int64]bool
	downMonitors map[int64]bool
	hostLabels   map[int64]hostLabels
	loadedAt     time.Time
}

// Suppressor отвечает на вопрос «упал ли задекларированный родитель узла?»
// поверх рёбер зависимостей (alert_dependencies, см. Store) и открытых
// инцидентов недоступности (host_incidents.kind='silent' и uptime-инциденты
// incidents). Используется host-evaluator'ом и uptime-детектором
// (HasParent/ParentDown) и escalation-scheduler'ом (CheckIncident/
// MarkSuppressed), у которого нет прямой зависимости на пакет host.
type Suppressor struct {
	pool     *pgxpool.Pool
	cacheTTL time.Duration
	// now — источник текущего времени для кеша-на-тик; в проде time.Now,
	// в тестах подменяется для детерминируемого истечения TTL.
	now func() time.Time

	mu    sync.Mutex
	cache *snapshot
}

// NewSuppressor создаёт Suppressor поверх пула соединений PostgreSQL.
func NewSuppressor(pool *pgxpool.Pool) *Suppressor {
	return &Suppressor{
		pool:     pool,
		cacheTTL: cacheTTL,
		now:      time.Now,
	}
}

// HasParent сообщает, задекларирован ли у узла (kind, nodeID) хотя бы один
// родитель (явное ребро или label-селектор, матчащий метки узла), без учёта
// того, упал ли этот родитель. kind ∈ {"host","monitor"}.
func (s *Suppressor) HasParent(ctx context.Context, kind string, nodeID int64) (bool, error) {
	snap, err := s.getSnapshot(ctx)
	if err != nil {
		return false, err
	}
	return len(matchingParents(snap, kind, nodeID)) > 0, nil
}

// ParentDown сообщает, упал ли хотя бы один ПРЯМОЙ (один уровень, БЕЗ
// рекурсии — depth-cap) задекларированный родитель узла (kind, nodeID).
//
// Инциденты родителей НЕ фильтруются по suppressed_by_dep (MINOR-7,
// инвариант транзитивности): если сам родитель — промежуточный узел
// транзитивной цепочки A→B→C и уже помечен как подавленный (suppressed_by_
// dep=true), его инцидент всё равно остаётся status='open' (host) /
// resolved_at IS NULL (uptime) и ПРОДОЛЖАЕТ подавлять C — иначе цепочка
// рвётся на втором звене, как только B подавляется. Рекурсии вверх по
// цепочке здесь нет: каждый вызов ParentDown резолвит ровно один уровень
// (родителей узла, не родителей родителя) — это же и страхует от зацикливания
// на label-рёбрах, которые не участвуют в проверке циклов Store.checkCycle.
func (s *Suppressor) ParentDown(ctx context.Context, kind string, nodeID int64) (bool, error) {
	snap, err := s.getSnapshot(ctx)
	if err != nil {
		return false, err
	}
	for _, e := range matchingParents(snap, kind, nodeID) {
		if parentIsDown(e, snap) {
			return true, nil
		}
	}
	return false, nil
}

// CheckIncident отвечает на тот же вопрос, что и HasParent/ParentDown, но
// принимает не узел, а конкретный инцидент — так его вызывает escalation-
// scheduler, у которого на входе только (source, incidentID). Для source
// отличного от "host" зависимости пока не резолвятся (uptime резолвит их
// сам через свой сервис, см. T6/T7) — возвращается (false, false, nil).
func (s *Suppressor) CheckIncident(ctx context.Context, source string, incidentID int64) (hasParent, parentDown bool, err error) {
	if source != "host" {
		return false, false, nil
	}

	var hostID int64
	if err := s.pool.QueryRow(ctx,
		`SELECT host_id FROM host_incidents WHERE id = $1`, incidentID,
	).Scan(&hostID); err != nil {
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

// MarkSuppressed помечает инцидент как подавленный зависимостью
// (suppressed_by_dep=true). Единственный писатель этого флага для host-
// инцидентов — сам host себя не помечает, чтобы не дублировать логику
// резолвинга зависимостей в двух местах. Для source, отличного от "host",
// это no-op: uptime-инциденты помечает исключительно uptime.Service.
// MarkSuppressedByDep (T6) — так у флага остаётся ровно один писатель на
// таблицу.
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

// getSnapshot возвращает текущий кеш-на-тик, перезагружая его, если он ещё
// не был загружен или устарел старше cacheTTL.
func (s *Suppressor) getSnapshot(ctx context.Context) (*snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.cache != nil && s.now().Sub(s.cache.loadedAt) < s.cacheTTL {
		return s.cache, nil
	}

	snap, err := s.loadSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	s.cache = snap
	return snap, nil
}

// loadSnapshot тянет весь снимок состояния одним набором запросов (по
// одному на каждую из четырёх составляющих — рёбра, упавшие хосты, упавшие
// мониторы, метки хостов).
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

// loadEdges тянет весь набор рёбер зависимостей всех проектов: набор
// невелик, а резолвинг всё равно идёт по конкретному узлу — фильтрация по
// проекту в памяти не нужна.
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

// loadDownHosts — id хостов с открытым инцидентом недоступности
// (kind='silent'): «хост упал» в терминах B5 — это молчание хоста, а не
// диск/память/нагрузка.
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

// loadDownMonitors — id мониторов с открытым uptime-инцидентом.
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

// loadHostLabels — project_id и метки (env, role) всех хостов, нужны для
// резолвинга label-селекторов рёбер (child_label_scope/value) без N+1-
// запроса на узел; project_id — обязательная часть снимка ради изоляции
// тенантов при матче (см. edgeMatchesChild).
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

// matchingParents возвращает рёбра, чей ребёнок — узел (kind, nodeID):
// явное совпадение (child_host_id/child_monitor_id) либо, для kind="host",
// label-селектор (child_label_scope/value), матчащий метки хоста nodeID В
// ТОМ ЖЕ ПРОЕКТЕ, что и ребро (e.ProjectID == hostLabels[nodeID].projectID).
// Explicit-рёбра project_id не сверяют — child_host_id/child_monitor_id уже
// глобально уникальны и принадлежность проекту проверена Store.Create при
// вставке ребра.
//
// Проверка проекта для label-рёбер обязательна: метки типовые (web/prod/db
// повторяются между тенантами), и без неё ребро одного проекта (например,
// «шлюз P1 → все хосты role=web») матчило бы одноимённые хосты ЧУЖОГО
// проекта — межпроектная утечка подавления, тихая потеря чужих алертов
// (находка ревью, устранена).
//
// Self-match ИСКЛЮЧАЕТСЯ: ребро пропускается, если его РОДИТЕЛЬ — тот же
// узел (kind, nodeID) — иначе узел с label-ребром на собственную группу
// подавлял бы сам себя, как только у него открывается инцидент (MAJOR-5
// ревью дизайна; см. TestParentDownLabelAndTransitive/self-match).
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

// parentIsDown сообщает, находится ли родитель ребра e в состоянии «упал»
// согласно снимку snap.
func parentIsDown(e Edge, snap *snapshot) bool {
	if e.ParentHostID != nil {
		return snap.downHosts[*e.ParentHostID]
	}
	if e.ParentMonitorID != nil {
		return snap.downMonitors[*e.ParentMonitorID]
	}
	return false
}
