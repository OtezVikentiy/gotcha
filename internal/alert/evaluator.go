package alert

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"gitflic.ru/otezvikentiy/gotcha/internal/notify"
)

// Event — сигнал об изменении состояния issue, который может породить
// алерт: новая группа (new_issue), переоткрытие resolved-группы
// (regression) или всплеск частоты событий (spike, из Spike-воркера).
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

// Evaluator решает, нужно ли по Event поставить уведомления в очередь:
// находит включённое правило нужного kind для проекта, проверяет троттлинг
// (alert_throttle, ключ issue_id+rule_id) и, если можно слать, ставит по
// одной задаче в Outbox на каждый включённый канал проекта.
type Evaluator struct {
	Svc    *Service
	Outbox *notify.Outbox

	// BaseURL — префикс для ссылки на issue в уведомлении: {BaseURL}/issues/{id}.
	BaseURL string

	// EmailEnabled сообщает, настроен ли SMTP (cfg.SMTPHost != ""). Пока
	// false, email-каналы пропускаются (с warn-логом), чтобы не ставить в
	// очередь задачи, которые notify.Worker всё равно не сможет доставить.
	EmailEnabled bool

	// ExternalDetails управляет тем, раскрывать ли детали ошибки во внешние
	// каналы (Telegram/webhook). Дефолт — false (проставляется в main.go из
	// cfg.ExternalChannelDetails; GOTCHA_EXTERNAL_CHANNEL_DETAILS по умолчанию
	// false): во внешние каналы уходит только ссылка на issue и вид алерта —
	// текст ошибки может нести ПДн, а Telegram/webhook уводят их за пределы
	// РФ (152-ФЗ). true (явное включение оператором) — слать полный payload с
	// title/culprit/level/телом. Email — внутренний SMTP оператора — гейтом
	// не затрагивается.
	ExternalDetails bool
}

// OnIssue — точка входа для ingest.Pipeline (new_issue/regression) и
// alert.Spike (spike). Ошибки логируются и не возвращаются: алертинг не
// должен ронять или блокировать вызывающую сторону (приём событий,
// spike-тик).
func (e *Evaluator) OnIssue(ctx context.Context, ev Event) {
	rule, ok, err := e.Svc.ruleByKind(ctx, ev.ProjectID, ev.Kind)
	if err != nil {
		slog.Error("alert: rule lookup failed", "project_id", ev.ProjectID, "kind", ev.Kind, "error", err)
		return
	}
	if !ok || !rule.Enabled {
		return
	}

	claimed, err := e.claimThrottle(ctx, ev.IssueID, rule.ID, rule.ThrottleMinutes)
	if err != nil {
		slog.Error("alert: throttle claim failed", "issue_id", ev.IssueID, "rule_id", rule.ID, "error", err)
		return
	}
	if !claimed {
		return
	}

	channels, err := e.Svc.Channels(ctx, ev.ProjectID)
	if err != nil {
		slog.Error("alert: channels lookup failed", "project_id", ev.ProjectID, "error", err)
		return
	}

	url := fmt.Sprintf("%s/issues/%d", e.BaseURL, ev.IssueID)
	subject := fmt.Sprintf("[gotcha] %s: %s", ev.Kind, ev.Title)
	body := fmt.Sprintf("%s\n\nCulprit: %s\nLevel: %s\nSeen: %d times\n\n%s",
		ev.Title, ev.Culprit, ev.Level, ev.TimesSeen, url)

	for _, ch := range channels {
		if !ch.Enabled {
			continue
		}
		if ch.Kind == ChannelEmail && !e.EmailEnabled {
			slog.Warn("alert: email channel skipped, SMTP not configured",
				"project_id", ev.ProjectID, "channel_id", ch.ID)
			continue
		}

		payload := map[string]any{
			"kind":         ev.Kind,
			"project_id":   ev.ProjectID,
			"issue_id":     ev.IssueID,
			"title":        ev.Title,
			"culprit":      ev.Culprit,
			"level":        ev.Level,
			"times_seen":   ev.TimesSeen,
			"url":          url,
			"subject":      subject,
			"body":         body,
			"channel_kind": ch.Kind,
			"target":       ch.Target,
			"secret":       ch.Secret,
		}
		if !e.ExternalDetails && (ch.Kind == ChannelTelegram || ch.Kind == ChannelWebhook) {
			// Обезличиваем payload для внешних каналов: без title/culprit/
			// level и без текста ошибки в теле/subject — только маршрутные
			// поля, ссылка на issue и вид алерта (см. Evaluator.ExternalDetails
			// и notify.RedactExternalPayload — тот же гейт во всех нотифаерах).
			payload = notify.RedactExternalPayload(payload)
		}
		if err := e.Outbox.Enqueue(ctx, ch.ID, payload); err != nil {
			slog.Error("alert: enqueue failed", "channel_id", ch.ID, "error", err)
		}
	}
}

// ruleByKind возвращает единственное правило (project_id, kind) —
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

// claimThrottle atomically checks the troттлинг-окно and, if we're allowed to
// send, records last_sent_at = now() in the same statement. Doing the
// check-and-mark as a single INSERT ... ON CONFLICT ... DO UPDATE ... WHERE
// closes the race between "read: are we outside the throttle window" and
// "write: record that we sent": without it, two concurrent OnIssue calls for
// the same (issueID, ruleID) — e.g. the documented Upsert race on the very
// first event of a fingerprint, where two pipeline workers can both observe
// New=true for the same issue — could both read "not throttled" before
// either commits, and both enqueue a full round of channel jobs.
//
// No row yet -> INSERT succeeds unconditionally -> claimed. Row exists and
// last_sent_at <= cutoff (throttle window elapsed) -> ON CONFLICT DO UPDATE
// fires -> claimed. Row exists and last_sent_at > cutoff (still throttled)
// -> the WHERE condition excludes the update -> RETURNING yields no row ->
// not claimed. throttleMinutes=0 means cutoff=now(), and last_sent_at is
// always <= the moment it was written, so a prior send never blocks the next
// one (no throttling), matching the documented "0 means no throttle" rule.
func (e *Evaluator) claimThrottle(ctx context.Context, issueID, ruleID int64, throttleMinutes int) (bool, error) {
	cutoff := time.Now().Add(-time.Duration(throttleMinutes) * time.Minute)
	var claimed int
	err := e.Svc.pool.QueryRow(ctx, `
		INSERT INTO alert_throttle (issue_id, rule_id, last_sent_at)
		VALUES ($1, $2, now())
		ON CONFLICT (issue_id, rule_id) DO UPDATE SET last_sent_at = now()
		WHERE alert_throttle.last_sent_at <= $3
		RETURNING 1`,
		issueID, ruleID, cutoff).Scan(&claimed)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("alert: claim throttle: %w", err)
	}
	return true, nil
}
