package alert

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"gitflic.ru/otezvikentiy/gotcha/internal/escalation"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/notify"
)

// Может породить алерт: новая группа (new_issue), переоткрытие
// (regression), всплеск частоты (spike, из Spike-воркера).
type Event struct {
	ProjectID int64
	IssueID   int64
	Kind      string // new_issue | regression | spike

	Title     string
	Culprit   string
	Level     string
	IssueURL  string
	TimesSeen int64
}

type Evaluator struct {
	Svc    *Service
	Outbox *notify.Outbox

	// Префикс для ссылки на issue в уведомлении: {BaseURL}/issues/{id}.
	BaseURL string

	// Настроен ли SMTP; пока false, email-каналы пропускаются, чтобы не
	// ставить в очередь недоставимые задачи.
	EmailEnabled bool

	// Нулевое значение не доверяет никому — детали уходят только тем, кого
	// оператор подтвердил как свой контур.
	Details DetailPolicy

	// GOTCHA_LOCALE инстанса — внешний канал не знает языка получателя, язык
	// уведомления выбирает оператор.
	Locale i18n.Locale

	// nil — окна не подавляют. У issue-алертов нет флага записи, поэтому гейт
	// стоит ДО claimThrottle/claimBudget, а не флагом на записи.
	Maint MaintenanceChecker

	// nil-совместим (escalation.ProjectNamer) — тогда уведомления идут без
	// имени проекта.
	Projects escalation.ProjectNamer
}

// Enum закрыт (см. Event.Kind); незнакомый вид уходит как есть — честнее,
// чем прятать его за пустой строкой.
func issueAlertKindLabel(ctx context.Context, kind string) string {
	switch kind {
	case "new_issue", "regression", "spike":
		return i18n.T(ctx, "notify.issue.kind."+kind)
	default:
		return kind
	}
}

// Ошибки логируются и не возвращаются — алертинг не должен ронять или
// блокировать вызывающую сторону.
func (e *Evaluator) OnIssue(ctx context.Context, ev Event) {
	rule, ok, err := e.Svc.ruleByKind(ctx, ev.ProjectID, ev.Kind)
	if err != nil {
		slog.Error("alert: rule lookup failed", "project_id", ev.ProjectID, "kind", ev.Kind, "error", err)
		return
	}
	if !ok || !rule.Enabled {
		return
	}

	if e.Maint != nil {
		if inMaint, err := e.Maint.InMaintenance(ctx, ev.ProjectID, time.Now()); err != nil {
			slog.Error("alert: maintenance check failed", "project_id", ev.ProjectID, "issue_id", ev.IssueID, "error", err)
		} else if inMaint {
			return // окно обслуживания: не жжём throttle/budget, не в suppressed-digest
		}
	}

	channels, err := e.Svc.Channels(ctx, ev.ProjectID)
	if err != nil {
		slog.Error("alert: channels lookup failed", "project_id", ev.ProjectID, "error", err)
		return
	}

	// Сколько каналов стоило слать (Deliverable + email-fallback) — если ни
	// одного, throttle/budget не трогаем вовсе: списывать не за что.
	deliverableCount := 0
	for _, ch := range channels {
		if !ch.Deliverable() {
			continue
		}
		if ch.Kind == ChannelEmail && !e.EmailEnabled {
			continue
		}
		deliverableCount++
	}
	if deliverableCount == 0 {
		return // некуда доставлять: не жжём throttle/budget, не в suppressed-digest
	}

	claimed, err := e.claimThrottle(ctx, ev.IssueID, rule.ID, rule.ThrottleMinutes)
	if err != nil {
		slog.Error("alert: throttle claim failed", "issue_id", ev.IssueID, "rule_id", rule.ID, "error", err)
		return
	}
	if !claimed {
		return
	}

	// Троттлинг ключуется (issue_id, rule_id) — у НОВОГО issue строки там нет,
	// он проходит всегда; подавленное не теряется, Digester шлёт сводку.
	budget, err := e.Svc.claimBudget(ctx, ev.ProjectID)
	if err != nil {
		slog.Error("alert: budget claim failed", "project_id", ev.ProjectID, "error", err)
		return
	}
	if !budget.Allowed {
		slog.Warn("alert: project notification budget exhausted, alert suppressed",
			"project_id", ev.ProjectID, "issue_id", ev.IssueID,
			"suppressed_in_window", budget.Suppressed)
		return
	}

	// Язык инстанса (GOTCHA_LOCALE), не запроса — у внешнего получателя нет
	// своей локали.
	lctx := i18n.WithLocale(ctx, e.Locale)
	url := fmt.Sprintf("%s/issues/%d", e.BaseURL, ev.IssueID)
	subject := i18n.Tf(lctx, "notify.issue.subject",
		"kind", issueAlertKindLabel(lctx, ev.Kind), "title", ev.Title)
	body := i18n.Tf(lctx, "notify.issue.body",
		"title", ev.Title, "culprit", ev.Culprit, "level", ev.Level,
		"count", strconv.FormatInt(ev.TimesSeen, 10), "url", url)

	dchans := make([]escalation.DispatchChannel, 0, len(channels))
	for _, ch := range channels {
		dchans = append(dchans, escalation.DispatchChannel{
			ID: ch.ID, Kind: ch.Kind, Target: ch.Target,
			IsEmail:       ch.Kind == ChannelEmail,
			Deliverable:   ch.Deliverable(),
			AllowsDetails: e.Details.AllowsDetails(ch),
		})
	}

	// lctx, не ctx: Dispatch берёт локаль уведомления из ctx для
	// WithProjectSubject/WithProjectBody и RedactExternalPayload.
	enqueuedIDs, err := escalation.Dispatch(lctx,
		escalation.DispatchDeps{Outbox: e.Outbox, EmailEnabled: e.EmailEnabled, Projects: e.Projects, LogTag: "alert"},
		escalation.DispatchInput{
			ProjectID: ev.ProjectID, Kind: ev.Kind, Subject: subject, Body: body,
			URL: url,
			Extra: map[string]any{
				"issue_id":   ev.IssueID,
				"title":      ev.Title,
				"culprit":    ev.Culprit,
				"level":      ev.Level,
				"times_seen": ev.TimesSeen,
			},
			Channels: dchans,
		})
	if err != nil {
		// Dispatch уже залогировал каждый канал отдельно — здесь просто отбрасываем.
		slog.Error("alert: dispatch failed", "project_id", ev.ProjectID, "issue_id", ev.IssueID, "error", err)
	}
	enqueued := len(enqueuedIDs)

	// Откат — только при полном провале доставки: частичный успех уже
	// доставлен, откатывать нечего.
	if enqueued == 0 {
		if err := e.releaseThrottle(ctx, ev.IssueID, rule.ID); err != nil {
			slog.Error("alert: release throttle after full enqueue failure",
				"issue_id", ev.IssueID, "rule_id", rule.ID, "error", err)
		}
		if err := e.Svc.refundBudget(ctx, ev.ProjectID); err != nil {
			slog.Error("alert: refund budget after full enqueue failure",
				"project_id", ev.ProjectID, "error", err)
		}
	}
}

