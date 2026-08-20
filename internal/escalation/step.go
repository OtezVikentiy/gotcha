package escalation

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// SendStepIfDue шлёт ступень [level] лесенки, если её задержка от открытия
// инцидента (elapsed) уже настала, и бампает уровень эскалации. Бамп
// применяется всегда, КРОМЕ тотального провала notifyStep — ошибка И ни один
// канал не заенкенился (QA P2-3: частичный сбой, когда хоть один канал
// реально ушёл в очередь, прогрессу не мешает — иначе один битый канал клинит
// лесенку бесконечным пере-пейджем здоровых; пустой enqueued БЕЗ ошибки — это
// не сбой, а просто пустая лесенка, бамп идёт как обычно). sent=true — бамп
// применился; в остальных случаях (лесенка исчерпана, ступень ещё не подошла
// по времени, тотальный провал notifyStep) — false, чтобы следующий тик
// повторил ступень целиком. Порядок notifyStep(enqueue)→log→bump намеренный
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
	enqueued, notifyErr := notifyStep(ladder[level].ChannelIDs, level)
	// Логируем РЕАЛЬНО заенкенные каналы ДАЖЕ при ошибке notifyStep — они уже
	// в очереди, и recovery должен про них знать (иначе пробел отбоя для тех,
	// кого реально запейджило). QA P2-3.
	for _, ch := range enqueued {
		if err := LogStep(ctx, pool, source, incidentID, ch, level); err != nil {
			slog.Error("escalation: log step failed", "source", source, "incident_id", incidentID, "channel_id", ch, "error", err)
		}
	}
	if notifyErr != nil && len(enqueued) == 0 {
		// ТОТАЛЬНЫЙ сбой: notifyStep вернул ошибку И ни один канал не
		// заенкенился — не бампим, следующий тик повторит эту же ступень
		// целиком. len(enqueued)==0 БЕЗ ошибки (напр. в лесенке нет ни одного
		// канала — проект без alert-каналов) сюда не попадает: notifyStep не
		// провалился, бампить дальше можно и нужно, как и раньше.
		return false, notifyErr
	}
	// Либо notifyStep не ошибся (обычный путь, enqueued может быть и пуст —
	// каналов в лесенке просто не было), либо хотя бы один канал реально
	// получил ступень при частичном сбое — продвигаем уровень, чтобы один
	// плохой канал не клинил лесенку бесконечным пере-пейджем здоровых.
	// Каналы, не попавшие в enqueued при частичном сбое, пропустят эту
	// ступень — осознанный компромисс: прогресс важнее стагнации.
	ok, bumpErr := bump(incidentID, level)
	return ok, errors.Join(notifyErr, bumpErr)
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
