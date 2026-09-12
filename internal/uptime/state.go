package uptime

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

type State struct {
	MonitorID        int64
	Region           string
	Status           string
	ConsecutiveFails int
	ConsecutiveOKs   int
	LastCheckedAt    *time.Time
	LastError        string
}

func (s *Service) States(ctx context.Context, monitorID int64) ([]State, error) {
	// JOIN отсекает состояния регионов, которых у монитора больше нет — иначе
	// сирота в «down» держит монитор красным навсегда, JOIN лечит и старые сироты без миграции.
	rows, err := s.pool.Query(ctx, `
		SELECT s.monitor_id, s.region, s.status, s.consecutive_fails, s.consecutive_oks,
		       s.last_checked_at, s.last_error
		FROM monitor_state s
		JOIN monitor_regions r ON r.monitor_id = s.monitor_id AND r.region = s.region
		WHERE s.monitor_id = $1 ORDER BY s.region`, monitorID)
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

// мониторы без состояния присутствуют в карте с nil-слайсом; порядок
// регионов внутри монитора тот же, что у States.
func (s *Service) StatesBatch(ctx context.Context, monitorIDs []int64) (map[int64][]State, error) {
	out := make(map[int64][]State, len(monitorIDs))
	if len(monitorIDs) == 0 {
		return out, nil
	}
	for _, id := range monitorIDs {
		out[id] = nil
	}
	// тот же JOIN, что в States — снятые регионы не должны попадать в агрегат/список.
	rows, err := s.pool.Query(ctx, `
		SELECT s.monitor_id, s.region, s.status, s.consecutive_fails, s.consecutive_oks,
		       s.last_checked_at, s.last_error
		FROM monitor_state s
		JOIN monitor_regions r ON r.monitor_id = s.monitor_id AND r.region = s.region
		WHERE s.monitor_id = ANY($1) ORDER BY s.monitor_id, s.region`, monitorIDs)
	if err != nil {
		return nil, fmt.Errorf("uptime: states batch: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var st State
		if err := rows.Scan(&st.MonitorID, &st.Region, &st.Status, &st.ConsecutiveFails,
			&st.ConsecutiveOKs, &st.LastCheckedAt, &st.LastError); err != nil {
			return nil, fmt.Errorf("uptime: states batch: scan: %w", err)
		}
		out[st.MonitorID] = append(out[st.MonitorID], st)
	}
	return out, rows.Err()
}

// один INSERT ... ON CONFLICT, не CTE+UPDATE: снэпшот запроса не увидит строку,
// только что вставленную CTE; неизвестный monitorID даёт ErrNoRows, не FK violation.
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
			-- Задания одного региона после истечения лизы могут прийти не по
			-- порядку: worker считается пропавшим, регион переставляется в
			-- очередь и проверяется заново, а старый worker всё же дозванивает
			-- и доставляет свой результат позже. $5 < last_checked_at отличает
			-- такой запоздавший результат от настоящего свежего.
			--
			-- last_checked_at не откатывается назад тем же приёмом, что уже
			-- применён для issues.last_seen (internal/issue/issue.go:68).
			-- last_error обязан следовать той же отметке: обновляется, только
			-- когда результат действительно свежее — иначе текст ошибки
			-- относился бы к более раннему моменту, чем показанное время
			-- последней проверки.
			--
			-- consecutive_fails/oks и status тоже не должны реагировать на
			-- запоздавший результат: это не отдельный полноценный «ещё один
			-- провал/успех» в текущей серии, а сведения о проверке, которая по
			-- времени была РАНЬШЕ уже учтённой. Если бы её всё равно
			-- прибавляли к серии, один настоящий свежий провал мог бы
			-- «дозаполнить» fail_threshold за счёт чужого устаревшего провала
			-- и преждевременно увести монитор в down (или наоборот, вверх) —
			-- то есть заразить текущую серию данными не в счёт неё. Поэтому
			-- запоздавший результат просто не трогает эти колонки, как будто
			-- его никогда не было (last_checked_at/last_error) применительно
			-- к состоянию.
			consecutive_fails = CASE
				WHEN $5 < monitor_state.last_checked_at THEN monitor_state.consecutive_fails
				WHEN $3 THEN 0
				ELSE monitor_state.consecutive_fails + 1
			END,
			consecutive_oks   = CASE
				WHEN $5 < monitor_state.last_checked_at THEN monitor_state.consecutive_oks
				WHEN $3 THEN monitor_state.consecutive_oks + 1
				ELSE 0
			END,
			last_checked_at   = GREATEST(monitor_state.last_checked_at, $5),
			last_error        = CASE WHEN $5 >= monitor_state.last_checked_at THEN $4 ELSE monitor_state.last_error END,
			status = CASE
				WHEN $5 < monitor_state.last_checked_at THEN monitor_state.status
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
