package uptime

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// State — состояние проверки монитора в одном регионе (monitor_state).
type State struct {
	MonitorID        int64
	Region           string
	Status           string
	ConsecutiveFails int
	ConsecutiveOKs   int
	LastCheckedAt    *time.Time
	LastError        string
}

// States возвращает состояния монитора по всем регионам, отсортированные
// по region.
func (s *Service) States(ctx context.Context, monitorID int64) ([]State, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT monitor_id, region, status, consecutive_fails, consecutive_oks, last_checked_at, last_error
		FROM monitor_state WHERE monitor_id = $1 ORDER BY region`, monitorID)
	if err != nil {
		return nil, fmt.Errorf("uptime: states: %w", err)
	}
	defer rows.Close()
	var out []State
	for rows.Next() {
		var st State
		if err := rows.Scan(&st.MonitorID, &st.Region, &st.Status, &st.ConsecutiveFails,
			&st.ConsecutiveOKs, &st.LastCheckedAt, &st.LastError); err != nil {
			return nil, fmt.Errorf("uptime: states: %w", err)
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// ApplyResult records one check result for (monitorID, region) and
// recomputes status from the monitor's fail_threshold/recovery_threshold —
// all in a single INSERT ... ON CONFLICT DO UPDATE statement, so the
// increment-then-recompute is atomic even under concurrent probes hitting
// the same region.
//
// This must be a single INSERT ... ON CONFLICT, not "upsert in one CTE,
// then UPDATE ... FROM that CTE in a second step": a data-modifying CTE and
// a subsequent statement in the same WITH-query each see the table as it
// was at the start of the query, so a follow-up UPDATE cannot see a row the
// CTE just inserted (RETURNING is the only way data crosses between
// sibling/parent statements). Folding the recompute into the single
// INSERT's own VALUES (fresh row) and ON CONFLICT DO UPDATE SET (existing
// row) branches avoids that trap entirely.
//
// A failure bumps consecutive_fails and resets consecutive_oks to 0 (and
// vice-versa for a success); status flips to 'down' once consecutive_fails
// reaches fail_threshold, or to 'up' once consecutive_oks reaches
// recovery_threshold. A partial series (below either threshold) leaves
// status unchanged — e.g. two fails then one success resets the fail
// streak without ever having gone down. An unknown monitorID makes the
// thresholds CTE empty, so the INSERT ... SELECT ... FROM thresholds
// produces no row and RETURNING yields ErrInvalidMonitor rather than a raw
// FK violation.
func (s *Service) ApplyResult(ctx context.Context, monitorID int64, region string, ok bool, errText string, at time.Time) (State, error) {
	var st State
	err := s.pool.QueryRow(ctx, `
		WITH thresholds AS (
			SELECT fail_threshold, recovery_threshold FROM monitors WHERE id = $1
		)
		INSERT INTO monitor_state (monitor_id, region, status, consecutive_fails, consecutive_oks, last_checked_at, last_error)
		SELECT $1, $2,
			CASE
				WHEN NOT $3 AND 1 >= fail_threshold THEN 'down'
				WHEN $3 AND 1 >= recovery_threshold THEN 'up'
				ELSE 'unknown'
			END,
			CASE WHEN $3 THEN 0 ELSE 1 END,
			CASE WHEN $3 THEN 1 ELSE 0 END,
			$5, $4
		FROM thresholds
		ON CONFLICT (monitor_id, region) DO UPDATE SET
			consecutive_fails = CASE WHEN $3 THEN 0 ELSE monitor_state.consecutive_fails + 1 END,
			consecutive_oks   = CASE WHEN $3 THEN monitor_state.consecutive_oks + 1 ELSE 0 END,
			last_checked_at   = $5,
			last_error        = $4,
			status = CASE
				WHEN NOT $3 AND (monitor_state.consecutive_fails + 1) >= (SELECT fail_threshold FROM thresholds) THEN 'down'
				WHEN $3 AND (monitor_state.consecutive_oks + 1) >= (SELECT recovery_threshold FROM thresholds) THEN 'up'
				ELSE monitor_state.status
			END
		RETURNING monitor_id, region, status, consecutive_fails, consecutive_oks, last_checked_at, last_error`,
		monitorID, region, ok, errText, at,
	).Scan(&st.MonitorID, &st.Region, &st.Status, &st.ConsecutiveFails, &st.ConsecutiveOKs,
		&st.LastCheckedAt, &st.LastError)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return State{}, ErrInvalidMonitor
		}
		return State{}, fmt.Errorf("uptime: apply result: %w", err)
	}
	return st, nil
}
