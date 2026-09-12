package incidentgroup

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Duck-typing — пакет не импортирует depsuppress.
type RootResolver interface {
	// Топовый упавший предок узла — или сам узел, если выше упавших нет.
	DownRoot(ctx context.Context, kind string, nodeID int64) (rootKind string, rootID int64, found bool, err error)
	// Сбрасывает кеш снимка — ретро-перебор должен видеть только что открытый корень.
	Invalidate()
}

// Единый предикат членства: DownRoot(узел) == корневой узел группы — путь по упавшим, не статика.
type Grouper struct {
	Pool  *pgxpool.Pool
	Store *Store
	Roots RootResolver
}

// found=false — гонка «корень закрылся между снимком DownRoot и этим запросом»: группу не создаём.
// Экспортирован: у Evaluator/Detector нет доступа к таблицам инцидентов чужого вида.
func (g *Grouper) RootIncident(ctx context.Context, rootKind string, rootID int64) (source string, incidentID, projectID int64, notified bool, found bool, err error) {
	switch rootKind {
	case "host":
		source = "host"
		err = g.Pool.QueryRow(ctx, `
			SELECT id, project_id, notified_open FROM host_incidents
			WHERE host_id = $1 AND kind = 'silent' AND status = 'open'`, rootID).
			Scan(&incidentID, &projectID, &notified)
	case "monitor":
		source = "uptime"
		err = g.Pool.QueryRow(ctx, `
			SELECT i.id, m.project_id, i.notified_open
			FROM incidents i JOIN monitors m ON m.id = i.monitor_id
			WHERE i.monitor_id = $1 AND i.resolved_at IS NULL`, rootID).
			Scan(&incidentID, &projectID, &notified)
	default:
		return "", 0, 0, false, false, fmt.Errorf("incidentgroup: unknown root node kind %q", rootKind)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return "", 0, 0, false, false, nil
	}
	if err != nil {
		return "", 0, 0, false, false, fmt.Errorf("incidentgroup: load root incident %s/%d: %w", rootKind, rootID, err)
	}
	return source, incidentID, projectID, notified, true, nil
}

// root.notified_open НА МОМЕНТ attach — член глушит уведомление только при attached && rootInforming.
// Лениво: MemberEligible — ДО EnsureGroup, иначе пустая группа виснет на карточке навсегда.
func (g *Grouper) Attach(ctx context.Context, source string, incidentID int64, nodeKind string, nodeID int64) (attached, rootInforming bool, err error) {
	rootKind, rootID, found, err := g.Roots.DownRoot(ctx, nodeKind, nodeID)
	if err != nil || !found {
		return false, false, err
	}
	rootSource, rootIncID, projectID, notified, ok, err := g.RootIncident(ctx, rootKind, rootID)
	if err != nil || !ok {
		return false, false, err
	}
	if rootSource == source && rootIncID == incidentID {
		return false, false, nil // сам корень — не член собственной группы
	}
	eligible, err := g.Store.MemberEligible(ctx, projectID, source, incidentID)
	if err != nil || !eligible {
		return false, false, err
	}
	grp, err := g.Store.EnsureGroup(ctx, projectID, rootSource, rootIncID, rootKind, rootID)
	if err != nil {
		return false, false, err
	}
	if grp.ResolvedAt != nil {
		return false, false, nil // группа уже закрыта — ведём себя как без группы
	}
	attached, err = g.Store.SetGroup(ctx, projectID, source, incidentID, grp.ID)
	if err != nil {
		return false, false, err
	}
	return attached, attached && notified, nil
}

// Узел резолвится по hosts.name = label_value того же проекта; кеш не нужен — событие редкое.
func (g *Grouper) AttachMetric(ctx context.Context, incidentID, projectID int64, hostName string) (attached, rootInforming bool, err error) {
	var hostID int64
	err = g.Pool.QueryRow(ctx,
		`SELECT id FROM hosts WHERE project_id = $1 AND name = $2`, projectID, hostName).
		Scan(&hostID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, false, nil // метка не указывает на известный хост проекта
	}
	if err != nil {
		return false, false, fmt.Errorf("incidentgroup: resolve host by name: %w", err)
	}
	return g.Attach(ctx, "metric", incidentID, "host", hostID)
}

