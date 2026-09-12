package escalation

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// nil/пустой ids снаружи означает «все каналы»; непустой — фильтр по членству,
// применяемый ПОСЛЕ Deliverable-гейта.
func ContainsID(ids []int64, id int64) bool {
	for _, v := range ids {
		if v == id {
			return true
		}
	}
	return false
}

// Пишется ПОСЛЕ успешного Enqueue — иначе провал доставки выглядел бы
// отправленным шагом. ON CONFLICT даёт ретраю после краха безопасный no-op.
func LogStep(ctx context.Context, pool *pgxpool.Pool, source string, incidentID, channelID int64, step int) error {
	_, err := pool.Exec(ctx, `
		INSERT INTO incident_escalations (incident_source, incident_id, channel_id, step)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (incident_source, incident_id, channel_id, step) DO NOTHING`, source, incidentID, channelID, step)
	if err != nil {
		return fmt.Errorf("escalation: log step: %w", err)
	}
	return nil
}

// Один счётчик на (source, incident, step), не на канал: заблокированный
// bump держит ВЕСЬ шаг, не только канал, у которого не залогировалось.
func recordLogFailure(ctx context.Context, pool *pgxpool.Pool, source string, incidentID int64, step int) (attempts int, err error) {
	row := pool.QueryRow(ctx, `
		INSERT INTO escalation_step_log_failures (incident_source, incident_id, step, attempts, last_attempt_at)
		VALUES ($1, $2, $3, 1, now())
		ON CONFLICT (incident_source, incident_id, step)
		DO UPDATE SET attempts = escalation_step_log_failures.attempts + 1, last_attempt_at = now()
		RETURNING attempts`, source, incidentID, step)
	if err := row.Scan(&attempts); err != nil {
		return 0, fmt.Errorf("escalation: record log failure: %w", err)
	}
	return attempts, nil
}

// Best-effort: ошибка здесь не роняет основной путь — худшее последствие
// отсутствия сброса — чуть более ранний принудительный bump в следующий раз.
func clearLogFailure(ctx context.Context, pool *pgxpool.Pool, source string, incidentID int64, step int) error {
	_, err := pool.Exec(ctx,
		"DELETE FROM escalation_step_log_failures WHERE incident_source = $1 AND incident_id = $2 AND step = $3",
		source, incidentID, step)
	if err != nil {
		return fmt.Errorf("escalation: clear log failure: %w", err)
	}
	return nil
}
