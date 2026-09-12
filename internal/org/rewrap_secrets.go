package org

import (
	"context"
	"log/slog"
)

const rewrapLogCap = 5

// ok=false (без ошибки) при 0 затронутых строк: значит client_secret успел
// смениться конкурентно между чтением партии и этим UPDATE, а не сбой записи.
func (s *Service) rewrapSSOSecret(ctx context.Context, orgID int64, prev, out string) (bool, error) {
	tag, err := s.pool.Exec(ctx,
		"UPDATE org_sso SET client_secret = $2 WHERE org_id = $1 AND client_secret = $3", orgID, out, prev)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

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
