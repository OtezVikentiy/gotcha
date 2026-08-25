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

// sourceProjectCond — предикат «инцидент источника принадлежит project_id
// $2» (W6): у host/metric/slo есть своя колонка project_id, у uptime
// (таблица `incidents`) её нет — фильтр идёт через monitors, тем же путём,
// что и в feedProjectQuery.
var sourceProjectCond = map[string]string{
	"host":   `x.project_id = $2`,
	"uptime": `EXISTS (SELECT 1 FROM monitors mm WHERE mm.id = x.monitor_id AND mm.project_id = $2)`,
	"metric": `x.project_id = $2`,
	"slo":    `x.project_id = $2`,
}

// SetGroup присоединяет инцидент к группе, если он ещё не член ОТКРЫТОЙ
// группы (W1/W2): первое присоединение выигрывает; инцидент, чей group_id
// указывает на группу, которая уже резолвнута или удалена (janitor purge),
// присоединяется заново — та же трактовка, что в гейтах уведомлений
// (host/incident.go, metric/incident.go, slo/store.go). project_id в WHERE
// (W6) — защита от кросс-проектной записи прямо в запросе, не только на
// инвариантах вызывающих. Возвращает true, если присоединение реально
// состоялось (RowsAffected > 0) — Attach решает по этому факту, создавать
// ли пустую группу (W4).
func (s *Store) SetGroup(ctx context.Context, projectID int64, source string, incidentID, groupID int64) (bool, error) {
	table, ok := sourceTables[source]
	if !ok {
		return false, fmt.Errorf("incidentgroup: set group: unknown source %q", source)
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE `+table+` x SET group_id = $3
		WHERE x.id = $1 AND `+sourceProjectCond[source]+`
		  AND (x.group_id IS NULL OR NOT EXISTS (
		        SELECT 1 FROM incident_groups g WHERE g.id = x.group_id AND g.resolved_at IS NULL))`,
		incidentID, projectID, groupID)
	if err != nil {
		return false, fmt.Errorf("incidentgroup: set group %s/%d: %w", source, incidentID, err)
	}
	return tag.RowsAffected() > 0, nil
}

// MemberEligible — true, если инцидент источника ЕЩЁ не член ОТКРЫТОЙ
// группы (тот же предикат, что и в SetGROUP). Grouper.Attach зовёт его ДО
// EnsureGroup (W4): если присоединение заведомо не состоится, группа нового
// корня не создаётся — иначе на карточке повисает пустая группа
// («host 0 · uptime 0 · metric 0 · slo 0»), которую sweep не тронет (корень
// открыт). Несуществующий (чужой проект/удалённый) инцидент — не
// присоединяем.
func (s *Store) MemberEligible(ctx context.Context, projectID int64, source string, incidentID int64) (bool, error) {
	table, ok := sourceTables[source]
	if !ok {
		return false, fmt.Errorf("incidentgroup: member eligible: unknown source %q", source)
	}
	var eligible bool
	err := s.pool.QueryRow(ctx, `
		SELECT x.group_id IS NULL OR NOT EXISTS (
		        SELECT 1 FROM incident_groups g WHERE g.id = x.group_id AND g.resolved_at IS NULL)
		FROM `+table+` x WHERE x.id = $1 AND `+sourceProjectCond[source],
		incidentID, projectID).Scan(&eligible)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("incidentgroup: member eligible %s/%d: %w", source, incidentID, err)
	}
	return eligible, nil
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
	// FormerGroupID/FormerGroupRootName — данные для бейджа «был в группе»
	// (W1, feed.badge.was_grouped — сам бейдж рисует R5): непустые только у
	// строк OpenOutOfGroup/ClosedSince, чей group_id указывает на группу,
	// которая уже резолвнута; group_id, указывающий на удалённую (purge)
	// группу, даёт FormerGroupID=0 — сведений о ней не осталось нигде.
	FormerGroupID       int64
	FormerGroupRootName string
}

func scanFeedItems(rows pgx.Rows) ([]FeedItem, error) {
	defer rows.Close()
	var out []FeedItem
	for rows.Next() {
		var it FeedItem
		if err := rows.Scan(&it.Source, &it.IncidentID, &it.Title, &it.SubKind,
			&it.StartedAt, &it.ResolvedAt, &it.Severity, &it.Acknowledged,
			&it.SuppressedByDep, &it.RefID, &it.RefName,
			&it.FormerGroupID, &it.FormerGroupRootName); err != nil {
			return nil, fmt.Errorf("incidentgroup: scan feed item: %w", err)
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// feedMemberSelect — 4 источника-члена по group_id (состав группы, Р6:
// выборка по колонке, без таблицы членов). LEFT JOIN + COALESCE — исчезнувший
// справочник (правило/SLO удалены) не прячет члена из состава. project_id в
// WHERE (W6, тем же путём, что sourceProjectCond у SetGroup — у `incidents`
// своей колонки нет, фильтр через monitors) — защита от кросс-проектной
// выдачи прямо в запросе. Два последних столбца — заглушки под FormerGroup*
// (у члена ТЕКУЩЕЙ группы бывшей группы, по определению, нет).
const feedMemberSelect = `
	SELECT 'host'::text, hi.id, COALESCE(h.name,''), hi.kind,
	       hi.started_at, hi.resolved_at, hi.severity,
	       hi.acknowledged_at IS NOT NULL, hi.suppressed_by_dep, 0::bigint, COALESCE(h.name,''),
	       0::bigint, ''::text
	FROM host_incidents hi LEFT JOIN hosts h ON h.id = hi.host_id
	WHERE hi.group_id = $1 AND hi.project_id = $2
	UNION ALL
	SELECT 'uptime', i.id, COALESCE(m.name,''), '',
	       i.started_at, i.resolved_at, '',
	       false, i.suppressed_by_dep, i.monitor_id, '',
	       0::bigint, ''::text
	FROM incidents i LEFT JOIN monitors m ON m.id = i.monitor_id
	WHERE i.group_id = $1 AND m.project_id = $2
	UNION ALL
	SELECT 'metric', mi.id, COALESCE(r.metric_name,''), '',
	       mi.started_at, mi.resolved_at, mi.severity,
	       mi.acknowledged_at IS NOT NULL, false, mi.rule_id, '',
	       0::bigint, ''::text
	FROM metric_incidents mi LEFT JOIN metric_alert_rules r ON r.id = mi.rule_id
	WHERE mi.group_id = $1 AND mi.project_id = $2
	UNION ALL
	SELECT 'slo', si.id, COALESCE(sl.name,''), '',
	       si.started_at, si.resolved_at, si.severity,
	       si.acknowledged_at IS NOT NULL, false, si.slo_id, '',
	       0::bigint, ''::text
	FROM slo_incidents si LEFT JOIN slos sl ON sl.id = si.slo_id
	WHERE si.group_id = $1 AND si.project_id = $2`

// Composition — состав группы: члены 4 источников, старейшие первыми.
// projectID (W6) — вторая линия защиты поверх group_id: группа чужого
// проекта отдаст пустой список, а не чужие инциденты.
func (s *Store) Composition(ctx context.Context, projectID, groupID int64) ([]FeedItem, error) {
	rows, err := s.pool.Query(ctx, feedMemberSelect+` ORDER BY 5`, groupID, projectID)
	if err != nil {
		return nil, fmt.Errorf("incidentgroup: composition: %w", err)
	}
	return scanFeedItems(rows)
}

// notOpenGroupMember — «инцидент не член ОТКРЫТОЙ группы» (W1/W2): group_id
// колонки alias NULL, либо LEFT JOIN на incident_groups под алиасом wg не
// нашёл строку (группа удалена janitor'ом), либо нашёл резолвнутую. Та же
// трактовка, что в гейтах уведомлений (host/incident.go, metric/incident.go,
// slo/store.go), перенесённая на LEFT JOIN, потому что тем же wg отдаём
// FormerGroup* ниже. alias — hi/i/mi/si (те же, что в hostNotRoot и др.).
func notOpenGroupMember(alias string) string {
	return `(` + alias + `.group_id IS NULL OR wg.id IS NULL OR wg.resolved_at IS NOT NULL)`
}

// feedProjectQuery — 6 источников по проекту; условия статуса подставляются
// готовыми строками-константами этого файла (не пользовательский ввод —
// конкатенация безопасна). $1 — project_id; условия ClosedSince
// дополнительно ссылаются на $2 (since), LIMIT — $3.
// host/uptime/metric/slo дополнительно LEFT JOIN'ят incident_groups (wg) —
// одним и тем же приёмом решают два вопроса: гейт notOpenGroupMember (W1/W2,
// применяется всегда, вне зависимости от cond) и данные бывшей группы для
// бейджа feed.badge.was_grouped (W1) — id + имя корня, восстановленное тем
// же способом, что groupRowSelect. trace/profile group_id не имеют
// (Р2 — у них нет узла) — вне групп всегда, заглушки под FormerGroup*.
// MINOR-10: все ветки ходят по project_id + status/resolved_at — у
// host/metric/slo есть индексы по project_id (см. существующие
// ListByProject), incident_groups_open_idx частичный по project_id;
// таблицы малы, отдельных индексов под ленту не заводим.
func feedProjectQuery(hostCond, uptimeCond, metricCond, sloCond, traceCond, profileCond string) string {
	return `
	SELECT 'host'::text, hi.id, COALESCE(h.name,''), hi.kind,
	       hi.started_at, hi.resolved_at, hi.severity,
	       hi.acknowledged_at IS NOT NULL, hi.suppressed_by_dep, 0::bigint, COALESCE(h.name,''),
	       COALESCE(wg.id, 0::bigint), COALESCE(wgh.name, wgm.name, '')
	FROM host_incidents hi
	LEFT JOIN hosts h ON h.id = hi.host_id
	LEFT JOIN incident_groups wg ON wg.id = hi.group_id
	LEFT JOIN hosts wgh ON wg.root_node_kind = 'host' AND wgh.id = wg.root_node_id
	LEFT JOIN monitors wgm ON wg.root_node_kind = 'monitor' AND wgm.id = wg.root_node_id
	WHERE hi.project_id = $1 AND ` + notOpenGroupMember("hi") + ` AND ` + hostCond + `
	UNION ALL
	SELECT 'uptime', i.id, COALESCE(m.name,''), '',
	       i.started_at, i.resolved_at, '',
	       false, i.suppressed_by_dep, i.monitor_id, '',
	       COALESCE(wg.id, 0::bigint), COALESCE(wgh.name, wgm.name, '')
	FROM incidents i
	JOIN monitors m ON m.id = i.monitor_id
	LEFT JOIN incident_groups wg ON wg.id = i.group_id
	LEFT JOIN hosts wgh ON wg.root_node_kind = 'host' AND wgh.id = wg.root_node_id
	LEFT JOIN monitors wgm ON wg.root_node_kind = 'monitor' AND wgm.id = wg.root_node_id
	WHERE m.project_id = $1 AND ` + notOpenGroupMember("i") + ` AND ` + uptimeCond + `
	UNION ALL
	SELECT 'metric', mi.id, COALESCE(r.metric_name,''), '',
	       mi.started_at, mi.resolved_at, mi.severity,
	       mi.acknowledged_at IS NOT NULL, false, mi.rule_id, '',
	       COALESCE(wg.id, 0::bigint), COALESCE(wgh.name, wgm.name, '')
	FROM metric_incidents mi
	LEFT JOIN metric_alert_rules r ON r.id = mi.rule_id
	LEFT JOIN incident_groups wg ON wg.id = mi.group_id
	LEFT JOIN hosts wgh ON wg.root_node_kind = 'host' AND wgh.id = wg.root_node_id
	LEFT JOIN monitors wgm ON wg.root_node_kind = 'monitor' AND wgm.id = wg.root_node_id
	WHERE mi.project_id = $1 AND ` + notOpenGroupMember("mi") + ` AND ` + metricCond + `
	UNION ALL
	SELECT 'slo', si.id, COALESCE(sl.name,''), '',
	       si.started_at, si.resolved_at, si.severity,
	       si.acknowledged_at IS NOT NULL, false, si.slo_id, '',
	       COALESCE(wg.id, 0::bigint), COALESCE(wgh.name, wgm.name, '')
	FROM slo_incidents si
	LEFT JOIN slos sl ON sl.id = si.slo_id
	LEFT JOIN incident_groups wg ON wg.id = si.group_id
	LEFT JOIN hosts wgh ON wg.root_node_kind = 'host' AND wgh.id = wg.root_node_id
	LEFT JOIN monitors wgm ON wg.root_node_kind = 'monitor' AND wgm.id = wg.root_node_id
	WHERE si.project_id = $1 AND ` + notOpenGroupMember("si") + ` AND ` + sloCond + `
	UNION ALL
	SELECT 'trace', pr.id, pr.target, pr.metric,
	       pr.started_at, pr.resolved_at, pr.severity,
	       pr.acknowledged_at IS NOT NULL, false, pr.id, '',
	       0::bigint, ''::text
	FROM perf_regressions pr
	WHERE pr.project_id = $1 AND ` + traceCond + `
	UNION ALL
	SELECT 'profile', pf.id, pf.function, pf.profile_type,
	       pf.started_at, pf.resolved_at, pf.severity,
	       pf.acknowledged_at IS NOT NULL, false, pf.id, '',
	       0::bigint, ''::text
	FROM profile_regressions pf
	WHERE pf.project_id = $1 AND ` + profileCond
}

// hostNotRoot/uptimeNotRoot — «инцидент не является корнем группы».
// Корню group_id намеренно не проставляется (Grouper.Attach: корень не член
// собственной группы), поэтому по одному `group_id IS NULL` он попадал бы и в
// шапку карточки группы, и во «Вне групп» — один инцидент двумя строками.
// Корнями бывают только host- и uptime-инциденты (rootIncident, §4.1),
// остальным источникам условие не нужно. Отбор идёт по уникальному индексу
// (root_source, root_incident_id); группа, удалённая как осиротевшая
// (janitor purge), корень снова показывает — это верно, карточки уже нет.
const (
	hostNotRoot   = `NOT EXISTS (SELECT 1 FROM incident_groups g WHERE g.project_id = $1 AND g.root_source = 'host' AND g.root_incident_id = hi.id)`
	uptimeNotRoot = `NOT EXISTS (SELECT 1 FROM incident_groups g WHERE g.project_id = $1 AND g.root_source = 'uptime' AND g.root_incident_id = i.id)`
)

// OpenOutOfGroup — открытые ВНЕгрупповые инциденты всех 6 источников
// (§6.1/2): trace/profile всегда вне групп (Р2 — узла нет). «Внегрупповой»
// (W1) — не член ОТКРЫТОЙ группы (гейт notOpenGroupMember, вшит в
// feedProjectQuery): открытый член резолвнутой или удалённой группы отсюда
// не прячется — он «открытая работа» (сюда) и одновременно «упало вместе с
// этим» (в свёрнутой карточке группы, Composition), это не дубль.
func (s *Store) OpenOutOfGroup(ctx context.Context, projectID int64) ([]FeedItem, error) {
	q := feedProjectQuery(
		`hi.status = 'open' AND `+hostNotRoot,
		`i.resolved_at IS NULL AND `+uptimeNotRoot,
		`mi.status = 'open'`,
		`si.status = 'open'`,
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
// LIMIT с подписью — окно суток с потолком). Член ОТКРЫТОЙ группы сюда не
// попадает (показан внутри её карточки, Composition, и там же появится
// после закрытия). Закрывшийся член группы, которая САМА уже резолвнута или
// удалена (W1), — попадает: это симметрично OpenOutOfGroup, дубль с
// (свёрнутой) карточкой закрытой группы намеренный, те же два разных
// смысла.
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