// Вызывается сразу после своего claimThrottle — гонка с чужим claim
// исключена, кроме throttleMinutes=0, где откат best-effort.
func (e *Evaluator) releaseThrottle(ctx context.Context, issueID, ruleID int64) error {
	if _, err := e.Svc.pool.Exec(ctx,
		`DELETE FROM alert_throttle WHERE issue_id = $1 AND rule_id = $2`,
		issueID, ruleID); err != nil {
		return fmt.Errorf("alert: release throttle: %w", err)
	}
	return nil
}

// UNIQUE(project_id, kind) гарантирует не более одной строки.
func (s *Service) ruleByKind(ctx context.Context, projectID int64, kind string) (Rule, bool, error) {
	var r Rule
	err := s.pool.QueryRow(ctx, `
		SELECT id, project_id, kind, enabled, threshold, window_minutes, throttle_minutes
		FROM alert_rules WHERE project_id = $1 AND kind = $2`, projectID, kind).
		Scan(&r.ID, &r.ProjectID, &r.Kind, &r.Enabled, &r.Threshold, &r.WindowMinutes, &r.ThrottleMinutes)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Rule{}, false, nil
		}
		return Rule{}, false, fmt.Errorf("alert: rule by kind: %w", err)
	}
	return r, true, nil
}

// Атомарный INSERT ON CONFLICT ... WHERE — закрывает гонку «проверить
// окно»/«записать last_sent_at» между конкурентными OnIssue для одной пары.
func (e *Evaluator) claimThrottle(ctx context.Context, issueID, ruleID int64, throttleMinutes int) (bool, error) {
	// Часы БАЗЫ, не процесса — иначе расхождение часов растягивало бы или
	// сокращало окно троттлинга.
	var claimed int
	err := e.Svc.pool.QueryRow(ctx, `
		INSERT INTO alert_throttle (issue_id, rule_id, last_sent_at)
		VALUES ($1, $2, now())
		ON CONFLICT (issue_id, rule_id) DO UPDATE SET last_sent_at = now()
		WHERE alert_throttle.last_sent_at <= now() - make_interval(mins => $3)
		RETURNING 1`,
		issueID, ruleID, throttleMinutes).Scan(&claimed)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("alert: claim throttle: %w", err)
	}
	return true, nil
}
