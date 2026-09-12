package org

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Сначала UTC, потом год/месяц: обратный порядок берёт календарь из зоны
// аргумента и штампует его как UTC — период сдвигается на стыке месяцев.
func monthStart(month time.Time) time.Time {
	y, m, _ := month.UTC().Date()
	return time.Date(y, m, 1, 0, 0, 0, 0, time.UTC)
}

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

func (s *Service) TransactionUsage(ctx context.Context, orgID int64, month time.Time) (int64, error) {
	var n int64
	err := s.pool.QueryRow(ctx,
		"SELECT transactions_count FROM org_usage WHERE org_id = $1 AND period_month = $2",
		orgID, monthStart(month)).Scan(&n)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("org: transaction usage: %w", err)
	}
	return n, nil
}

func (s *Service) IncTransactionUsage(ctx context.Context, orgID int64, month time.Time) (int64, error) {
	var n int64
	err := s.pool.QueryRow(ctx, `
		INSERT INTO org_usage (org_id, period_month, transactions_count)
		VALUES ($1, $2, 1)
		ON CONFLICT (org_id, period_month) DO UPDATE SET
			transactions_count = org_usage.transactions_count + 1
		RETURNING transactions_count`,
		orgID, monthStart(month)).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("org: inc transaction usage: %w", err)
	}
	return n, nil
}

func (s *Service) MetricUsage(ctx context.Context, orgID int64, month time.Time) (int64, error) {
	var n int64
	err := s.pool.QueryRow(ctx,
		"SELECT metrics_count FROM org_usage WHERE org_id = $1 AND period_month = $2",
		orgID, monthStart(month)).Scan(&n)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("org: metric usage: %w", err)
	}
	return n, nil
}

func (s *Service) IncMetricUsage(ctx context.Context, orgID int64, month time.Time) (int64, error) {
	var n int64
	err := s.pool.QueryRow(ctx, `
		INSERT INTO org_usage (org_id, period_month, metrics_count)
		VALUES ($1, $2, 1)
		ON CONFLICT (org_id, period_month) DO UPDATE SET
			metrics_count = org_usage.metrics_count + 1
		RETURNING metrics_count`,
		orgID, monthStart(month)).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("org: inc metric usage: %w", err)
	}
	return n, nil
}

func (s *Service) ProfileUsage(ctx context.Context, orgID int64, month time.Time) (int64, error) {
	var n int64
	err := s.pool.QueryRow(ctx,
		"SELECT profiles_count FROM org_usage WHERE org_id = $1 AND period_month = $2",
		orgID, monthStart(month)).Scan(&n)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("org: profile usage: %w", err)
	}
	return n, nil
}

func (s *Service) IncProfileUsage(ctx context.Context, orgID int64, month time.Time) (int64, error) {
	var n int64
	err := s.pool.QueryRow(ctx, `
		INSERT INTO org_usage (org_id, period_month, profiles_count)
		VALUES ($1, $2, 1)
		ON CONFLICT (org_id, period_month) DO UPDATE SET
			profiles_count = org_usage.profiles_count + 1
		RETURNING profiles_count`,
		orgID, monthStart(month)).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("org: inc profile usage: %w", err)
	}
	return n, nil
}

func (s *Service) LogUsage(ctx context.Context, orgID int64, month time.Time) (int64, error) {
	var n int64
	err := s.pool.QueryRow(ctx,
		"SELECT logs_count FROM org_usage WHERE org_id = $1 AND period_month = $2",
		orgID, monthStart(month)).Scan(&n)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("org: log usage: %w", err)
	}
	return n, nil
}

type Dropped struct {
	Events       int64
	Transactions int64
	Metrics      int64
	Profiles     int64
	Logs         int64
}

