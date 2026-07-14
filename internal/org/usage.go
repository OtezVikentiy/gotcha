package org

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// monthStart нормализует month к первому дню месяца (UTC) — org_usage
// ключуется по (org_id, period_month), где period_month всегда 1-е число.
func monthStart(month time.Time) time.Time {
	y, m, _ := month.Date()
	return time.Date(y, m, 1, 0, 0, 0, 0, time.UTC)
}

// Usage возвращает счётчик событий организации за месяц (0, если записи нет).
func (s *Service) Usage(ctx context.Context, orgID int64, month time.Time) (int64, error) {
	var n int64
	err := s.pool.QueryRow(ctx,
		"SELECT events_count FROM org_usage WHERE org_id = $1 AND period_month = $2",
		orgID, monthStart(month)).Scan(&n)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("org: usage: %w", err)
	}
	return n, nil
}

// IncUsage увеличивает счётчик событий организации за месяц на 1 и
// возвращает новое значение. Разные месяцы независимы (первый инкремент
// месяца заводит строку с events_count=1).
func (s *Service) IncUsage(ctx context.Context, orgID int64, month time.Time) (int64, error) {
	var n int64
	err := s.pool.QueryRow(ctx, `
		INSERT INTO org_usage (org_id, period_month, events_count)
		VALUES ($1, $2, 1)
		ON CONFLICT (org_id, period_month) DO UPDATE SET
			events_count = org_usage.events_count + 1
		RETURNING events_count`,
		orgID, monthStart(month)).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("org: inc usage: %w", err)
	}
	return n, nil
}

// SetQuota меняет месячную квоту событий организации. Quota >= 0 required
// (0 means unlimited).
func (s *Service) SetQuota(ctx context.Context, orgID, quota int64) error {
	if quota < 0 {
		return ErrInvalidQuota
	}
	tag, err := s.pool.Exec(ctx,
		"UPDATE organizations SET event_quota = $2 WHERE id = $1", orgID, quota)
	if err != nil {
		return fmt.Errorf("org: set quota: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
