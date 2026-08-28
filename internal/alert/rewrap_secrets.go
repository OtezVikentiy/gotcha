package alert

import (
	"context"
	"log/slog"
)

// rewrapLogCap — сколько нечитаемых секретов бэкфилл логирует подробно за
// один проход. При массово провалившейся ротации (потерян PREV) подробное
// логирование каждого дало бы полотно на десятки-сотни строк, в котором
// тонет итоговая строка; кап не теряет сам факт нечитаемости — итог считает
// все, просто не печатает каждый по отдельности.
const rewrapLogCap = 5

// rewrapChannelSecret — CAS-обновление secret'а одного канала: условие в
// WHERE проверяет, что значение всё ещё равно тому, что RewrapSecrets прочитал
// в начале партии. Конкурентная запись между чтением партии и этим UPDATE
// (обычный UpdateChannel или бэкфилл на другой реплике) не затирается — ноль
// затронутых строк означает «кто-то опередил», а не ошибку.
func (s *Service) rewrapChannelSecret(ctx context.Context, id int64, prev, out string) (bool, error) {
	tag, err := s.pool.Exec(ctx,
		"UPDATE alert_channels SET secret = $2 WHERE id = $1 AND secret = $3", id, out, prev)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// RewrapSecrets поднимает secret всех каналов доставки до конверта версии 2
// текущего ключа кольца: legacy plaintext, v1 (текущим или предыдущим
// ключом), v2 предыдущим ключом — всё читаемое, а не только при заданном
// PREV (см. Keyring.Rewrap, §4/§6 спеки ротации). Инстанс, ещё ни разу не
// ротировавший ключ, всё равно приезжает в v2 — иначе первая реальная
// ротация упрётся в v1-значения без id ключа. Побочный эффект: каналы,
// заведённые до включения шифрования (legacy plaintext), впервые шифруются
// здесь — решение владельца (§13.2 спеки), не баг.
//
// Вызывается один раз на старте после SetKeyring, до подъёма слушателя.
// Идемпотентен: секрет, уже лежащий в v2 текущего ключа, Rewrap не трогает,
// второй проход возвращает 0. Пустой секрет ("канал без секрета") не
// запечатывается — иначе сломался бы смысл «оставить прежний» в
// UpdateChannel. Нерасшифруемый секрет пропускается: старт не падает, канал
// остаётся в прежнем нечитаемом состоянии — Channels/ChannelSecret уже
// деградируют по нему поштучно на чтении. Без ключа (dev) — no-op.
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
