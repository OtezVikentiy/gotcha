// Package escalation — политика эскалации инцидентов: лесенка шагов
// (задержка от открытия → набор каналов) на (проект, severity), и
// дефолт-fallback для проектов, где лесенка ещё не настраивалась.
package escalation

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Severity инцидента — совпадает с CHECK-ограничением escalation_steps.severity
// и severity-колонками пяти инцидент-таблиц (0077).
const (
	SeverityCritical = "critical"
	SeverityWarning  = "warning"
)

// ErrInvalidPolicy — лесенка или её параметры не прошли валидацию до похода в БД.
var ErrInvalidPolicy = errors.New("escalation: invalid policy")

// Step — одна ступень лесенки: через DelayMinutes от открытия инцидента
// уведомление уходит в каналы ChannelIDs (порядок не важен).
type Step struct {
	StepNo       int
	DelayMinutes int
	ChannelIDs   []int64
}

// Ladder — лесенка эскалации, отсортированная по StepNo возрастанию.
type Ladder []Step

// PolicyStore — CRUD над политикой эскалации проекта.
type PolicyStore struct {
	pool *pgxpool.Pool
}

// NewPolicyStore создаёт стор политики эскалации поверх пула PostgreSQL.
func NewPolicyStore(pool *pgxpool.Pool) *PolicyStore {
	return &PolicyStore{pool: pool}
}

func validSeverity(severity string) bool {
	switch severity {
	case SeverityCritical, SeverityWarning:
		return true
	default:
		return false
	}
}

// ValidateSteps проверяет лесенку до похода в БД: severity задаётся отдельным
// аргументом SetLadder и здесь не проверяется. Пустая лесенка допустима — она
// означает «политика не настроена, использовать дефолт-fallback» и валидна
// сама по себе.
//
// Непустая лесенка обязана иметь step_no 0..N без дыр и дублей (значит и
// ступень 0 обязательна), delay_minutes >= 0, и хотя бы один канал на
// ступень — ступень без каналов бессмысленна при явной настройке. Пустой
// ChannelIDs — это только про дефолт-fallback, когда у проекта вообще нет
// каналов, а не про настроенную ступень (см. Ladder).
func ValidateSteps(steps []Step) error {
	if len(steps) == 0 {
		return nil
	}
	sorted := make([]Step, len(steps))
	copy(sorted, steps)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].StepNo < sorted[j].StepNo })
	for i, st := range sorted {
		if st.StepNo != i {
			return fmt.Errorf("%w: step_no must be sequential 0..N without gaps or duplicates, got %d at position %d",
				ErrInvalidPolicy, st.StepNo, i)
		}
		if st.DelayMinutes < 0 {
			return fmt.Errorf("%w: delay_minutes must be >= 0, got %d for step %d",
				ErrInvalidPolicy, st.DelayMinutes, st.StepNo)
		}
		if len(st.ChannelIDs) == 0 {
			return fmt.Errorf("%w: step %d has no channels", ErrInvalidPolicy, st.StepNo)
		}
	}
	return nil
}

