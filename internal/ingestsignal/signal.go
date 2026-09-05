// Package ingestsignal — per-project учёт сигналов приёма, которые раньше
// были видны только процесс-локальными self-метриками без метки проекта
// (аудит перед 1.0, находки K7-5/K7-6): отказ по ключу (missing/invalid,
// чужой проект, запрещённый скоуп) и попадание на устаревший путь приёма
// (см. internal/ingest/deprecated.go). Store — тонкая обёртка над таблицей
// ingest_signals (project_id, kind) → (hits, last_seen_at); запись в неё
// ведёт Recorder (recorder.go), а не сам приём — путь неаутентифицированный,
// и писать в PG на каждый запрос значило бы дать атакующему усилитель
// нагрузки.
package ingestsignal

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Kind — вид сигнала приёма. Список ЗАКРЫТ в коде, а не CHECK-констрейнтом
// схемы (см. 0091_ingest_signals.up.sql) — новый вид не должен требовать
// миграции.
type Kind string

const (
	// KindDeprecatedLogs — запрос пришёл на устаревший алиас /logs.
	KindDeprecatedLogs Kind = "deprecated_logs"
	// KindDeprecatedPprof — запрос пришёл на устаревший алиас /profiles/pprof.
	KindDeprecatedPprof Kind = "deprecated_pprof"
	// KindDeprecatedDeployments — запрос пришёл на устаревший алиас деплоев
	// (/api/{project}/deployments/).
	KindDeprecatedDeployments Kind = "deprecated_deployments"
	// KindKeyInvalid — sentry_key отсутствует, не резолвится ни в один
	// проект, либо отозван.
	KindKeyInvalid Kind = "key_invalid"
	// KindKeyProjectMismatch — sentry_key резолвится, но в чужой проект
	// относительно project id из пути запроса.
	KindKeyProjectMismatch Kind = "key_project_mismatch"
	// KindKeyScope — ключ резолвится и принадлежит проекту, но его тип не
	// допущен к этому сигналу телеметрии.
	KindKeyScope Kind = "key_scope"
)

// Signal — одна строка ingest_signals: агрегат (project_id, kind).
type Signal struct {
	ProjectID  int64
	Kind       Kind
	Hits       int64
	LastSeenAt time.Time
}

// Store — CRUD поверх таблицы ingest_signals.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore создаёт Store поверх пула pool.
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// Bump прибавляет hits к счётчику (projectID, kind) и продвигает
// last_seen_at до at, если at позже уже сохранённого значения. Проект,
// которого не существует, — молчаливый no-op без ошибки: projectID при
// KindKeyInvalid берётся из URL запроса ДО того, как ключ (и, значит,
// существование проекта) хоть как-то проверен, и перебор случайных id в
// пути не должен ни падать, ни копить мусорные строки в ingest_signals.
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

// ForProject возвращает все сигналы проекта, отсортированные по kind.
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
