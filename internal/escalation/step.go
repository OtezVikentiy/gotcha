package escalation

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Без потолка падающий LogStep пейджит одну ступень на каждом тике бесконечно.
// После maxLogFailureAttempts провалов подряд прогресс продавливается принудительно, с громким логом.
const maxLogFailureAttempts = 5

// Claim-before-notify: занимаем ступень, потом отправляем — иначе дубль при двух репликах.
// Инвариант: escalation_level не обгоняет incident_escalations — bump зовётся последним, после claim/log.
func SendStepIfDue(ctx context.Context, ladder Ladder, source string, pool *pgxpool.Pool, incidentID int64, level int, elapsed time.Duration,
	notifyStep func(channelIDs []int64, step int) ([]int64, error), bump func(id int64, from int) (bool, error)) (sent bool, err error) {
	if level >= len(ladder) {
		return false, nil
	}
	if elapsed < time.Duration(ladder[level].DelayMinutes)*time.Minute {
		return false, nil
	}
	chs := ladder[level].ChannelIDs
	if len(chs) == 0 {
		// Ступень без каналов — нечего занимать и слать, но бампим, чтобы
		// эскалация не клинила.
		return bump(incidentID, level)
	}
	won, err := ClaimStepChannels(ctx, pool, source, incidentID, level, chs)
	if err != nil {
		return false, err
	}
	if len(won) == 0 {
		// Ступень уже занята (другой репликой или упавшим предыдущим тиком) —
		// не шлём, но бампим: CAS безопасен для гонки, а эту ступень уже не восстановить.
		return bump(incidentID, level)
	}
	enqueued, notifyErr := notifyStep(won, level)
	// unsent — выигранные claim'ом каналы, которые реально не встали в очередь.
	// Их лог откатывается, чтобы следующий тик повторил именно их.
	unsent := difference(won, enqueued)
	relErr := ReleaseStepChannels(ctx, pool, source, incidentID, level, unsent)
	if relErr != nil {
		slog.Error("escalation: claimed step not released, next tick will skip it",
			"source", source, "incident_id", incidentID, "step", level, "channels", unsent, "error", relErr)
	}
	if notifyErr != nil && len(enqueued) == 0 {
		return false, errors.Join(notifyErr, relErr)
	}
	// Хотя бы один канал получил ступень — продвигаем уровень, чтобы плохой
	// канал не клинил лесенку.
	ok, bumpErr := bump(incidentID, level)
	return ok, errors.Join(notifyErr, relErr, bumpErr)
}

// won — подмножество chs, выигранное этим вызовом (INSERT ON CONFLICT DO NOTHING).
// Пустой won при непустом chs — ступень уже занята другой репликой или упавшим тиком.
func ClaimStepChannels(ctx context.Context, pool *pgxpool.Pool, source string, incidentID int64, step int, chs []int64) (won []int64, err error) {
	if len(chs) == 0 {
		return nil, nil
	}
	rows, err := pool.Query(ctx, `
		INSERT INTO incident_escalations (incident_source, incident_id, step, channel_id)
		SELECT $1, $2, $3, unnest($4::bigint[])
		ON CONFLICT (incident_source, incident_id, channel_id, step) DO NOTHING
		RETURNING channel_id`, source, incidentID, step, chs)
	if err != nil {
		return nil, fmt.Errorf("escalation: claim step channels: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var ch int64
		if err := rows.Scan(&ch); err != nil {
			return nil, fmt.Errorf("escalation: claim step channels scan: %w", err)
		}
		won = append(won, ch)
	}
	return won, rows.Err()
}

// Каналы chs были заняты ClaimStepChannels, но не поставлены в очередь —
// следующий тик увидит ступень для них свободной и повторит.
func ReleaseStepChannels(ctx context.Context, pool *pgxpool.Pool, source string, incidentID int64, step int, chs []int64) error {
	if len(chs) == 0 {
		return nil
	}
	_, err := pool.Exec(ctx, `
		DELETE FROM incident_escalations
		WHERE incident_source = $1 AND incident_id = $2 AND step = $3 AND channel_id = ANY($4::bigint[])`,
		source, incidentID, step, chs)
	if err != nil {
		return fmt.Errorf("escalation: release step channels: %w", err)
	}
	return nil
}

func difference(won, enqueued []int64) []int64 {
	skip := make(map[int64]bool, len(enqueued))
	for _, e := range enqueued {
		skip[e] = true
	}
	var out []int64
	for _, w := range won {
		if !skip[w] {
			out = append(out, w)
		}
	}
	return out
}

// done=true не означает err==nil: после принудительного прогресса по потолку попыток
// done=true, но err остаётся ненулевым — вызывающий обязан вернуть его наверх.
func LogStepChannels(ctx context.Context, pool *pgxpool.Pool, source string, incidentID int64, step int, chs []int64) (done bool, err error) {
	var logErr error
	for _, ch := range chs {
		if e := LogStep(ctx, pool, source, incidentID, ch, step); e != nil {
			slog.Error("escalation: log step failed", "source", source, "incident_id", incidentID, "channel_id", ch, "step", step, "error", e)
			logErr = errors.Join(logErr, e)
		}
	}
	if logErr == nil {
		if err := clearLogFailure(ctx, pool, source, incidentID, step); err != nil {
			// Best-effort: неудача сброса не должна ронять успешный путь.
			slog.Warn("escalation: clear log failure after success failed", "source", source, "incident_id", incidentID, "step", step, "error", err)
		}
		return true, nil
	}
	attempts, trackErr := recordLogFailure(ctx, pool, source, incidentID, step)
	if trackErr != nil {
		slog.Error("escalation: record log failure failed", "source", source, "incident_id", incidentID, "step", step, "error", trackErr)
		return false, errors.Join(logErr, trackErr)
	}
	if attempts < maxLogFailureAttempts {
		return false, logErr
	}
	slog.Error("escalation: log kept failing after max attempts, forcing progress anyway",
		"source", source, "incident_id", incidentID, "step", step, "attempts", attempts, "error", logErr)
	if err := clearLogFailure(ctx, pool, source, incidentID, step); err != nil {
		slog.Error("escalation: clear log failure after forced progress failed", "source", source, "incident_id", incidentID, "step", step, "error", err)
	}
	return true, logErr
}

// DISTINCT: разные ступени могли слать в разные наборы каналов. Канал, не
// видевший тревогу, не должен первым увидеть «инцидент закрыт».
func RecoveryChannels(ctx context.Context, pool *pgxpool.Pool, source string, incidentID int64) ([]int64, error) {
	rows, err := pool.Query(ctx,
		"SELECT DISTINCT channel_id FROM incident_escalations WHERE incident_source = $1 AND incident_id = $2",
		source, incidentID)
	if err != nil {
		return nil, fmt.Errorf("escalation: recovery channels: %w", err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var channelID int64
		if err := rows.Scan(&channelID); err != nil {
			return nil, fmt.Errorf("escalation: recovery channels: %w", err)
		}
		out = append(out, channelID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("escalation: recovery channels: %w", err)
	}
	return out, nil
}
