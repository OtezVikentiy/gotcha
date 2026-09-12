package incidentgroup

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Group struct {
	ID             int64
	ProjectID      int64
	RootSource     string // 'host' | 'uptime'
	RootIncidentID int64
	RootNodeKind   string // 'host' | 'monitor'
	RootNodeID     int64
	StartedAt      time.Time
	ResolvedAt     *time.Time
}

const groupColumns = `id, project_id, root_source, root_incident_id, root_node_kind, root_node_id, started_at, resolved_at`

func scanGroup(row pgx.Row) (Group, error) {
	var g Group
	err := row.Scan(&g.ID, &g.ProjectID, &g.RootSource, &g.RootIncidentID,
		&g.RootNodeKind, &g.RootNodeID, &g.StartedAt, &g.ResolvedAt)
	return g, err
}

// Единственный писатель group_id всех 4 таблиц инцидентов — симметрия с однописательством suppressed_by_dep.
type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// Гонка двух открытий безопасна: INSERT ON CONFLICT DO NOTHING, проигравший дочитывает победителя.
func (s *Store) EnsureGroup(ctx context.Context, projectID int64, rootSource string, rootIncidentID int64, rootNodeKind string, rootNodeID int64) (Group, error) {
	row := s.pool.QueryRow(ctx, `
		INSERT INTO incident_groups (project_id, root_source, root_incident_id, root_node_kind, root_node_id)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (root_source, root_incident_id) DO NOTHING
		RETURNING `+groupColumns,
		projectID, rootSource, rootIncidentID, rootNodeKind, rootNodeID)
	g, err := scanGroup(row)
	if errors.Is(err, pgx.ErrNoRows) {
		row = s.pool.QueryRow(ctx,
			`SELECT `+groupColumns+` FROM incident_groups WHERE root_source = $1 AND root_incident_id = $2`,
			rootSource, rootIncidentID)
		g, err = scanGroup(row)
	}
	if err != nil {
		return Group{}, fmt.Errorf("incidentgroup: ensure group: %w", err)
	}
	return g, nil
}

// ok=false — открытой не было; идемпотентно, потому что sweep и хук закрытия корня могут гоняться.
func (s *Store) Resolve(ctx context.Context, rootSource string, rootIncidentID int64) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE incident_groups SET resolved_at = now()
		WHERE root_source = $1 AND root_incident_id = $2 AND resolved_at IS NULL`,
		rootSource, rootIncidentID)
	if err != nil {
		return false, fmt.Errorf("incidentgroup: resolve group: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// trace/profile не входят — у них нет узла, в карте их нет.
// У uptime (таблица `incidents`) нет своей project_id — фильтр идёт через monitors, как в feedProjectQuery.
var sourceMeta = map[string]struct {
	table       string
	projectCond string
}{
	"host":   {"host_incidents", `x.project_id = $2`},
	"uptime": {"incidents", `EXISTS (SELECT 1 FROM monitors mm WHERE mm.id = x.monitor_id AND mm.project_id = $2)`},
	"metric": {"metric_incidents", `x.project_id = $2`},
	"slo":    {"slo_incidents", `x.project_id = $2`},
}

// group_id на резолвнутую/удалённую группу не блокирует новое присоединение — первое побеждает.
// project_id в WHERE — защита от кросс-проектной записи; RowsAffected>0 решает судьбу пустой группы.
func (s *Store) SetGroup(ctx context.Context, projectID int64, source string, incidentID, groupID int64) (bool, error) {
	meta, ok := sourceMeta[source]
	if !ok {
		return false, fmt.Errorf("incidentgroup: set group: unknown source %q", source)
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE `+meta.table+` x SET group_id = $3
		WHERE x.id = $1 AND `+meta.projectCond+`
		  AND (x.group_id IS NULL OR NOT EXISTS (
		        SELECT 1 FROM incident_groups g WHERE g.id = x.group_id AND g.resolved_at IS NULL))`,
		incidentID, projectID, groupID)
	if err != nil {
		return false, fmt.Errorf("incidentgroup: set group %s/%d: %w", source, incidentID, err)
	}
	return tag.RowsAffected() > 0, nil
}