// Ladder возвращает лесенку эскалации проекта для данной severity.
//
// Если для (project_id, severity) нет настроенных шагов — БЛОКЕР-1:
// возвращает дефолт-лесенку из ОДНОЙ ступени (step0, delay0) со всеми
// enabled-каналами проекта. Это ровно старое поведение до появления
// эскалаций: уведомление уходит сразу и во все включённые каналы.
// Deliverable/SecretBroken-фильтр остаётся на отправке, не здесь.
func (s *PolicyStore) Ladder(ctx context.Context, projectID int64, severity string) (Ladder, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT es.step_no, es.delay_minutes, esc.channel_id
		FROM escalation_steps es
		LEFT JOIN escalation_step_channels esc ON esc.step_id = es.id
		WHERE es.project_id = $1 AND es.severity = $2
		ORDER BY es.step_no, esc.channel_id`, projectID, severity)
	if err != nil {
		return nil, fmt.Errorf("escalation: ladder: %w", err)
	}
	defer rows.Close()

	var ladder Ladder
	stepIndex := map[int]int{}
	for rows.Next() {
		var stepNo, delay int
		var channelID *int64
		if err := rows.Scan(&stepNo, &delay, &channelID); err != nil {
			return nil, fmt.Errorf("escalation: ladder: %w", err)
		}
		idx, ok := stepIndex[stepNo]
		if !ok {
			ladder = append(ladder, Step{StepNo: stepNo, DelayMinutes: delay})
			idx = len(ladder) - 1
			stepIndex[stepNo] = idx
		}
		if channelID != nil {
			ladder[idx].ChannelIDs = append(ladder[idx].ChannelIDs, *channelID)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("escalation: ladder: %w", err)
	}
	if len(ladder) == 0 {
		return s.defaultLadder(ctx, projectID)
	}
	return ladder, nil
}

// defaultLadder — старое поведение (до эскалаций): одна ступень delay0=0 со
// всеми enabled-каналами проекта. Каналов может не быть вовсе — тогда
// ChannelIDs пуст, и Enqueue просто ничего не отправит, как и сегодня.
func (s *PolicyStore) defaultLadder(ctx context.Context, projectID int64) (Ladder, error) {
	rows, err := s.pool.Query(ctx,
		"SELECT id FROM alert_channels WHERE project_id = $1 AND enabled ORDER BY id", projectID)
	if err != nil {
		return nil, fmt.Errorf("escalation: default ladder: %w", err)
	}
	defer rows.Close()
	var channelIDs []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("escalation: default ladder: %w", err)
		}
		channelIDs = append(channelIDs, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("escalation: default ladder: %w", err)
	}
	return Ladder{{StepNo: 0, DelayMinutes: 0, ChannelIDs: channelIDs}}, nil
}

// Ladders возвращает лесенки обеих severity проекта — для редактора политики
// (UI T8), чтобы показать текущее эффективное поведение сразу для critical и
// warning. Severity без настроенных шагов приходит как дефолт-fallback, а не
// пустая лесенка.
func (s *PolicyStore) Ladders(ctx context.Context, projectID int64) (map[string]Ladder, error) {
	out := make(map[string]Ladder, 2)
	for _, severity := range []string{SeverityCritical, SeverityWarning} {
		ladder, err := s.Ladder(ctx, projectID, severity)
		if err != nil {
			return nil, err
		}
		out[severity] = ladder
	}
	return out, nil
}

// verifyChannelsBelongToProject проверяет, что каждый channel_id, упомянутый
// в steps, принадлежит projectID — а не другому проекту той же (или чужой)
// организации. Дедуплицирует id перед запросом: одна и та же ступень нередко
// ссылается на канал, уже встретившийся в другой ступени.
func verifyChannelsBelongToProject(ctx context.Context, tx pgx.Tx, projectID int64, steps []Step) error {
	seen := map[int64]bool{}
	var ids []int64
	for _, st := range steps {
		for _, id := range st.ChannelIDs {
			if !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
	}
	if len(ids) == 0 {
		return nil
	}
	rows, err := tx.Query(ctx,
		"SELECT id FROM alert_channels WHERE project_id = $1 AND id = ANY($2)", projectID, ids)
	if err != nil {
		return fmt.Errorf("escalation: set ladder: verify channels: %w", err)
	}
	defer rows.Close()
	owned := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return fmt.Errorf("escalation: set ladder: verify channels: %w", err)
		}
		owned[id] = true
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("escalation: set ladder: verify channels: %w", err)
	}
	for _, id := range ids {
		if !owned[id] {
			return fmt.Errorf("%w: channel %d does not belong to project %d", ErrInvalidPolicy, id, projectID)
		}
	}
	return nil
}

// SetLadder транзакционно заменяет лесенку эскалации проекта для данной
// severity: старые шаги (и их каналы, каскадом FK) удаляются, новые
// вставляются целиком. Пустой steps допустим — снимает настроенную лесенку,
// возвращая проект к дефолт-fallback.
func (s *PolicyStore) SetLadder(ctx context.Context, projectID int64, severity string, steps []Step) error {
	if !validSeverity(severity) {
		return fmt.Errorf("%w: invalid severity %q", ErrInvalidPolicy, severity)
	}
	if err := ValidateSteps(steps); err != nil {
		return err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("escalation: set ladder: %w", err)
	}
	defer tx.Rollback(ctx)

	// Defense-in-depth (T9, concern T2): хендлер веб-слоя уже фильтрует
	// channel_id формы по каналам ПРОЕКТА до вызова SetLadder, но эта
	// проверка — единственная, которую нельзя обойти ни забытым фильтром на
	// новом вызывающем, ни прямым вызовом стора в обход веб-слоя. Без неё
	// оператор проекта A мог бы подобрать channel_id чужого проекта B и
	// прицепить его к своей лесенке — уведомления инцидентов проекта A
	// улетели бы в канал, которым владеет и управляет B.
	if err := verifyChannelsBelongToProject(ctx, tx, projectID, steps); err != nil {
		return err
	}

	if _, err := tx.Exec(ctx,
		"DELETE FROM escalation_steps WHERE project_id = $1 AND severity = $2", projectID, severity); err != nil {
		return fmt.Errorf("escalation: set ladder: delete: %w", err)
	}
	for _, st := range steps {
		var stepID int64
		if err := tx.QueryRow(ctx, `
			INSERT INTO escalation_steps (project_id, severity, step_no, delay_minutes)
			VALUES ($1, $2, $3, $4) RETURNING id`,
			projectID, severity, st.StepNo, st.DelayMinutes).Scan(&stepID); err != nil {
			return fmt.Errorf("escalation: set ladder: insert step: %w", err)
		}
		for _, channelID := range st.ChannelIDs {
			if _, err := tx.Exec(ctx,
				"INSERT INTO escalation_step_channels (step_id, channel_id) VALUES ($1, $2)",
				stepID, channelID); err != nil {
				return fmt.Errorf("escalation: set ladder: insert channel: %w", err)
			}
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("escalation: set ladder: commit: %w", err)
	}
	return nil
}
