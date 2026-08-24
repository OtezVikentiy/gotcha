package incidentgroup

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// RootResolver — подмножество depsuppress.Suppressor, нужное Grouper'у:
// duck-typing (паттерн MaintenanceChecker/depChecker), пакет не импортирует
// depsuppress.
type RootResolver interface {
	// DownRoot — топовый упавший предок узла (или сам узел, если он упал и
	// выше упавших нет); см. depsuppress.Suppressor.DownRoot.
	DownRoot(ctx context.Context, kind string, nodeID int64) (rootKind string, rootID int64, found bool, err error)
	// Invalidate сбрасывает кеш снимка — ретро-перебор должен видеть только
	// что открытый корень.
	Invalidate()
}

// Grouper — сервис-резолвер членства: по узлу инцидента находит корневую
// группу (единый предикат Р3: DownRoot(узел) == корневой узел группы —
// путь по упавшим, не статическое поддерево рёбер).
type Grouper struct {
	Pool  *pgxpool.Pool
	Store *Store
	Roots RootResolver
}

// rootIncident — открытый инцидент недоступности узла-корня: host →
// host_incidents kind='silent', monitor → uptime incidents. found=false —
// гонка «корень закрылся между снимком DownRoot и этим запросом» (группу
// тогда не создаём: sweep, §4.4, закрыл бы её тут же).
func (g *Grouper) rootIncident(ctx context.Context, rootKind string, rootID int64) (source string, incidentID, projectID int64, notified bool, found bool, err error) {
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

// Attach присоединяет инцидент (source, incidentID) с узлом (nodeKind,
// nodeID) к группе его down-корня, если корень есть и это не сам инцидент.
// rootInforming — гейт «информирующего корня» (Р4, MAJOR-3):
// root.notified_open НА МОМЕНТ attach; глушить собственное open-уведомление
// член может ТОЛЬКО при attached && rootInforming. Немые корни (открытый в
// maintenance uptime-корень, B5-подавленный host-корень, корень в грейсе
// B5) дают attach «только для состава» — член уведомляет сам (fail-noisy).
// Группа, успевшая закрыться, — attach не делается вовсе.
func (g *Grouper) Attach(ctx context.Context, source string, incidentID int64, nodeKind string, nodeID int64) (attached, rootInforming bool, err error) {
	rootKind, rootID, found, err := g.Roots.DownRoot(ctx, nodeKind, nodeID)
	if err != nil || !found {
		return false, false, err
	}
	rootSource, rootIncID, projectID, notified, ok, err := g.rootIncident(ctx, rootKind, rootID)
	if err != nil || !ok {
		return false, false, err
	}
	if rootSource == source && rootIncID == incidentID {
		return false, false, nil // сам корень — не член собственной группы
	}
	grp, err := g.Store.EnsureGroup(ctx, projectID, rootSource, rootIncID, rootKind, rootID)
	if err != nil {
		return false, false, err
	}
	if grp.ResolvedAt != nil {
		return false, false, nil // группа уже закрыта — ведём себя как без группы
	}
	if err := g.Store.SetGroup(ctx, source, incidentID, grp.ID); err != nil {
		return false, false, err
	}
	return true, notified, nil
}

// AttachMetric — Attach для metric-инцидента правила с label_key='host':
// узел резолвится по hosts.name = label_value ТОГО ЖЕ проекта (Р1). Резолв
// зовётся только при создании инцидента (редкое событие), поэтому прямой
// индексированный запрос вместо кеша-на-тик — кешировать нечего.
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

// candidate — открытый инцидент проекта с резолвнутым узлом: кандидат
// ретро-присоединения (Р7).
type candidate struct {
	source     string
	incidentID int64
	nodeKind   string
	nodeID     int64
}

// openCandidates — открытые ВНЕгрупповые инциденты проекта источников
// host/metric/slo с их узлами. Uptime намеренно отсутствует (MAJOR-4):
// uptime-членство возникает только через хук MarkSuppressedByDep. Metric —
// только правила label_key='host' с хостом того же проекта; slo — только
// sli_kind='uptime' с monitor_id (Р1). Ретро по env/role-селекторам метрик
// не делается (осознанно отложено, §10).
func (g *Grouper) openCandidates(ctx context.Context, projectID int64) ([]candidate, error) {
	rows, err := g.Pool.Query(ctx, `
		SELECT 'host'::text, hi.id, 'host'::text, hi.host_id
		FROM host_incidents hi
		WHERE hi.project_id = $1 AND hi.status = 'open' AND hi.group_id IS NULL
		UNION ALL
		SELECT 'metric', mi.id, 'host', h.id
		FROM metric_incidents mi
		JOIN metric_alert_rules r ON r.id = mi.rule_id AND r.label_key = 'host'
		JOIN hosts h ON h.project_id = mi.project_id AND h.name = r.label_value
		WHERE mi.project_id = $1 AND mi.status = 'open' AND mi.group_id IS NULL
		UNION ALL
		SELECT 'slo', si.id, 'monitor', s.monitor_id
		FROM slo_incidents si
		JOIN slos s ON s.id = si.slo_id AND s.sli_kind = 'uptime' AND s.monitor_id IS NOT NULL
		WHERE si.project_id = $1 AND si.status = 'open' AND si.group_id IS NULL`,
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

// OnRootOpened — ретро-присоединение (Р7): при открытии корневого инцидента
// уже открытые инциденты проекта, чей узел проходит DownRoot == корень,
// присоединяются задним числом — их уведомления УЖЕ ушли, notified-статус
// не трогается, присоединение чисто для состава. Группа создаётся ЛЕНИВО:
// нет членов — нет группы (EnsureGroup идемпотентен, первый будущий член
// создаст её сам через Attach). Перебор — прогон DownRoot по узлу каждого
// кандидата (MAJOR-6: НЕ обход рёбер вниз — PreviewSuppression одноуровнев
// и не знает состояния промежуточных узлов).
func (g *Grouper) OnRootOpened(ctx context.Context, rootSource string, rootIncidentID int64, rootNodeKind string, rootNodeID, projectID int64) error {
	// Снимок depsuppress может не знать о только что открытом корне (кеш
	// 5с) — сбрасываем, иначе перебор молча пропустит всех членов.
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
		if err := g.Store.SetGroup(ctx, c.source, c.incidentID, grp.ID); err != nil {
			return err
		}
	}
	return nil
}

// OnRootClosed закрывает группу корневого инцидента (Р5). Отсутствие
// группы — не ошибка (группа могла не создаться: членов не было).
func (g *Grouper) OnRootClosed(ctx context.Context, rootSource string, rootIncidentID int64) error {
	_, err := g.Store.Resolve(ctx, rootSource, rootIncidentID)
	return err
}
