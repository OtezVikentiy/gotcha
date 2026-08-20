package escalation

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// SendStepIfDue шлёт ступень [level] лесенки, если её задержка от открытия
// инцидента (elapsed) уже настала, и бампает уровень эскалации. sent=true —
// уведомление реально ушло (notifyStep не ошибся) И бамп применился; в
// остальных случаях (лесенка исчерпана, ступень ещё не подошла по времени,
// провал notifyStep) — false. Порядок notifyStep(enqueue)→log→bump намеренный
// (M-3 брифа Task 6).
//
// Логирование в incident_escalations — ЗДЕСЬ, в оркестрации, а не внутри
// notifyStep (T7-fix): изначально логировал сам нотифаер (T6), и это работало
// с продовым нотифаером, но молчало с мок-нотифаерами тестов (мок не пишет в
// БД) — RecoveryChannels не находил ничего залогированного, и recovery
// немел даже когда notifyStep реально «отправил». Здесь источник истины один
// на все 5 источников: notifyStep возвращает РЕАЛЬНО заенкенные каналы (что
// бы за ним ни стояло — outbox или тестовый мок), эта функция логирует их в
// pool — то есть лог пишется независимо от реализации notifyStep.
func SendStepIfDue(ctx context.Context, ladder Ladder, source string, pool *pgxpool.Pool, incidentID int64, level int, elapsed time.Duration,
	notifyStep func(channelIDs []int64, step int) ([]int64, error), bump func(id int64, from int) (bool, error)) (sent bool, err error) {
	if level >= len(ladder) {
		return false, nil
	}
	if elapsed < time.Duration(ladder[level].DelayMinutes)*time.Minute {
		return false, nil
	}
	enqueued, err := notifyStep(ladder[level].ChannelIDs, level)
	if err != nil {
		return false, err
	}
	for _, ch := range enqueued {
		if err := LogStep(ctx, pool, source, incidentID, ch, level); err != nil {
			slog.Error("escalation: log step failed", "source", source, "incident_id", incidentID, "channel_id", ch, "error", err)
		}
	}
	ok, err := bump(incidentID, level)
	return ok, err
}

// RecoveryChannels возвращает каналы, в которые за время жизни инцидента
// уходила хотя бы одна ступень эскалации (несколько разных шагов могли слать
// в разные наборы каналов, поэтому DISTINCT) — recovery адресуется ИМ, а не
// всем каналам проекта заново: канал, который ни разу не видел тревогу, не
// должен первым увидеть «инцидент закрыт» (M-7 брифа Task 6).
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
