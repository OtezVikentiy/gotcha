package alert

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Потолок уведомлений на проект за окно.
//
// Дефолты щедрые нарочно: цель — срезать размножение, а не мешать настоящему
// инциденту. Проект, у которого разом упало полтора десятка сервисов, должен
// получить все уведомления; проект, которому кто-то шлёт уникальный fingerprint
// на каждое событие, — не должен утопить почту участников.
const (
	defaultAlertBudgetWindow = time.Hour
	defaultAlertBudgetLimit  = 50
)

// BudgetDecision — исход попытки занять место под уведомление.
type BudgetDecision struct {
	// Allowed — можно слать.
	Allowed bool
	// Suppressed — сколько уведомлений подавлено в текущем окне НАКОПИТЕЛЬНО,
	// включая это. Ноль при Allowed.
	Suppressed int
}

// SetBudget задаёт окно и потолок. Нулевой или отрицательный потолок ВЫКЛЮЧАЕТ
// ограничение целиком — для инсталляции с доверенными отправителями это
// осознанный выбор оператора, а не аварийный режим.
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

// claimBudget занимает место под одно уведомление проекта.
//
// Решение и учёт — ОДНИМ оператором, как в claimThrottle и по той же причине:
// иначе между «проверил, бюджет есть» и «списал» встаёт гонка, а конкурентные
// вызовы здесь штатны (пайплайн приёма многопоточный, и на первом событии
// нового fingerprint два воркера могут одновременно увидеть New=true).
//
// Окно скользит не по расписанию, а по первому обращению после истечения:
// ON CONFLICT сам решает, продлевать текущее окно или начать новое. Поэтому
// проект без трафика не занимает ни строки в планировщике, ни такта в цикле.
//
// suppressed НЕ обнуляется здесь — его забирает Digester, чтобы сводка
// «подавлено ещё N» не потерялась вместе со сбросом окна.
func (s *Service) claimBudget(ctx context.Context, projectID int64) (BudgetDecision, error) {
	window, limit := s.budgetParams()
	if limit <= 0 {
		return BudgetDecision{Allowed: true}, nil // ограничение выключено
	}
	// Отсечка окна вычисляется часами БАЗЫ: window_start пишется её now(), и
	// сравнение с моментом от часов процесса зависело бы от их расхождения —
	// отстающие часы растягивали окно бюджета, опережающие сокращали.
	// Длительность передаётся секундами, потому что интервал приходит из
	// конфига именно как длительность, а не как момент.
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

// SuppressedBatch — подавленные уведомления одного проекта, готовые к сводке.
type SuppressedBatch struct {
	ProjectID  int64
	Suppressed int
	Since      time.Time
}

// ClaimSuppressed забирает накопленные подавленные уведомления проектов, у
// которых окно уже истекло, и обнуляет счётчик — атомарно, чтобы две реплики
// не разослали одну сводку дважды.
//
// Забираем ТОЛЬКО по истечении окна: иначе сводка ушла бы посреди всплеска и
// сообщила бы неполное число, а следом пришла бы вторая с остатком.
func (s *Service) ClaimSuppressed(ctx context.Context, limit int) ([]SuppressedBatch, error) {
	window, _ := s.budgetParams()
	// Отсечка — часами базы, как и в claimBudget: window_start пишется её now().
	windowSecs := int(window / time.Second)

	// Значение забирается ДО обнуления, поэтому через CTE, а не RETURNING у
	// UPDATE: RETURNING отдаёт НОВУЮ строку, то есть уже обнулённый счётчик, и
	// сводка ушла бы с числом «подавлено 0».
	//
	// FOR UPDATE SKIP LOCKED в claimed — чтобы две реплики не разослали одну
	// сводку дважды: строку забирает та, что успела первой, вторая её
	// пропускает. JOIN с cleared обязателен: он заставляет data-modifying CTE
	// выполниться и связывает выдачу с фактически обнулёнными строками.
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

// budgetOf — текущее состояние бюджета проекта. Для тестов и диагностики.
func (s *Service) budgetOf(ctx context.Context, projectID int64) (sent, suppressed int, err error) {
	err = s.pool.QueryRow(ctx,
		`SELECT sent, suppressed FROM alert_project_budget WHERE project_id = $1`,
		projectID).Scan(&sent, &suppressed)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, nil
	}
	return sent, suppressed, err
}
