// Package issue — группы ошибок: upsert-группировка по fingerprint
// и жизненный цикл unresolved/resolved/ignored.
package issue

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Issue struct {
	ID          int64
	ProjectID   int64
	Fingerprint string
	Title       string
	Culprit     string
	Level       string
	Status      string
	FirstSeen   time.Time
	LastSeen    time.Time
	TimesSeen   int64
	AssigneeID  *int64
}

// UpsertResult — что произошло с группой при поступлении события.
type UpsertResult struct {
	IssueID    int64
	New        bool // группа создана этим событием
	Regression bool // группа была resolved и переоткрыта
}

type Service struct {
	pool *pgxpool.Pool
}

func NewService(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool}
}

// Upsert регистрирует событие в группе. Новый fingerprint создаёт issue;
// существующий обновляет last_seen/times_seen/title/level; resolved
// переоткрывается (регрессия), ignored остаётся ignored.
//
// Гонка двух первых событий одного fingerprint: обе стороны могут получить
// New=true (CTE old видит снимок до вставки). Редко и безвредно —
// дедупликацию алертов делает троттлинг (план 6).
func (s *Service) Upsert(ctx context.Context, projectID int64, fingerprint, title, culprit, level string, seenAt time.Time) (UpsertResult, error) {
	const q = `
WITH old AS (
    SELECT status FROM issues WHERE project_id = $1 AND fingerprint = $2
), up AS (
    INSERT INTO issues (project_id, fingerprint, title, culprit, level, first_seen, last_seen)
    VALUES ($1, $2, $3, $4, $5, $6, $6)
    ON CONFLICT (project_id, fingerprint) DO UPDATE SET
        title      = EXCLUDED.title,
        culprit    = EXCLUDED.culprit,
        level      = EXCLUDED.level,
        last_seen  = GREATEST(issues.last_seen, EXCLUDED.last_seen),
        times_seen = issues.times_seen + 1,
        status     = CASE WHEN issues.status = 'resolved' THEN 'unresolved' ELSE issues.status END
    RETURNING id
)
SELECT up.id,
       old.status IS NULL                        AS is_new,
       coalesce(old.status = 'resolved', false)  AS regression
FROM up LEFT JOIN old ON true`

	var r UpsertResult
	err := s.pool.QueryRow(ctx, q, projectID, fingerprint, title, culprit, level, seenAt).
		Scan(&r.IssueID, &r.New, &r.Regression)
	if err != nil {
		return UpsertResult{}, fmt.Errorf("issue: upsert: %w", err)
	}
	return r, nil
}
