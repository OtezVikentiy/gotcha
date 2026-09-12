package alert

import (
	"context"
	"log/slog"
)

// При массово провалившейся ротации подробный лог каждого дал бы полотно —
// кап не теряет сам факт, итог считает все, просто не печатает каждый.
const rewrapLogCap = 5

// CAS: WHERE проверяет, что secret всё ещё тот, что прочитан в начале
// партии — конкурентная запись не затирается, ноль строк значит «опередили».
func (s *Service) rewrapChannelSecret(ctx context.Context, id int64, prev, out string) (bool, error) {
	tag, err := s.pool.Exec(ctx,
		"UPDATE alert_channels SET secret = $2 WHERE id = $1 AND secret = $3", id, out, prev)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// Идемпотентна; пустой секрет не запечатывается (иначе сломался бы смысл
// «оставить прежний» в UpdateChannel); нерасшифруемый — пропускается, не падает.
func (s *Service) RewrapSecrets(ctx context.Context) (int, error) {
	if !s.secretKeySet {
		return 0, nil
	}
	rows, err := s.pool.Query(ctx, "SELECT id, secret FROM alert_channels")
	if err != nil {
		return 0, err
	}
	type candidate struct {
		id     int64
		secret string
	}
	// Партия читается ДО апдейтов: курсор нельзя держать открытым во время
	// UPDATE по тому же пулу.
	var batch []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.id, &c.secret); err != nil {
			rows.Close()
			return 0, err
		}
		batch = append(batch, c)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()

	updated, unreadable := 0, 0
	for _, c := range batch {
		out, changed, err := s.ring.Rewrap(c.secret)
		if err != nil {
			unreadable++
			if unreadable <= rewrapLogCap {
				slog.Error("alert: channel secret cannot be rewrapped, skipping",
					"channel_id", c.id, "error", err)
			}
			continue
		}
		if !changed {
			continue
		}
		ok, err := s.rewrapChannelSecret(ctx, c.id, c.secret, out)
		if err != nil {
			// SQL-ошибка на одной строке не роняет старт: маскирование и
			// поканальная деградация на чтении работают и без бэкфилла.
			slog.Warn("alert: rewrap channel secret: update failed", "channel_id", c.id, "error", err)
			continue
		}
		if ok {
			updated++
		}
	}
	slog.Info("alert: rewrap secrets backfill complete", "updated", updated, "unreadable", unreadable)
	return updated, nil
}
