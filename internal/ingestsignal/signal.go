package ingestsignal

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Список ЗАКРЫТ в коде, а не CHECK-констрейнтом схемы — новый вид не должен
// требовать миграции.
type Kind string

const (
	KindDeprecatedLogs        Kind = "deprecated_logs"
	KindDeprecatedPprof       Kind = "deprecated_pprof"
	KindDeprecatedDeployments Kind = "deprecated_deployments"
	KindKeyInvalid            Kind = "key_invalid"
	KindKeyProjectMismatch    Kind = "key_project_mismatch"
	KindKeyScope              Kind = "key_scope"
)

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

// Проект, которого не существует, — молчаливый no-op без ошибки: projectID
// при KindKeyInvalid берётся из URL ДО того, как ключ хоть как-то проверен.
func (s *Store) Bump(ctx context.Context, projectID int64, kind Kind, hits int64, at time.Time) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO ingest_signals (project_id, kind, hits, last_seen_at)
		 SELECT $1, $2, $3, $4
		 WHERE EXISTS (SELECT 1 FROM projects WHERE id = $1)
		 ON CONFLICT (project_id, kind) DO UPDATE
		 SET hits = ingest_signals.hits + EXCLUDED.hits,
		     last_seen_at = GREATEST(ingest_signals.last_seen_at, EXCLUDED.last_seen_at)`,
		projectID, string(kind), hits, at)
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
