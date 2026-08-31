package org

import (
	"context"
	"log/slog"
)

// rewrapLogCap — сколько нечитаемых client_secret бэкфилл логирует подробно
// за один проход. Симметрично internal/alert.rewrapLogCap: при массово
// провалившейся ротации подробное логирование каждого дало бы полотно, в
// котором тонет итоговая строка; кап не теряет сам факт — итог считает все.
const rewrapLogCap = 5

// rewrapSSOSecret — CAS-обновление client_secret одной организации: условие
// в WHERE проверяет, что значение всё ещё равно тому, что RewrapSecrets
// прочитал в начале партии. Конкурентная запись между чтением партии и этим
// UPDATE (обычный UpsertSSO или бэкфилл на другой реплике) не затирается —
// ноль затронутых строк означает «кто-то опередил», а не ошибку.
func (s *Service) rewrapSSOSecret(ctx context.Context, orgID int64, prev, out string) (bool, error) {
	tag, err := s.pool.Exec(ctx,
		"UPDATE org_sso SET client_secret = $2 WHERE org_id = $1 AND client_secret = $3", orgID, out, prev)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// RewrapSecrets поднимает client_secret всех SSO-конфигов до конверта версии
// 2 текущего ключа кольца. Симметрично alert.Service.RewrapSecrets (см. его
// комментарий за подробностями инвариантов, §4/§6 спеки ротации); здесь
// отличается только таблица: org_sso, PK — org_id (без отдельного id),
// максимум одна строка на организацию (UNIQUE org_id).
//
// Вызывается один раз на старте после SetKeyring, до подъёма слушателя.
// Идемпотентен, пустой секрет не запечатывается, нерасшифруемый —
// пропускается без ошибки. Без ключа (dev) — no-op.
func (s *Service) RewrapSecrets(ctx context.Context) (int, error) {
	if !s.secretKeySet {
		return 0, nil
	}
	rows, err := s.pool.Query(ctx, "SELECT org_id, client_secret FROM org_sso")
	if err != nil {
		return 0, err
	}
	type candidate struct {
		orgID  int64
		secret string
	}
	// Партия читается ДО апдейтов: курсор нельзя держать открытым во время
	// UPDATE по тому же пулу.
	var batch []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.orgID, &c.secret); err != nil {
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
				slog.Error("org: sso client_secret cannot be rewrapped, skipping",
					"org_id", c.orgID, "error", err)
			}
			continue
		}
		if !changed {
			continue
		}
		ok, err := s.rewrapSSOSecret(ctx, c.orgID, c.secret, out)
		if err != nil {
			// SQL-ошибка на одной строке не роняет старт: decryptSSO уже
			// деградирует поштучно на чтении и без бэкфилла.
			slog.Warn("org: rewrap sso secret: update failed", "org_id", c.orgID, "error", err)
			continue
		}
		if ok {
			updated++
		}
	}
	slog.Info("org: rewrap secrets backfill complete", "updated", updated, "unreadable", unreadable)
	return updated, nil
}