type candidate struct {
	source     string
	incidentID int64
	nodeKind   string
	nodeID     int64
}

// Uptime отсутствует — членство у него только через хук MarkSuppressedByDep, не ретро-перебор.
// Побывавший в резолвнутой/удалённой группе — снова кандидат: не выпадает из перебора навсегда.
func (g *Grouper) openCandidates(ctx context.Context, projectID int64) ([]candidate, error) {
	rows, err := g.Pool.Query(ctx, `
		SELECT 'host'::text, hi.id, 'host'::text, hi.host_id
		FROM host_incidents hi
		LEFT JOIN incident_groups wg ON wg.id = hi.group_id
		WHERE hi.project_id = $1 AND hi.status = 'open'
		  AND (hi.group_id IS NULL OR wg.id IS NULL OR wg.resolved_at IS NOT NULL)
		UNION ALL
		SELECT 'metric', mi.id, 'host', h.id
		FROM metric_incidents mi
		JOIN metric_alert_rules r ON r.id = mi.rule_id AND r.label_key = 'host'
		JOIN hosts h ON h.project_id = mi.project_id AND h.name = r.label_value
		LEFT JOIN incident_groups wg ON wg.id = mi.group_id
		WHERE mi.project_id = $1 AND mi.status = 'open'
		  AND (mi.group_id IS NULL OR wg.id IS NULL OR wg.resolved_at IS NOT NULL)
		UNION ALL
		SELECT 'slo', si.id, 'monitor', s.monitor_id
		FROM slo_incidents si
		JOIN slos s ON s.id = si.slo_id AND s.sli_kind = 'uptime' AND s.monitor_id IS NOT NULL
		LEFT JOIN incident_groups wg ON wg.id = si.group_id
		WHERE si.project_id = $1 AND si.status = 'open'
		  AND (si.group_id IS NULL OR wg.id IS NULL OR wg.resolved_at IS NOT NULL)`,
		projectID)
	if err != nil {
		return nil, fmt.Errorf("incidentgroup: open candidates: %w", err)
	}
	defer rows.Close()
	var out []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.source, &c.incidentID, &c.nodeKind, &c.nodeID); err != nil {
			return nil, fmt.Errorf("incidentgroup: scan candidate: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("incidentgroup: open candidates: %w", err)
	}
	return out, nil
}

// Их уведомления УЖЕ ушли — notified не трогается, присоединение чисто для состава.
// Перебор — DownRoot по узлу каждого кандидата, не обход рёбер: PreviewSuppression одноуровнев.
func (g *Grouper) OnRootOpened(ctx context.Context, rootSource string, rootIncidentID int64, rootNodeKind string, rootNodeID, projectID int64) error {
	// Снимок кеширован 5с — сбрасываем, иначе перебор молча пропустит только что открывшийся корень.
	g.Roots.Invalidate()
	cands, err := g.openCandidates(ctx, projectID)
	if err != nil {
		return err
	}
	var grp *Group
	for _, c := range cands {
		if c.source == rootSource && c.incidentID == rootIncidentID {
			continue
		}
		rk, rid, found, err := g.Roots.DownRoot(ctx, c.nodeKind, c.nodeID)
		if err != nil {
			return err
		}
		if !found || rk != rootNodeKind || rid != rootNodeID {
			continue
		}
		if grp == nil {
			gg, err := g.Store.EnsureGroup(ctx, projectID, rootSource, rootIncidentID, rootNodeKind, rootNodeID)
			if err != nil {
				return err
			}
			if gg.ResolvedAt != nil {
				return nil // корень успел закрыться — sweep уже закрыл группу
			}
			grp = &gg
		}
		if _, err := g.Store.SetGroup(ctx, projectID, c.source, c.incidentID, grp.ID); err != nil {
			return err
		}
	}
	return nil
}

// Отсутствие группы — не ошибка: членов не было, группа не создавалась.
func (g *Grouper) OnRootClosed(ctx context.Context, rootSource string, rootIncidentID int64) error {
	_, err := g.Store.Resolve(ctx, rootSource, rootIncidentID)
	return err
}