// Зовётся ДО EnsureGroup: если присоединение не состоится, группа нового корня не создаётся.
// Иначе на карточке повисает пустая группа, которую sweep не тронет — корень ещё открыт.
func (s *Store) MemberEligible(ctx context.Context, projectID int64, source string, incidentID int64) (bool, error) {
	meta, ok := sourceMeta[source]
	if !ok {
		return false, fmt.Errorf("incidentgroup: member eligible: unknown source %q", source)
	}
	var eligible bool
	err := s.pool.QueryRow(ctx, `
		SELECT x.group_id IS NULL OR NOT EXISTS (
		        SELECT 1 FROM incident_groups g WHERE g.id = x.group_id AND g.resolved_at IS NULL)
		FROM `+meta.table+` x WHERE x.id = $1 AND `+meta.projectCond,
		incidentID, projectID).Scan(&eligible)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("incidentgroup: member eligible %s/%d: %w", source, incidentID, err)
	}
	return eligible, nil
}

type GroupRow struct {
	Group
	RootName string
	// Есть только у host-корня — у uptime (`incidents`) колонки severity нет вовсе, категория неприменима.
	// incidentSeverityBadge не рисует бейдж на пустой строке — тот же случай, что у uptime-члена.
	RootSeverity string
}

const groupRowSelect = `
	SELECT g.id, g.project_id, g.root_source, g.root_incident_id, g.root_node_kind, g.root_node_id, g.started_at, g.resolved_at,
	       CASE WHEN g.root_node_kind = 'host' THEN COALESCE(h.name, '') ELSE COALESCE(m.name, '') END AS root_name,
	       COALESCE(rh.severity, '') AS root_severity
	FROM incident_groups g
	LEFT JOIN hosts h    ON g.root_node_kind = 'host'    AND h.id = g.root_node_id
	LEFT JOIN monitors m ON g.root_node_kind = 'monitor' AND m.id = g.root_node_id
	LEFT JOIN host_incidents rh ON g.root_source = 'host' AND rh.id = g.root_incident_id`

