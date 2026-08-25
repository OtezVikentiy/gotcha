// Package incidentgroup реализует корреляцию алертов (D3): группы инцидентов
// вокруг корневого инцидента недоступности узла (host silent / uptime down)
// поверх графа зависимостей B5 (depsuppress). Хранение — таблица
// incident_groups + колонка group_id на 4 таблицах инцидентов (миграция
// 0079, Р6: без таблицы членов и без FK — состав группы == выборка по
// group_id, группа переживает ретеншен инцидентов).
package incidentgroup

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Group — группа инцидентов: корневой инцидент недоступности узла + члены
// (инциденты 4 источников с group_id = ID).
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

// Store — CRUD групп поверх incident_groups; единственный писатель колонки
// group_id всех 4 таблиц инцидентов (SetGroup) — симметрия с
// однописательством suppressed_by_dep (см. depsuppress.MarkSuppressed).
type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// EnsureGroup идемпотентно создаёт группу корневого инцидента. Гонка двух
// открытий безопасна: INSERT .. ON CONFLICT (root_source, root_incident_id)
// DO NOTHING, проигравший дочитывает победителя (тот же приём, что
// host.IncidentService.Open).
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

// Resolve закрывает открытую группу корневого инцидента. ok=false — открытой
// не было (идемпотентно; sweep и хук закрытия корня могут гоняться).
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

// sourceTables — таблица инцидентов каждого источника-члена. trace/profile
// в группы не входят (Р2 — у них нет узла).
var sourceTables = map[string]string{
	"host":   "host_incidents",
	"uptime": "incidents",
	"metric": "metric_incidents",
	"slo":    "slo_incidents",
}

// SetGroup присоединяет инцидент к группе. group_id IS NULL в WHERE —
// первое присоединение выигрывает и пере-attach не делается (MINOR-8:
// смена корня при досылке не меняет числа уведомлений, только усложняет).
func (s *Store) SetGroup(ctx context.Context, source string, incidentID, groupID int64) error {
	table, ok := sourceTables[source]
	if !ok {
		return fmt.Errorf("incidentgroup: set group: unknown source %q", source)
	}
	if _, err := s.pool.Exec(ctx,
		`UPDATE `+table+` SET group_id = $2 WHERE id = $1 AND group_id IS NULL`,
		incidentID, groupID); err != nil {
		return fmt.Errorf("incidentgroup: set group %s/%d: %w", source, incidentID, err)
	}
	return nil
}

// GroupRow — группа с резолвнутым именем корневого узла (для карточки ленты).
type GroupRow struct {
	Group
	RootName string
}

const groupRowSelect = `
	SELECT g.id, g.project_id, g.root_source, g.root_incident_id, g.root_node_kind, g.root_node_id, g.started_at, g.resolved_at,
	       CASE WHEN g.root_node_kind = 'host' THEN COALESCE(h.name, '') ELSE COALESCE(m.name, '') END AS root_name
	FROM incident_groups g
	LEFT JOIN hosts h    ON g.root_node_kind = 'host'    AND h.id = g.root_node_id
	LEFT JOIN monitors m ON g.root_node_kind = 'monitor' AND m.id = g.root_node_id`

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
			&g.RootNodeKind, &g.RootNodeID, &g.StartedAt, &g.ResolvedAt, &g.RootName); err != nil {
			return nil, fmt.Errorf("incidentgroup: scan group row: %w", err)
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// OpenGroups — открытые группы проекта, свежайшие первыми (лента §6.1/1).
func (s *Store) OpenGroups(ctx context.Context, projectID int64) ([]GroupRow, error) {
	return s.queryGroupRows(ctx, `
		WHERE g.project_id = $1 AND g.resolved_at IS NULL
		ORDER BY g.started_at DESC`, projectID)
}

// ClosedGroupsSince — группы, закрытые не раньше since (лента §6.1/3).
func (s *Store) ClosedGroupsSince(ctx context.Context, projectID int64, since time.Time, limit int) ([]GroupRow, error) {
	return s.queryGroupRows(ctx, `
		WHERE g.project_id = $1 AND g.resolved_at IS NOT NULL AND g.resolved_at >= $2
		ORDER BY g.resolved_at DESC
		LIMIT $3`, projectID, since, limit)
}

// FeedItem — унифицированная строка ленты/состава: инцидент любого из 6
// источников с именем объекта и данными для ссылки/бейджей.
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
}

