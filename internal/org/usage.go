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

// TransactionUsage возвращает счётчик транзакций организации за месяц
// (0, если записи нет). Счётчик отдельный от событий: транзакции и ошибки
// живут в разных колонках одной строки org_usage и не мешают друг другу.
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

// IncTransactionUsage увеличивает счётчик транзакций организации за месяц на 1
// и возвращает новое значение. events_count при этом не трогается (и наоборот,
// см. IncUsage) — квоты ошибок и транзакций независимы.
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

// MetricUsage возвращает счётчик метрик организации за месяц (0, если нет
// записи). Отдельный счётчик от событий/транзакций (org_usage.metrics_count).
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

// IncMetricUsage увеличивает счётчик метрик организации за месяц на 1 и
// возвращает новое значение. events_count/transactions_count не трогаются —
// квоты независимы.
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

// ProfileUsage возвращает счётчик профилей организации за месяц (0, если нет
// записи). Отдельный счётчик (org_usage.profiles_count).
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

// IncProfileUsage увеличивает счётчик профилей организации за месяц на 1.
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

// Dropped — счётчики ОТКЛОНЁННЫХ (drop) единиц организации за месяц: сколько
// событий/транзакций/метрик/профилей приём отбросил (исчерпана квота и т.п.).
// Отдельны от принятых счётчиков (events_count и др.) — это реальные потери,
// которые оператор обязан видеть (PROD-P1: конец молчаливых потерь).
type Dropped struct {
	Events       int64
	Transactions int64
	Metrics      int64
	Profiles     int64
}

// DroppedUsage возвращает счётчики дропов организации за месяц (нули, если
// записи нет).
func (s *Service) DroppedUsage(ctx context.Context, orgID int64, month time.Time) (Dropped, error) {
	var d Dropped
	err := s.pool.QueryRow(ctx, `
		SELECT dropped_events, dropped_transactions, dropped_metrics, dropped_profiles
		FROM org_usage WHERE org_id = $1 AND period_month = $2`,
		orgID, monthStart(month)).Scan(&d.Events, &d.Transactions, &d.Metrics, &d.Profiles)
	if errors.Is(err, pgx.ErrNoRows) {
		return Dropped{}, nil
	}
	if err != nil {
		return Dropped{}, fmt.Errorf("org: dropped usage: %w", err)
	}
	return d, nil
}

// incDropped — общий UPSERT для счётчиков дропов: заводит строку месяца с
// нужным счётчиком = n либо прибавляет n к существующему. col — доверенное имя
// колонки из фиксированного набора (не из пользовательского ввода).
func (s *Service) incDropped(ctx context.Context, col string, orgID int64, month time.Time, n int64) error {
	if n <= 0 {
		return nil // отрицательный/нулевой инкремент — no-op, счётчик потерь только растёт
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

// IncDroppedEvents увеличивает счётчик отклонённых событий организации за месяц
// на n. Принятые счётчики не трогаются.
func (s *Service) IncDroppedEvents(ctx context.Context, orgID int64, month time.Time, n int64) error {
	return s.incDropped(ctx, "dropped_events", orgID, month, n)
}

// IncDroppedTransactions увеличивает счётчик отклонённых транзакций за месяц на n.
func (s *Service) IncDroppedTransactions(ctx context.Context, orgID int64, month time.Time, n int64) error {
	return s.incDropped(ctx, "dropped_transactions", orgID, month, n)
}

// IncDroppedMetrics увеличивает счётчик отклонённых метрик за месяц на n.
func (s *Service) IncDroppedMetrics(ctx context.Context, orgID int64, month time.Time, n int64) error {
	return s.incDropped(ctx, "dropped_metrics", orgID, month, n)
}

// IncDroppedProfiles увеличивает счётчик отклонённых профилей за месяц на n.
func (s *Service) IncDroppedProfiles(ctx context.Context, orgID int64, month time.Time, n int64) error {
	return s.incDropped(ctx, "dropped_profiles", orgID, month, n)
}

// checkAndCount — атомарный УСЛОВНЫЙ инкремент счётчика приёма col за месяц:
// увеличивает col на 1 только если организация ещё укладывается в квоту, и
// сообщает, разрешён ли приём. В отличие от Inc*, отклонённая единица НЕ
// инкрементит счётчик (ARCH-L1: usage не считает отвергнутое).
//
// Семантика ON CONFLICT ... WHERE:
//   - строки месяца ещё нет → INSERT VALUES(...,1) проходит (первая единица
//     всегда влезает при quota>=1 или безлимите) → RETURNING 1 → allowed;
//   - строка есть → DO UPDATE применяет WHERE «$3=0 (безлимит) ИЛИ col<$3»:
//     при col==quota условие ложно, апдейта нет → RETURNING пусто →
//     pgx.ErrNoRows → allowed=false БЕЗ инкремента.
//
// quota==0 — безлимит: инкремент всегда, allowed=true. col — доверенное имя
// колонки из фиксированного набора (не из пользовательского ввода).
func (s *Service) checkAndCount(ctx context.Context, col string, orgID int64, month time.Time, quota int64) (bool, error) {
	var n int64
	sql := `
		INSERT INTO org_usage (org_id, period_month, ` + col + `)
		VALUES ($1, $2, 1)
		ON CONFLICT (org_id, period_month) DO UPDATE SET
			` + col + ` = org_usage.` + col + ` + 1
		WHERE $3 = 0 OR org_usage.` + col + ` < $3
		RETURNING ` + col
	err := s.pool.QueryRow(ctx, sql, orgID, monthStart(month), quota).Scan(&n)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil // квота исчерпана: WHERE не сработал, счётчик не тронут
	}
	if err != nil {
		return false, fmt.Errorf("org: check %s: %w", col, err)
	}
	return true, nil
}

// CheckAndCountEvents условно инкрементит счётчик событий за месяц (квота 0 —
// безлимит) и сообщает, разрешён ли приём. Отклонённые не считаются.
func (s *Service) CheckAndCountEvents(ctx context.Context, orgID int64, month time.Time, quota int64) (bool, error) {
	return s.checkAndCount(ctx, "events_count", orgID, month, quota)
}

// CheckAndCountTransactions — то же для счётчика транзакций (независимая квота).
func (s *Service) CheckAndCountTransactions(ctx context.Context, orgID int64, month time.Time, quota int64) (bool, error) {
	return s.checkAndCount(ctx, "transactions_count", orgID, month, quota)
}

// CheckAndCountMetrics — то же для счётчика метрик (независимая квота).
func (s *Service) CheckAndCountMetrics(ctx context.Context, orgID int64, month time.Time, quota int64) (bool, error) {
	return s.checkAndCount(ctx, "metrics_count", orgID, month, quota)
}

// CheckAndCountProfiles — то же для счётчика профилей (независимая квота).
func (s *Service) CheckAndCountProfiles(ctx context.Context, orgID int64, month time.Time, quota int64) (bool, error) {
	return s.checkAndCount(ctx, "profiles_count", orgID, month, quota)
}

// SetProfileQuota меняет месячную квоту профилей организации. Quota >= 0 required
// (0 means unlimited).
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

// SetMetricQuota меняет месячную квоту метрик организации. Quota >= 0 required
// (0 means unlimited).
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

// SetTransactionQuota меняет месячную квоту транзакций организации.
// Quota >= 0 required (0 means unlimited).
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