func (s *Service) DroppedUsage(ctx context.Context, orgID int64, month time.Time) (Dropped, error) {
	var d Dropped
	err := s.pool.QueryRow(ctx, `
		SELECT dropped_events, dropped_transactions, dropped_metrics, dropped_profiles, dropped_logs
		FROM org_usage WHERE org_id = $1 AND period_month = $2`,
		orgID, monthStart(month)).Scan(&d.Events, &d.Transactions, &d.Metrics, &d.Profiles, &d.Logs)
	if errors.Is(err, pgx.ErrNoRows) {
		return Dropped{}, nil
	}
	if err != nil {
		return Dropped{}, fmt.Errorf("org: dropped usage: %w", err)
	}
	return d, nil
}

func (s *Service) incDropped(ctx context.Context, col string, orgID int64, month time.Time, n int64) error {
	if n <= 0 {
		return nil
	}
	sql := `
		INSERT INTO org_usage (org_id, period_month, ` + col + `)
		VALUES ($1, $2, $3)
		ON CONFLICT (org_id, period_month) DO UPDATE SET
			` + col + ` = org_usage.` + col + ` + $3`
	if _, err := s.pool.Exec(ctx, sql, orgID, monthStart(month), n); err != nil {
		return fmt.Errorf("org: inc %s: %w", col, err)
	}
	return nil
}

func (s *Service) IncDroppedEvents(ctx context.Context, orgID int64, month time.Time, n int64) error {
	return s.incDropped(ctx, "dropped_events", orgID, month, n)
}

func (s *Service) IncDroppedTransactions(ctx context.Context, orgID int64, month time.Time, n int64) error {
	return s.incDropped(ctx, "dropped_transactions", orgID, month, n)
}

func (s *Service) IncDroppedMetrics(ctx context.Context, orgID int64, month time.Time, n int64) error {
	return s.incDropped(ctx, "dropped_metrics", orgID, month, n)
}

func (s *Service) IncDroppedProfiles(ctx context.Context, orgID int64, month time.Time, n int64) error {
	return s.incDropped(ctx, "dropped_profiles", orgID, month, n)
}

func (s *Service) IncDroppedLogs(ctx context.Context, orgID int64, month time.Time, n int64) error {
	return s.incDropped(ctx, "dropped_logs", orgID, month, n)
}

func (s *Service) checkAndCount(ctx context.Context, col string, orgID int64, month time.Time, quota, want int64) (int64, error) {
	if want <= 0 {
		return 0, nil
	}
	// PG RETURNING отдаёт только новую строку: колонка *_before хранит значение
	// ДО этого UPDATE, чтобы списанное считалось разностью after-before.
	before := col + "_before"
	sql := `
		INSERT INTO org_usage (org_id, period_month, ` + col + `, ` + before + `)
		VALUES ($1, $2, CASE WHEN $4 <= 0 THEN $3 ELSE LEAST($3, $4) END, 0)
		ON CONFLICT (org_id, period_month) DO UPDATE SET
			` + col + ` = org_usage.` + col + ` + CASE
				WHEN $4 <= 0 THEN $3
				ELSE GREATEST(LEAST($3, $4 - org_usage.` + col + `), 0)
			END,
			` + before + ` = org_usage.` + col + `
		WHERE $4 <= 0 OR org_usage.` + col + ` < $4
		RETURNING ` + col + `, ` + before
	var after, pre int64
	err := s.pool.QueryRow(ctx, sql, orgID, monthStart(month), want, quota).Scan(&after, &pre)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("org: check %s: %w", col, err)
	}
	return after - pre, nil
}

func (s *Service) CheckAndCountEvents(ctx context.Context, orgID int64, month time.Time, quota, want int64) (int64, error) {
	return s.checkAndCount(ctx, "events_count", orgID, month, quota, want)
}

func (s *Service) refund(ctx context.Context, col string, orgID int64, month time.Time, n int64) error {
	if n <= 0 {
		return nil
	}
	sql := `
		UPDATE org_usage SET ` + col + ` = GREATEST(` + col + ` - $3, 0)
		WHERE org_id = $1 AND period_month = $2`
	if _, err := s.pool.Exec(ctx, sql, orgID, monthStart(month), n); err != nil {
		return fmt.Errorf("org: refund %s: %w", col, err)
	}
	return nil
}