func (s *Store) queryGroupRows(ctx context.Context, tail string, args ...any) ([]GroupRow, error) {
	rows, err := s.pool.Query(ctx, groupRowSelect+tail, args...)
	if err != nil {
		return nil, fmt.Errorf("incidentgroup: groups: %w", err)
	}
	defer rows.Close()
	var out []GroupRow
	for rows.Next() {
		var g GroupRow
		if err := rows.Scan(&g.ID, &g.ProjectID, &g.RootSource, &g.RootIncidentID,
			&g.RootNodeKind, &g.RootNodeID, &g.StartedAt, &g.ResolvedAt, &g.RootName, &g.RootSeverity); err != nil {
			return nil, fmt.Errorf("incidentgroup: scan group row: %w", err)
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// Раньше — без LIMIT: открытых единицы-десятки было гарантией, пока не было проекта со штормом.
// Константа пакета, не параметр — используется и вне web-слоя, где произвольный лимит не нужен.
const (
	MaxOpenGroups     = 50
	MaxOpenOutOfGroup = 50
)

func (s *Store) OpenGroups(ctx context.Context, projectID int64) ([]GroupRow, error) {
	return s.queryGroupRows(ctx, `
		WHERE g.project_id = $1 AND g.resolved_at IS NULL
		ORDER BY g.started_at DESC
		LIMIT $2`, projectID, MaxOpenGroups)
}

func (s *Store) ClosedGroupsSince(ctx context.Context, projectID int64, since time.Time, limit int) ([]GroupRow, error) {
	return s.queryGroupRows(ctx, `
		WHERE g.project_id = $1 AND g.resolved_at IS NOT NULL AND g.resolved_at >= $2
		ORDER BY g.resolved_at DESC
		LIMIT $3`, projectID, since, limit)
}

type FeedItem struct {
	Source          string
	IncidentID      int64
	Title           string
	SubKind         string
	StartedAt       time.Time
	ResolvedAt      *time.Time
	Severity        string
	Acknowledged    bool
	SuppressedByDep bool
	RefID           int64
	RefName         string
	// Непустые только у OpenOutOfGroup/ClosedSince строк, чей group_id указывает на резолвнутую группу.
	// group_id на удалённую (purge) группу даёт FormerGroupID=0 — сведений о ней не осталось.
	FormerGroupID       int64
	FormerGroupRootName string
	// Не suppressed_by_dep — разные механизмы молчания, оба флага могут быть true одновременно.
	// Не хранится персистентно — восстанавливается из notified_open корня, монотонного пока открыт.
	HeldByGroup bool
}

func scanFeedItems(rows pgx.Rows) ([]FeedItem, error) {
	defer rows.Close()
	var out []FeedItem
	for rows.Next() {
		var it FeedItem
		if err := rows.Scan(&it.Source, &it.IncidentID, &it.Title, &it.SubKind,
			&it.StartedAt, &it.ResolvedAt, &it.Severity, &it.Acknowledged,
			&it.SuppressedByDep, &it.RefID, &it.RefName,
			&it.FormerGroupID, &it.FormerGroupRootName, &it.HeldByGroup); err != nil {
			return nil, fmt.Errorf("incidentgroup: scan feed item: %w", err)
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// У uptime LEFT JOIN monitors фактически ведёт себя как INNER — m.project_id в WHERE отсеивает NULL-строки.
// HeldByGroup — через общий CTE grp: группа/корень одни на весь состав, $1 не меняется по веткам.
const feedMemberSelect = `
	WITH grp AS (
		SELECT g.resolved_at AS group_resolved_at,
		       COALESCE(rh.notified_open, ri.notified_open, false) AS root_notified_open
		FROM incident_groups g
		LEFT JOIN host_incidents rh ON g.root_source = 'host'   AND rh.id = g.root_incident_id
		LEFT JOIN incidents      ri ON g.root_source = 'uptime' AND ri.id = g.root_incident_id
		WHERE g.id = $1
	)
	SELECT 'host'::text, hi.id, COALESCE(h.name,''), hi.kind,
	       hi.started_at, hi.resolved_at, hi.severity,
	       hi.acknowledged_at IS NOT NULL, hi.suppressed_by_dep, 0::bigint, COALESCE(h.name,''),
	       0::bigint, ''::text,
	       COALESCE(grp.group_resolved_at IS NULL AND hi.resolved_at IS NULL AND grp.root_notified_open, false)
	FROM host_incidents hi
	LEFT JOIN hosts h ON h.id = hi.host_id
	LEFT JOIN grp ON true
	WHERE hi.group_id = $1 AND hi.project_id = $2
	UNION ALL
	SELECT 'uptime', i.id, COALESCE(m.name,''), '',
	       i.started_at, i.resolved_at, '',
	       false, i.suppressed_by_dep, i.monitor_id, '',
	       0::bigint, ''::text,
	       COALESCE(grp.group_resolved_at IS NULL AND i.resolved_at IS NULL AND grp.root_notified_open, false)
	FROM incidents i
	LEFT JOIN monitors m ON m.id = i.monitor_id
	LEFT JOIN grp ON true
	WHERE i.group_id = $1 AND m.project_id = $2
	UNION ALL
	SELECT 'metric', mi.id, COALESCE(r.metric_name,''), '',
	       mi.started_at, mi.resolved_at, mi.severity,
	       mi.acknowledged_at IS NOT NULL, false, mi.rule_id, '',
	       0::bigint, ''::text,
	       COALESCE(grp.group_resolved_at IS NULL AND mi.resolved_at IS NULL AND grp.root_notified_open, false)
	FROM metric_incidents mi
	LEFT JOIN metric_alert_rules r ON r.id = mi.rule_id
	LEFT JOIN grp ON true
	WHERE mi.group_id = $1 AND mi.project_id = $2
	UNION ALL
	SELECT 'slo', si.id, COALESCE(sl.name,''), '',
	       si.started_at, si.resolved_at, si.severity,
	       si.acknowledged_at IS NOT NULL, false, si.slo_id, '',
	       0::bigint, ''::text,
	       COALESCE(grp.group_resolved_at IS NULL AND si.resolved_at IS NULL AND grp.root_notified_open, false)
	FROM slo_incidents si
	LEFT JOIN slos sl ON sl.id = si.slo_id
	LEFT JOIN grp ON true
	WHERE si.group_id = $1 AND si.project_id = $2`

// projectID — вторая линия защиты: группа чужого проекта отдаст пустой список, не чужие инциденты.
func (s *Store) Composition(ctx context.Context, projectID, groupID int64) ([]FeedItem, error) {
	rows, err := s.pool.Query(ctx, feedMemberSelect+` ORDER BY 5`, groupID, projectID)
	if err != nil {
		return nil, fmt.Errorf("incidentgroup: composition: %w", err)
	}
	return scanFeedItems(rows)
}

// Как feedMemberSelect, но $1 — массив id; join на grp — по group_id, не `ON true`.
// group_id первым столбцом — группировка по группам в Go (scanFeedItemsBatch), не array_agg в SQL.
const feedMemberSelectBatch = `
	WITH grp AS (
		SELECT g.id AS group_id, g.resolved_at AS group_resolved_at,
		       COALESCE(rh.notified_open, ri.notified_open, false) AS root_notified_open
		FROM incident_groups g
		LEFT JOIN host_incidents rh ON g.root_source = 'host'   AND rh.id = g.root_incident_id
		LEFT JOIN incidents      ri ON g.root_source = 'uptime' AND ri.id = g.root_incident_id
		WHERE g.id = ANY($1)
	)
	SELECT hi.group_id, 'host'::text, hi.id, COALESCE(h.name,''), hi.kind,
	       hi.started_at, hi.resolved_at, hi.severity,
	       hi.acknowledged_at IS NOT NULL, hi.suppressed_by_dep, 0::bigint, COALESCE(h.name,''),
	       0::bigint, ''::text,
	       COALESCE(grp.group_resolved_at IS NULL AND hi.resolved_at IS NULL AND grp.root_notified_open, false)
	FROM host_incidents hi
	LEFT JOIN hosts h ON h.id = hi.host_id
	LEFT JOIN grp ON grp.group_id = hi.group_id
	WHERE hi.group_id = ANY($1) AND hi.project_id = $2
	UNION ALL
	SELECT i.group_id, 'uptime', i.id, COALESCE(m.name,''), '',
	       i.started_at, i.resolved_at, '',
	       false, i.suppressed_by_dep, i.monitor_id, '',
	       0::bigint, ''::text,
	       COALESCE(grp.group_resolved_at IS NULL AND i.resolved_at IS NULL AND grp.root_notified_open, false)
	FROM incidents i
	LEFT JOIN monitors m ON m.id = i.monitor_id
	LEFT JOIN grp ON grp.group_id = i.group_id
	WHERE i.group_id = ANY($1) AND m.project_id = $2
	UNION ALL
	SELECT mi.group_id, 'metric', mi.id, COALESCE(r.metric_name,''), '',
	       mi.started_at, mi.resolved_at, mi.severity,
	       mi.acknowledged_at IS NOT NULL, false, mi.rule_id, '',
	       0::bigint, ''::text,
	       COALESCE(grp.group_resolved_at IS NULL AND mi.resolved_at IS NULL AND grp.root_notified_open, false)
	FROM metric_incidents mi
	LEFT JOIN metric_alert_rules r ON r.id = mi.rule_id
	LEFT JOIN grp ON grp.group_id = mi.group_id
	WHERE mi.group_id = ANY($1) AND mi.project_id = $2
	UNION ALL
	SELECT si.group_id, 'slo', si.id, COALESCE(sl.name,''), '',
	       si.started_at, si.resolved_at, si.severity,
	       si.acknowledged_at IS NOT NULL, false, si.slo_id, '',
	       0::bigint, ''::text,
	       COALESCE(grp.group_resolved_at IS NULL AND si.resolved_at IS NULL AND grp.root_notified_open, false)
	FROM slo_incidents si
	LEFT JOIN slos sl ON sl.id = si.slo_id
	LEFT JOIN grp ON grp.group_id = si.group_id
	WHERE si.group_id = ANY($1) AND si.project_id = $2`

func scanFeedItemsBatch(rows pgx.Rows) (map[int64][]FeedItem, error) {
	defer rows.Close()
	out := map[int64][]FeedItem{}
	for rows.Next() {
		var groupID int64
		var it FeedItem
		if err := rows.Scan(&groupID, &it.Source, &it.IncidentID, &it.Title, &it.SubKind,
			&it.StartedAt, &it.ResolvedAt, &it.Severity, &it.Acknowledged,
			&it.SuppressedByDep, &it.RefID, &it.RefName,
			&it.FormerGroupID, &it.FormerGroupRootName, &it.HeldByGroup); err != nil {
			return nil, fmt.Errorf("incidentgroup: scan feed item batch: %w", err)
		}
		out[groupID] = append(out[groupID], it)
	}
	return out, rows.Err()
}

// Заменяет цикл Composition по каждой группе ленты — десятки round-trip на одну отрисовку страницы.
func (s *Store) Compositions(ctx context.Context, projectID int64, groupIDs []int64) (map[int64][]FeedItem, error) {
	if len(groupIDs) == 0 {
		return map[int64][]FeedItem{}, nil
	}
	rows, err := s.pool.Query(ctx, feedMemberSelectBatch+` ORDER BY 1, 6`, groupIDs, projectID)
	if err != nil {
		return nil, fmt.Errorf("incidentgroup: compositions: %w", err)
	}
	return scanFeedItemsBatch(rows)
}

// group_id NULL, группа удалена janitor'ом (wg.id NULL), либо резолвнута.
// LEFT JOIN, не NOT EXISTS — тем же wg отдаём данные FormerGroup* ниже.
func notOpenGroupMember(alias string) string {
	return `(` + alias + `.group_id IS NULL OR wg.id IS NULL OR wg.resolved_at IS NOT NULL)`
}

// Условия статуса — константы этого файла, не пользовательский ввод, конкатенация безопасна.
// HeldByGroup всегда false — строки этого запроса по определению не члены ОТКРЫТОЙ группы.
func feedProjectQuery(hostCond, uptimeCond, metricCond, sloCond, traceCond, profileCond string) string {
	return `
	SELECT 'host'::text, hi.id, COALESCE(h.name,''), hi.kind,
	       hi.started_at, hi.resolved_at, hi.severity,
	       hi.acknowledged_at IS NOT NULL, hi.suppressed_by_dep, 0::bigint, COALESCE(h.name,''),
	       COALESCE(wg.id, 0::bigint), COALESCE(wgh.name, wgm.name, ''), false
	FROM host_incidents hi
	LEFT JOIN hosts h ON h.id = hi.host_id
	LEFT JOIN incident_groups wg ON wg.id = hi.group_id AND wg.project_id = $1
	LEFT JOIN hosts wgh ON wg.root_node_kind = 'host' AND wgh.id = wg.root_node_id
	LEFT JOIN monitors wgm ON wg.root_node_kind = 'monitor' AND wgm.id = wg.root_node_id
	WHERE hi.project_id = $1 AND ` + notOpenGroupMember("hi") + ` AND ` + hostCond + `
	UNION ALL
	SELECT 'uptime', i.id, COALESCE(m.name,''), '',
	       i.started_at, i.resolved_at, '',
	       false, i.suppressed_by_dep, i.monitor_id, '',
	       COALESCE(wg.id, 0::bigint), COALESCE(wgh.name, wgm.name, ''), false
	FROM incidents i
	JOIN monitors m ON m.id = i.monitor_id
	LEFT JOIN incident_groups wg ON wg.id = i.group_id AND wg.project_id = $1
	LEFT JOIN hosts wgh ON wg.root_node_kind = 'host' AND wgh.id = wg.root_node_id
	LEFT JOIN monitors wgm ON wg.root_node_kind = 'monitor' AND wgm.id = wg.root_node_id
	WHERE m.project_id = $1 AND ` + notOpenGroupMember("i") + ` AND ` + uptimeCond + `
	UNION ALL
	SELECT 'metric', mi.id, COALESCE(r.metric_name,''), '',
	       mi.started_at, mi.resolved_at, mi.severity,
	       mi.acknowledged_at IS NOT NULL, false, mi.rule_id, '',
	       COALESCE(wg.id, 0::bigint), COALESCE(wgh.name, wgm.name, ''), false
	FROM metric_incidents mi
	LEFT JOIN metric_alert_rules r ON r.id = mi.rule_id
	LEFT JOIN incident_groups wg ON wg.id = mi.group_id AND wg.project_id = $1
	LEFT JOIN hosts wgh ON wg.root_node_kind = 'host' AND wgh.id = wg.root_node_id
	LEFT JOIN monitors wgm ON wg.root_node_kind = 'monitor' AND wgm.id = wg.root_node_id
	WHERE mi.project_id = $1 AND ` + notOpenGroupMember("mi") + ` AND ` + metricCond + `
	UNION ALL
	SELECT 'slo', si.id, COALESCE(sl.name,''), '',
	       si.started_at, si.resolved_at, si.severity,
	       si.acknowledged_at IS NOT NULL, false, si.slo_id, '',
	       COALESCE(wg.id, 0::bigint), COALESCE(wgh.name, wgm.name, ''), false
	FROM slo_incidents si
	LEFT JOIN slos sl ON sl.id = si.slo_id
	LEFT JOIN incident_groups wg ON wg.id = si.group_id AND wg.project_id = $1
	LEFT JOIN hosts wgh ON wg.root_node_kind = 'host' AND wgh.id = wg.root_node_id
	LEFT JOIN monitors wgm ON wg.root_node_kind = 'monitor' AND wgm.id = wg.root_node_id
	WHERE si.project_id = $1 AND ` + notOpenGroupMember("si") + ` AND ` + sloCond + `
	UNION ALL
	SELECT 'trace', pr.id, pr.target, pr.metric,
	       pr.started_at, pr.resolved_at, pr.severity,
	       pr.acknowledged_at IS NOT NULL, false, pr.id, '',
	       0::bigint, ''::text, false
	FROM perf_regressions pr
	WHERE pr.project_id = $1 AND ` + traceCond + `
	UNION ALL
	SELECT 'profile', pf.id, pf.function, pf.profile_type,
	       pf.started_at, pf.resolved_at, pf.severity,
	       pf.acknowledged_at IS NOT NULL, false, pf.id, '',
	       0::bigint, ''::text, false
	FROM profile_regressions pf
	WHERE pf.project_id = $1 AND ` + profileCond
}

// Корню group_id не проставляется — без исключения он дублировался бы в шапке карточки и «Вне групп».
// resolved_at IS NULL исключает только корень ЕЩЁ открытой группы — закрытая его больше не прячет.
const (
	hostNotRoot   = `NOT EXISTS (SELECT 1 FROM incident_groups g WHERE g.project_id = $1 AND g.root_source = 'host' AND g.root_incident_id = hi.id AND g.resolved_at IS NULL)`
	uptimeNotRoot = `NOT EXISTS (SELECT 1 FROM incident_groups g WHERE g.project_id = $1 AND g.root_source = 'uptime' AND g.root_incident_id = i.id AND g.resolved_at IS NULL)`
)

// Открытый член резолвнутой группы не прячется — здесь «открытая работа», в карточке «упало вместе».
func (s *Store) OpenOutOfGroup(ctx context.Context, projectID int64) ([]FeedItem, error) {
	q := feedProjectQuery(
		`hi.status = 'open' AND `+hostNotRoot,
		`i.resolved_at IS NULL AND `+uptimeNotRoot,
		`mi.status = 'open'`,
		`si.status = 'open'`,
		`pr.status = 'open'`,
		`pf.status = 'open'`,
	) + ` ORDER BY 5 DESC LIMIT $2`
	rows, err := s.pool.Query(ctx, q, projectID, MaxOpenOutOfGroup)
	if err != nil {
		return nil, fmt.Errorf("incidentgroup: open out of group: %w", err)
	}
	return scanFeedItems(rows)
}

// Член ОТКРЫТОЙ группы не попадает — показан в её карточке, появится здесь после закрытия.
// Член уже резолвнутой/удалённой группы попадает — намеренный дубль, симметричный OpenOutOfGroup.
func (s *Store) ClosedSince(ctx context.Context, projectID int64, since time.Time, limit int) ([]FeedItem, error) {
	q := feedProjectQuery(
		`hi.status = 'resolved' AND hi.resolved_at >= $2 AND `+hostNotRoot,
		`i.resolved_at IS NOT NULL AND i.resolved_at >= $2 AND `+uptimeNotRoot,
		`mi.status = 'resolved' AND mi.resolved_at >= $2`,
		`si.status = 'resolved' AND si.resolved_at >= $2`,
		`pr.status = 'resolved' AND pr.resolved_at >= $2`,
		`pf.status = 'resolved' AND pf.resolved_at >= $2`,
	) + ` ORDER BY 6 DESC LIMIT $3`
	rows, err := s.pool.Query(ctx, q, projectID, since, limit)
	if err != nil {
		return nil, fmt.Errorf("incidentgroup: closed since: %w", err)
	}
	return scanFeedItems(rows)
}
