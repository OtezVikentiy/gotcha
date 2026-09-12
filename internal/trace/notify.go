package trace

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/alert"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/notify"
)

const (
	MaxPerfAlertsPerHour = 10

	perfAlertWindow = time.Hour
)

type OutboxNotifier struct {
	Alerts *alert.Service
	Outbox *notify.Outbox

	// обязателен: без него троттлинг недоступен, notify вернёт ошибку, а не разошлёт без ограничений.
	Pool *pgxpool.Pool

	// ссылка в уведомлении собирается как значение + /perf-issues/{id}.
	BaseURL string

	EmailEnabled bool

	// нулевое значение не раскрывает деталей никому.
	Details alert.DetailPolicy

	// локаль инстанса (GOTCHA_LOCALE), не запроса: у внешнего получателя своей локали нет.
	Locale i18n.Locale
}

// логика повторена в internal/web/templates/perfissues.templ — общего кода нет, чтобы не тянуть импорт web.
func perfIssueNotifyTitle(ctx context.Context, iss PerfIssue) string {
	param := iss.Description
	if iss.Kind == KindHTTPFlood {
		param = iss.Culprit
	}
	if param == "" {
		if iss.Title != "" {
			return iss.Title
		}
		return i18n.T(ctx, "perf.title."+iss.Kind)
	}
	return i18n.T(ctx, "perf.title."+iss.Kind) + ": " + param
}

// вызывается только при первом обнаружении — иначе алерт шёл бы на каждый повтор.
func (n *OutboxNotifier) NotifyNew(ctx context.Context, projectID int64, iss PerfIssue) error {
	return n.notify(ctx, projectID, iss, false)
}

// вызывается при повторном обнаружении после resolved — тихое переоткрытие обманет дежурного.
func (n *OutboxNotifier) NotifyRegression(ctx context.Context, projectID int64, iss PerfIssue) error {
	return n.notify(ctx, projectID, iss, true)
}

// ошибка Enqueue по одному каналу не прерывает остальные — все они собираются через errors.Join.
func (n *OutboxNotifier) notify(ctx context.Context, projectID int64, iss PerfIssue, regression bool) error {
	if n.Pool == nil {
		return errors.New("trace: notify: nil pool, perf alert throttle unavailable")
	}

	channels, err := n.Alerts.Channels(ctx, projectID)
	if err != nil {
		return fmt.Errorf("trace: notify: project channels: %w", err)
	}
	// каналы читаются до клейма слота: проект без включённых каналов не должен жечь часовой лимит.
	deliverable := false
	for _, ch := range channels {
		if ch.Enabled && (ch.Kind != alert.ChannelEmail || n.EmailEnabled) {
			deliverable = true
			break
		}
	}
	if !deliverable {
		return nil
	}

	claimed, err := n.claimAlert(ctx, projectID)
	if err != nil {
		return fmt.Errorf("trace: notify: claim throttle: %w", err)
	}
	if !claimed {
		// throttled ≠ потеряно: проблема уже записана в perf_issues, только не разослана.
		slog.Warn("perf alert throttled, issue recorded but not delivered",
			"project_id", projectID, "perf_issue_id", iss.ID, "kind", iss.Kind,
			"culprit", iss.Culprit, "regression", regression, "limit_per_hour", MaxPerfAlertsPerHour)
		return nil
	}

	lctx := i18n.WithLocale(ctx, n.Locale)
	title := perfIssueNotifyTitle(lctx, iss)
	url := fmt.Sprintf("%s/perf-issues/%d", n.BaseURL, iss.ID)
	subject := i18n.Tf(lctx, "notify.perf.subject", "title", title)
	if regression {
		subject = i18n.Tf(lctx, "notify.perf.subject.regression", "title", title)
	}
	body := i18n.Tf(lctx, "notify.perf.body", "title", title,
		"culprit", iss.Culprit, "count", strconv.FormatInt(iss.Count, 10), "url", url)

	var errs error
	enqueued := 0
	for _, ch := range channels {
		if !ch.Deliverable() {
			continue
		}
		if ch.Kind == alert.ChannelEmail && !n.EmailEnabled {
			slog.Warn("trace: email channel skipped, SMTP not configured",
				"project_id", projectID, "channel_id", ch.ID)
			continue
		}

		payload := map[string]any{
			"kind":          iss.Kind,
			"regression":    regression,
			"project_id":    projectID,
			"perf_issue_id": iss.ID,
			"title":         title,
			"culprit":       iss.Culprit,
			"count":         iss.Count,
			"url":           url,
			"subject":       subject,
			"body":          body,
			"channel_kind":  ch.Kind,
			"target":        ch.Target,
			// секрета в payload нет намеренно — plain jsonb обесценил бы шифрование alert_channels.secret.
		}
		if !n.Details.AllowsDetails(ch) {
			payload = notify.RedactExternalPayload(lctx, payload)
		}
		if err := n.Outbox.Enqueue(ctx, ch.ID, payload); err != nil {
			slog.Error("trace: notify: enqueue failed", "channel_id", ch.ID, "error", err)
			errs = errors.Join(errs, fmt.Errorf("trace: notify: enqueue channel %d: %w", ch.ID, err))
			continue
		}
		enqueued++
	}
	// слот занимается до Enqueue, чтобы воркеры не обошли лимит гонкой; если не встало
	// ни одного сообщения — слот освобождается.
	if enqueued == 0 && errs != nil {
		if err := n.releaseAlert(ctx, projectID); err != nil {
			slog.Error("trace: notify: release throttle slot", "project_id", projectID, "error", err)
		}
	}
	return errs
}

// best-effort: неудачный release просто теряет один слот из часового лимита, не ломает корректность.
func (n *OutboxNotifier) releaseAlert(ctx context.Context, projectID int64) error {
	cutoff := time.Now().Add(-perfAlertWindow)
	_, err := n.Pool.Exec(ctx, `
		UPDATE perf_alert_throttle SET sent = sent - 1
		WHERE project_id = $1 AND window_start > $2 AND sent > 0`,
		projectID, cutoff)
	if err != nil {
		return fmt.Errorf("trace: release perf alert slot: %w", err)
	}
	return nil
}

// ON CONFLICT берёт блокировку строки — параллельные клеймы одного проекта сериализуются,
// лимит соблюдается точно, не приблизительно.
func (n *OutboxNotifier) claimAlert(ctx context.Context, projectID int64) (bool, error) {
	cutoff := time.Now().Add(-perfAlertWindow)
	var sent int
	err := n.Pool.QueryRow(ctx, `
		INSERT INTO perf_alert_throttle (project_id, window_start, sent)
		VALUES ($1, now(), 1)
		ON CONFLICT (project_id) DO UPDATE SET
			window_start = CASE WHEN perf_alert_throttle.window_start <= $2
			                    THEN now() ELSE perf_alert_throttle.window_start END,
			sent         = CASE WHEN perf_alert_throttle.window_start <= $2
			                    THEN 1 ELSE perf_alert_throttle.sent + 1 END
		WHERE perf_alert_throttle.window_start <= $2 OR perf_alert_throttle.sent < $3
		RETURNING sent`,
		projectID, cutoff, MaxPerfAlertsPerHour).Scan(&sent)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil // лимит проекта на час выбран
		}
		return false, err
	}
	return true, nil
}
