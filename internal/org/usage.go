package org

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// monthStart нормализует month к первому дню месяца в UTC — org_usage
// ключуется по (org_id, period_month), где period_month всегда 1-е число.
//
// Момент сначала переводится в UTC, и только потом из него берутся год и
// месяц. Без этого Date() читал год и месяц В ЗОНЕ АРГУМЕНТА, а результат
// штамповался как UTC — то есть строка выбиралась по локальному календарю
// вызывающего. Вызывающие при этом разные: приём считает квоту от локальных
// часов (ingest.OrgQuota.now = time.Now), счётчик отброшенного — от
// time.Now().UTC(), страница организации — снова от локальных. На инстансе с
// выставленной TZ в окне шириной со смещение зоны на стыке месяцев это
// расходилось: принятое из запроса писалось в один месяц, отброшенное из ТОГО
// ЖЕ запроса — в другой, а квота организации обнулялась раньше срока.
func monthStart(month time.Time) time.Time {
	y, m, _ := month.UTC().Date()
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

// checkAndCount условно списывает want единиц из месячной квоты и возвращает,
// СКОЛЬКО удалось списать. Отклонённое НЕ инкрементит счётчик (usage не считает
// то, что не приняли).
//
// Списание ЧАСТИЧНОЕ: если до квоты осталось меньше, чем просят, засчитывается
// остаток, а вызывающий выбрасывает разницу и считает её в дропы. Так
// организация получает ровно свою квоту, а не «последний конверт целиком мимо»,
// и org_usage остаётся точным. Раньше списывалась единица за HTTP-ЗАПРОС: один
// конверт с тысячей событий или десятью тысячами OTLP-спанов стоил ровно
// столько же, сколько одно событие, то есть квоту можно было обойти на четыре
// порядка, а usage — источник правды по потреблению — врал на столько же.
//
// Одним оператором это не выражается: чтобы вернуть СКОЛЬКО списано, нужно
// знать значение до инкремента, а RETURNING в PostgreSQL 17 отдаёт только новую
// строку (RETURNING OLD появился в 18). Поэтому короткая транзакция с
// SELECT ... FOR UPDATE — блокировка строки закрывает гонку двух приёмов той же
// организации, ровно как это делал условный WHERE раньше.
//
// quota==0 — безлимит: списывается всё запрошенное. col — доверенное имя
// колонки из фиксированного набора (не из пользовательского ввода).
func (s *Service) checkAndCount(ctx context.Context, col string, orgID int64, month time.Time, quota, want int64) (int64, error) {
	if want <= 0 {
		return 0, nil
	}
	m := monthStart(month)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("org: check %s: %w", col, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Строка месяца может отсутствовать — создаём пустую, чтобы дальше её можно
	// было заблокировать. DO NOTHING: конкурент мог успеть первым.
	if _, err := tx.Exec(ctx,
		`INSERT INTO org_usage (org_id, period_month) VALUES ($1, $2)
		 ON CONFLICT (org_id, period_month) DO NOTHING`, orgID, m); err != nil {
		return 0, fmt.Errorf("org: check %s: %w", col, err)
	}

	var used int64
	if err := tx.QueryRow(ctx,
		`SELECT `+col+` FROM org_usage WHERE org_id = $1 AND period_month = $2 FOR UPDATE`,
		orgID, m).Scan(&used); err != nil {
		return 0, fmt.Errorf("org: check %s: %w", col, err)
	}

	granted := want
	if quota > 0 {
		room := quota - used
		if room <= 0 {
			return 0, tx.Commit(ctx) // квота исчерпана, счётчик не тронут
		}
		if room < granted {
			granted = room
		}
	}

	if _, err := tx.Exec(ctx,
		`UPDATE org_usage SET `+col+` = `+col+` + $3 WHERE org_id = $1 AND period_month = $2`,
		orgID, m, granted); err != nil {
		return 0, fmt.Errorf("org: check %s: %w", col, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("org: check %s: %w", col, err)
	}
	return granted, nil
}

// CheckAndCountEvents условно инкрементит счётчик событий за месяц (квота 0 —
// безлимит) и сообщает, разрешён ли приём. Отклонённые не считаются.
func (s *Service) CheckAndCountEvents(ctx context.Context, orgID int64, month time.Time, quota, want int64) (int64, error) {
	return s.checkAndCount(ctx, "events_count", orgID, month, quota, want)
}

// CheckAndCountTransactions — то же для счётчика транзакций (независимая квота).
func (s *Service) CheckAndCountTransactions(ctx context.Context, orgID int64, month time.Time, quota, want int64) (int64, error) {
	return s.checkAndCount(ctx, "transactions_count", orgID, month, quota, want)
}

// CheckAndCountMetrics — то же для счётчика метрик (независимая квота).
func (s *Service) CheckAndCountMetrics(ctx context.Context, orgID int64, month time.Time, quota, want int64) (int64, error) {
	return s.checkAndCount(ctx, "metrics_count", orgID, month, quota, want)
}

// CheckAndCountProfiles — то же для счётчика профилей (независимая квота).
func (s *Service) CheckAndCountProfiles(ctx context.Context, orgID int64, month time.Time, quota, want int64) (int64, error) {
	return s.checkAndCount(ctx, "profiles_count", orgID, month, quota, want)
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
