package alert

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Дефолты щедрые нарочно — срезать размножение уведомлений, а не мешать
// настоящему инциденту с десятком упавших сервисов сразу.
const (
	defaultAlertBudgetWindow = time.Hour
	defaultAlertBudgetLimit  = 50
)

type BudgetDecision struct {
	Allowed bool
	// Сколько уведомлений подавлено в окне накопительно, включая это; ноль
	// при Allowed.
	Suppressed int
}

// Нулевой или отрицательный потолок выключает ограничение целиком —
// осознанный выбор оператора, не аварийный режим.
func (s *Service) SetBudget(window time.Duration, limit int) {
	if window > 0 {
		s.budgetWindow = window
	}
	s.budgetLimit = limit
	s.budgetSet = true
}

func (s *Service) budgetParams() (time.Duration, int) {
	if !s.budgetSet {
		return defaultAlertBudgetWindow, defaultAlertBudgetLimit
	}
	w := s.budgetWindow
	if w <= 0 {
		w = defaultAlertBudgetWindow
	}
	return w, s.budgetLimit
}

// Решение и учёт — одним запросом против гонки при конкурентных вызовах.
// suppressed не обнуляется здесь — его забирает Digester.
func (s *Service) claimBudget(ctx context.Context, projectID int64) (BudgetDecision, error) {
	window, limit := s.budgetParams()
	if limit <= 0 {
		return BudgetDecision{Allowed: true}, nil // ограничение выключено
	}
	// Часы БАЗЫ, не процесса — иначе расхождение часов растягивало бы или
	// сокращало окно бюджета.
	windowSecs := int(window / time.Second)

	var d BudgetDecision
	err := s.pool.QueryRow(ctx, `
		INSERT INTO alert_project_budget AS b (project_id, window_start, sent, suppressed, allowed)
		VALUES ($1, now(), 1, 0, true)
		ON CONFLICT (project_id) DO UPDATE SET
			window_start = CASE WHEN b.window_start <= now() - make_interval(secs => $2) THEN now() ELSE b.window_start END,
			sent = CASE
				WHEN b.window_start <= now() - make_interval(secs => $2) THEN 1
				WHEN b.sent < $3 THEN b.sent + 1
				ELSE b.sent END,
			suppressed = CASE
				WHEN b.window_start <= now() - make_interval(secs => $2) THEN b.suppressed
				WHEN b.sent < $3 THEN b.suppressed
				ELSE b.suppressed + 1 END,
			allowed = (b.window_start <= now() - make_interval(secs => $2) OR b.sent < $3)
		RETURNING allowed, suppressed`,
		projectID, windowSecs, limit).Scan(&d.Allowed, &d.Suppressed)
	if err != nil {
		return BudgetDecision{}, fmt.Errorf("alert: claim budget: %w", err)
	}
	if d.Allowed {
		d.Suppressed = 0
	}
	return d, nil
}

// Возвращает место при полном провале Enqueue — иначе бюджет списывался бы
// зря. Ограничен текущим окном; best-effort, ошибку логирует вызывающий.
func (s *Service) refundBudget(ctx context.Context, projectID int64) error {
	window, limit := s.budgetParams()
	if limit <= 0 {
		return nil // ограничение выключено — claimBudget тоже ничего не писал
	}
	windowSecs := int(window / time.Second)
	_, err := s.pool.Exec(ctx, `
		UPDATE alert_project_budget
		SET sent = sent - 1
		WHERE project_id = $1
		  AND window_start > now() - make_interval(secs => $2)
		  AND sent > 0`,
		projectID, windowSecs)
	if err != nil {
		return fmt.Errorf("alert: refund budget: %w", err)
	}
	return nil
}

type SuppressedBatch struct {
	ProjectID  int64
	Suppressed int
	Since      time.Time
}

// Атомарно — чтобы две реплики не разослали одну сводку дважды. Берём
// только по истечении окна, иначе сводка ушла бы с неполным числом.
func (s *Service) ClaimSuppressed(ctx context.Context, limit int) ([]SuppressedBatch, error) {
	window, _ := s.budgetParams()
	// Отсечка — часами базы, как и в claimBudget: window_start пишется её now().
	windowSecs := int(window / time.Second)

	// CTE, не RETURNING у UPDATE: RETURNING отдал бы уже обнулённый счётчик.
	// FOR UPDATE SKIP LOCKED защищает от гонки реплик; JOIN cleared обязателен для CTE.
	rows, err := s.pool.Query(ctx, `
		WITH claimed AS (
			SELECT project_id, suppressed, window_start
			FROM alert_project_budget
			WHERE suppressed > 0 AND window_start <= now() - make_interval(secs => $1)
			ORDER BY project_id
			LIMIT $2
			FOR UPDATE SKIP LOCKED
		), cleared AS (
			UPDATE alert_project_budget b
			SET suppressed = 0, digest_at = now()
			FROM claimed c
			WHERE b.project_id = c.project_id
			RETURNING b.project_id
		)
		SELECT c.project_id, c.suppressed, c.window_start
		FROM claimed c
		JOIN cleared d ON d.project_id = c.project_id
		ORDER BY c.project_id`, windowSecs, limit)
	if err != nil {
		return nil, fmt.Errorf("alert: claim suppressed: %w", err)
	}
	defer rows.Close()

	var out []SuppressedBatch
	for rows.Next() {
		var b SuppressedBatch
		if err := rows.Scan(&b.ProjectID, &b.Suppressed, &b.Since); err != nil {
			return nil, fmt.Errorf("alert: claim suppressed: %w", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// Для тестов и диагностики.
func (s *Service) budgetOf(ctx context.Context, projectID int64) (sent, suppressed int, err error) {
	err = s.pool.QueryRow(ctx,
		`SELECT sent, suppressed FROM alert_project_budget WHERE project_id = $1`,
		projectID).Scan(&sent, &suppressed)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, nil
	}
	return sent, suppressed, err
}