func (s *Service) RefundEvents(ctx context.Context, orgID int64, month time.Time, n int64) error {
	return s.refund(ctx, "events_count", orgID, month, n)
}

func (s *Service) CheckAndCountTransactions(ctx context.Context, orgID int64, month time.Time, quota, want int64) (int64, error) {
	return s.checkAndCount(ctx, "transactions_count", orgID, month, quota, want)
}

func (s *Service) RefundTransactions(ctx context.Context, orgID int64, month time.Time, n int64) error {
	return s.refund(ctx, "transactions_count", orgID, month, n)
}

func (s *Service) CheckAndCountMetrics(ctx context.Context, orgID int64, month time.Time, quota, want int64) (int64, error) {
	return s.checkAndCount(ctx, "metrics_count", orgID, month, quota, want)
}

func (s *Service) RefundMetrics(ctx context.Context, orgID int64, month time.Time, n int64) error {
	return s.refund(ctx, "metrics_count", orgID, month, n)
}

func (s *Service) CheckAndCountProfiles(ctx context.Context, orgID int64, month time.Time, quota, want int64) (int64, error) {
	return s.checkAndCount(ctx, "profiles_count", orgID, month, quota, want)
}

func (s *Service) RefundProfiles(ctx context.Context, orgID int64, month time.Time, n int64) error {
	return s.refund(ctx, "profiles_count", orgID, month, n)
}

func (s *Service) CheckAndCountLogs(ctx context.Context, orgID int64, month time.Time, quota, want int64) (int64, error) {
	return s.checkAndCount(ctx, "logs_count", orgID, month, quota, want)
}

func (s *Service) RefundLogs(ctx context.Context, orgID int64, month time.Time, n int64) error {
	return s.refund(ctx, "logs_count", orgID, month, n)
}

func (s *Service) SetProfileQuota(ctx context.Context, orgID, quota int64) error {
	if quota < 0 {
		return ErrInvalidQuota
	}
	tag, err := s.pool.Exec(ctx,
		"UPDATE organizations SET profile_quota = $2 WHERE id = $1", orgID, quota)
	if err != nil {
		return fmt.Errorf("org: set profile quota: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Service) SetMetricQuota(ctx context.Context, orgID, quota int64) error {
	if quota < 0 {
		return ErrInvalidQuota
	}
	tag, err := s.pool.Exec(ctx,
		"UPDATE organizations SET metric_quota = $2 WHERE id = $1", orgID, quota)
	if err != nil {
		return fmt.Errorf("org: set metric quota: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Service) SetLogQuota(ctx context.Context, orgID, quota int64) error {
	if quota < 0 {
		return ErrInvalidQuota
	}
	tag, err := s.pool.Exec(ctx,
		"UPDATE organizations SET log_quota = $2 WHERE id = $1", orgID, quota)
	if err != nil {
		return fmt.Errorf("org: set log quota: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Service) SetTransactionQuota(ctx context.Context, orgID, quota int64) error {
	if quota < 0 {
		return ErrInvalidQuota
	}
	tag, err := s.pool.Exec(ctx,
		"UPDATE organizations SET transaction_quota = $2 WHERE id = $1", orgID, quota)
	if err != nil {
		return fmt.Errorf("org: set transaction quota: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Service) SetQuotas(ctx context.Context, orgID int64, event, transaction, metric, profile, log *int64) error {
	for _, v := range []*int64{event, transaction, metric, profile, log} {
		if v != nil && *v < 0 {
			return ErrInvalidQuota
		}
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE organizations SET
			event_quota = COALESCE($2, event_quota),
			transaction_quota = COALESCE($3, transaction_quota),
			metric_quota = COALESCE($4, metric_quota),
			profile_quota = COALESCE($5, profile_quota),
			log_quota = COALESCE($6, log_quota)
		WHERE id = $1`,
		orgID, event, transaction, metric, profile, log)
	if err != nil {
		return fmt.Errorf("org: set quotas: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

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
