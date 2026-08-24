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
