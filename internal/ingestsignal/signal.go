package ingestsignal

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Список ЗАКРЫТ в коде. Переименование существующего вида требует миграции
// данных — старые строки уже лежат в ingest_signals.kind, добавление нового — нет.
type Kind string

const (
	KindDeprecatedLogs        Kind = "deprecated_logs"
	KindDeprecatedPprof       Kind = "deprecated_pprof"
	KindDeprecatedDeployments Kind = "deprecated_deployments"
	KindKeyInvalid            Kind = "key_invalid"
	KindKeyProjectMismatch    Kind = "key_project_mismatch"
	KindKeyScope              Kind = "key_scope"
)

// Держать <= окна показа вида (web/issues.go — 1ч, web/projsettings.go — 7д).
var resetWindow = map[Kind]time.Duration{
	KindKeyInvalid:            time.Hour,
	KindKeyProjectMismatch:    time.Hour,
	KindKeyScope:              time.Hour,
	KindDeprecatedLogs:        7 * 24 * time.Hour,
	KindDeprecatedPprof:       7 * 24 * time.Hour,
	KindDeprecatedDeployments: 7 * 24 * time.Hour,
}

// «Без сброса» — больше любого реального resetWindow.
const ttlIndefinite = 100 * 365 * 24 * time.Hour

type Signal struct {
	ProjectID  int64
	Kind       Kind
	Hits       int64
	LastSeenAt time.Time
}

type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// projectID при KindKeyInvalid не проверен — неизвестный проект не пишется.
// Разрыв с last_seen_at больше resetWindow[kind] — hits стартует заново.
func (s *Store) Bump(ctx context.Context, projectID int64, kind Kind, hits int64, at time.Time) error {
	window := resetWindow[kind]
	if window <= 0 {
		window = ttlIndefinite
	}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO ingest_signals (project_id, kind, hits, last_seen_at)
		 SELECT $1, $2, $3, $4
		 WHERE EXISTS (SELECT 1 FROM projects WHERE id = $1)
		 ON CONFLICT (project_id, kind) DO UPDATE
		 SET hits = CASE
		         WHEN ingest_signals.last_seen_at < $4 - ($5 * interval '1 second')
		         THEN EXCLUDED.hits
		         ELSE ingest_signals.hits + EXCLUDED.hits
		     END,
		     last_seen_at = GREATEST(ingest_signals.last_seen_at, EXCLUDED.last_seen_at)`,
		projectID, string(kind), hits, at, window.Seconds())
	if err != nil {
		return fmt.Errorf("ingestsignal: bump: %w", err)
	}
	return nil
}

func (s *Store) ForProject(ctx context.Context, projectID int64) ([]Signal, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT project_id, kind, hits, last_seen_at FROM ingest_signals
		 WHERE project_id = $1 ORDER BY kind`,
		projectID)
	if err != nil {
		return nil, fmt.Errorf("ingestsignal: for project: %w", err)
	}
	defer rows.Close()

	var out []Signal
	for rows.Next() {
		var sig Signal
		var kind string
		if err := rows.Scan(&sig.ProjectID, &kind, &sig.Hits, &sig.LastSeenAt); err != nil {
			return nil, fmt.Errorf("ingestsignal: for project: scan: %w", err)
		}
		sig.Kind = Kind(kind)
		out = append(out, sig)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ingestsignal: for project: rows: %w", err)
	}
	return out, nil
}