func scanFeedItems(rows pgx.Rows) ([]FeedItem, error) {
	defer rows.Close()
	var out []FeedItem
	for rows.Next() {
		var it FeedItem
		if err := rows.Scan(&it.Source, &it.IncidentID, &it.Title, &it.SubKind,
			&it.StartedAt, &it.ResolvedAt, &it.Severity, &it.Acknowledged,
			&it.SuppressedByDep, &it.RefID, &it.RefName); err != nil {
			return nil, fmt.Errorf("incidentgroup: scan feed item: %w", err)
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// feedMemberSelect — 4 источника-члена по group_id (состав группы, Р6:
// выборка по колонке, без таблицы членов). LEFT JOIN + COALESCE — исчезнувший
// справочник (правило/SLO удалены) не прячет члена из состава.
const feedMemberSelect = `
	SELECT 'host'::text, hi.id, COALESCE(h.name,''), hi.kind,
	       hi.started_at, hi.resolved_at, hi.severity,
	       hi.acknowledged_at IS NOT NULL, hi.suppressed_by_dep, 0::bigint, COALESCE(h.name,'')
	FROM host_incidents hi LEFT JOIN hosts h ON h.id = hi.host_id
	WHERE hi.group_id = $1
	UNION ALL
	SELECT 'uptime', i.id, COALESCE(m.name,''), '',
	       i.started_at, i.resolved_at, '',
	       false, i.suppressed_by_dep, i.monitor_id, ''
	FROM incidents i LEFT JOIN monitors m ON m.id = i.monitor_id
	WHERE i.group_id = $1
	UNION ALL
	SELECT 'metric', mi.id, COALESCE(r.metric_name,''), '',
	       mi.started_at, mi.resolved_at, mi.severity,
	       mi.acknowledged_at IS NOT NULL, false, mi.rule_id, ''
	FROM metric_incidents mi LEFT JOIN metric_alert_rules r ON r.id = mi.rule_id
	WHERE mi.group_id = $1
	UNION ALL
	SELECT 'slo', si.id, COALESCE(sl.name,''), '',
	       si.started_at, si.resolved_at, si.severity,
	       si.acknowledged_at IS NOT NULL, false, si.slo_id, ''
	FROM slo_incidents si LEFT JOIN slos sl ON sl.id = si.slo_id
	WHERE si.group_id = $1`

// Composition — состав группы: члены 4 источников, старейшие первыми.
func (s *Store) Composition(ctx context.Context, groupID int64) ([]FeedItem, error) {
	rows, err := s.pool.Query(ctx, feedMemberSelect+` ORDER BY 5`, groupID)
	if err != nil {
		return nil, fmt.Errorf("incidentgroup: composition: %w", err)
	}
	return scanFeedItems(rows)
}

// feedProjectQuery — 6 источников по проекту; условия статуса/группы
// подставляются готовыми строками-константами этого файла (не
// пользовательский ввод — конкатенация безопасна). $1 — project_id;
// условия ClosedSince дополнительно ссылаются на $2 (since), LIMIT — $3.
// MINOR-10: все ветки ходят по project_id + status/resolved_at — у
// host/metric/slo есть индексы по project_id (см. существующие
// ListByProject), incident_groups_open_idx частичный по project_id;
// таблицы малы, отдельных индексов под ленту не заводим.
func feedProjectQuery(hostCond, uptimeCond, metricCond, sloCond, traceCond, profileCond string) string {
	return `
	SELECT 'host'::text, hi.id, COALESCE(h.name,''), hi.kind,
	       hi.started_at, hi.resolved_at, hi.severity,
	       hi.acknowledged_at IS NOT NULL, hi.suppressed_by_dep, 0::bigint, COALESCE(h.name,'')
	FROM host_incidents hi LEFT JOIN hosts h ON h.id = hi.host_id
	WHERE hi.project_id = $1 AND ` + hostCond + `
	UNION ALL
	SELECT 'uptime', i.id, COALESCE(m.name,''), '',
	       i.started_at, i.resolved_at, '',
	       false, i.suppressed_by_dep, i.monitor_id, ''
	FROM incidents i JOIN monitors m ON m.id = i.monitor_id
	WHERE m.project_id = $1 AND ` + uptimeCond + `
	UNION ALL
	SELECT 'metric', mi.id, COALESCE(r.metric_name,''), '',
	       mi.started_at, mi.resolved_at, mi.severity,
	       mi.acknowledged_at IS NOT NULL, false, mi.rule_id, ''
	FROM metric_incidents mi LEFT JOIN metric_alert_rules r ON r.id = mi.rule_id
	WHERE mi.project_id = $1 AND ` + metricCond + `
	UNION ALL
	SELECT 'slo', si.id, COALESCE(sl.name,''), '',
	       si.started_at, si.resolved_at, si.severity,
	       si.acknowledged_at IS NOT NULL, false, si.slo_id, ''
	FROM slo_incidents si LEFT JOIN slos sl ON sl.id = si.slo_id
	WHERE si.project_id = $1 AND ` + sloCond + `
	UNION ALL
	SELECT 'trace', pr.id, pr.target, pr.metric,
	       pr.started_at, pr.resolved_at, pr.severity,
	       pr.acknowledged_at IS NOT NULL, false, pr.id, ''
	FROM perf_regressions pr
	WHERE pr.project_id = $1 AND ` + traceCond + `
	UNION ALL
	SELECT 'profile', pf.id, pf.function, pf.profile_type,
	       pf.started_at, pf.resolved_at, pf.severity,
	       pf.acknowledged_at IS NOT NULL, false, pf.id, ''
	FROM profile_regressions pf
	WHERE pf.project_id = $1 AND ` + profileCond
}

// OpenOutOfGroup — открытые ВНЕгрупповые инциденты всех 6 источников
// (§6.1/2): trace/profile всегда вне групп (Р2 — узла нет).
func (s *Store) OpenOutOfGroup(ctx context.Context, projectID int64) ([]FeedItem, error) {
	q := feedProjectQuery(
		`hi.status = 'open' AND hi.group_id IS NULL`,
		`i.resolved_at IS NULL AND i.group_id IS NULL`,
		`mi.status = 'open' AND mi.group_id IS NULL`,
		`si.status = 'open' AND si.group_id IS NULL`,
		`pr.status = 'open'`,
		`pf.status = 'open'`,
	) + ` ORDER BY 5 DESC`
	rows, err := s.pool.Query(ctx, q, projectID)
	if err != nil {
		return nil, fmt.Errorf("incidentgroup: open out of group: %w", err)
	}
	return scanFeedItems(rows)
}

// ClosedSince — внегрупповые инциденты, закрытые не раньше since (§6.1/3,
// LIMIT с подписью — окно суток с потолком; члены закрытых групп показаны
// внутри свёрнутых групповых карточек и здесь не дублируются).
func (s *Store) ClosedSince(ctx context.Context, projectID int64, since time.Time, limit int) ([]FeedItem, error) {
	q := feedProjectQuery(
		`hi.status = 'resolved' AND hi.resolved_at >= $2 AND hi.group_id IS NULL`,
		`i.resolved_at IS NOT NULL AND i.resolved_at >= $2 AND i.group_id IS NULL`,
		`mi.status = 'resolved' AND mi.resolved_at >= $2 AND mi.group_id IS NULL`,
		`si.status = 'resolved' AND si.resolved_at >= $2 AND si.group_id IS NULL`,
		`pr.status = 'resolved' AND pr.resolved_at >= $2`,
		`pf.status = 'resolved' AND pf.resolved_at >= $2`,
	) + ` ORDER BY 6 DESC LIMIT $3`
	rows, err := s.pool.Query(ctx, q, projectID, since, limit)
	if err != nil {
		return nil, fmt.Errorf("incidentgroup: closed since: %w", err)
	}
	return scanFeedItems(rows)
}
