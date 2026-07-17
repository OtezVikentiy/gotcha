package trace

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/alert"
	"gitflic.ru/otezvikentiy/gotcha/internal/notify"
)

// Троттлинг алертов о производительности. У алертов об ошибках гейтов два
// (alert_rules и throttle_minutes через alert.Evaluator.claimThrottle), у
// алертов uptime — переходы состояния инцидента. У perf-алертов не было
// НИЧЕГО: каждая новая пара (project_id, fingerprint) ставила задачу в outbox на
// каждый канал. Библиотека фич-флагов, читающая Redis в цикле на двух сотнях
// эндпойнтов, дала бы две сотни сообщений дежурному за первые минуты после
// включения детекции.
//
// Окно «прыгающее» (tumbling), а не скользящее: одна строка на проект вместо
// строки на каждый алерт, и клейм — один атомарный INSERT ... ON CONFLICT (как
// в claimThrottle). Строки perf_issues при этом создаются ВСЕГДА — ограничивается
// только рассылка, и всё, что не поместилось, попадает в лог (молчаливый дроп
// читался бы как «проблем больше нет»).
const (
	// MaxPerfAlertsPerHour экспортирован: на него смотрят тесты пакета и по нему
	// же считается «сколько осталось» в логе.
	MaxPerfAlertsPerHour = 10

	perfAlertWindow = time.Hour
)

// OutboxNotifier — алерты о проблемах производительности поверх того же
// notify.Outbox и тех же каналов проекта, что и алерты об ошибках
// (alert.Evaluator) и об инцидентах uptime (uptime.OutboxNotifier): формат
// payload намеренно совпадает с ними — его читает notify.Worker
// (channel_kind/target/secret), а доставляют те же Sender'ы.
type OutboxNotifier struct {
	Alerts *alert.Service // каналы проекта: Alerts.Channels(projectID)
	Outbox *notify.Outbox

	// Pool — та же PG, что под Alerts и Outbox: в ней живёт perf_alert_throttle
	// (см. claimAlert). Обязателен: без него рассылка ничем не ограничена, и
	// notify возвращает ошибку, а не тихо шлёт всё подряд.
	Pool *pgxpool.Pool

	// BaseURL — префикс ссылки на проблему в уведомлении:
	// {BaseURL}/perf-issues/{id}.
	BaseURL string

	// EmailEnabled — см. alert.Evaluator.EmailEnabled: пока false,
	// email-каналы пропускаются (с warn-логом), чтобы не ставить в очередь
	// задачи, которые notify.Worker всё равно не сможет доставить.
	EmailEnabled bool

	// ExternalDetails — см. alert.Evaluator.ExternalDetails: при false во
	// внешние каналы (Telegram/webhook) уходит обезличенный payload без
	// iss.Title/iss.Culprit (имя транзакции, текст SQL — потенциальные ПДн за
	// пределами РФ, 152-ФЗ). true (дефолт из cfg) — поведение прежнее.
	ExternalDetails bool
}

// NotifyNew ставит по одной задаче в Outbox на каждый включённый канал проекта.
// Зовётся ТОЛЬКО при первом обнаружении проблемы (RecordResult.Created):
// проблема производительности воспроизводится на каждом запросе к эндпойнту, и
// алерт на каждое повторение был бы лавиной, которая хуже молчания.
func (n *OutboxNotifier) NotifyNew(ctx context.Context, projectID int64, iss PerfIssue) error {
	return n.notify(ctx, projectID, iss, false)
}

// NotifyRegression — проблема была помечена resolved и обнаружена снова
// (RecordResult.Regression). Молча переоткрывать её нельзя: для дежурного
// «починили и сломалось опять» — такое же событие, как новая проблема (так же
// устроены алерты об ошибках, alert.KindRegression).
func (n *OutboxNotifier) NotifyRegression(ctx context.Context, projectID int64, iss PerfIssue) error {
	return n.notify(ctx, projectID, iss, true)
}

// notify — общая постановка задач в Outbox. Ошибка Enqueue по одному каналу не
// прерывает постановку остальных: все такие ошибки логируются и собираются через
// errors.Join (как в uptime.OutboxNotifier).
func (n *OutboxNotifier) notify(ctx context.Context, projectID int64, iss PerfIssue, regression bool) error {
	if n.Pool == nil {
		return errors.New("trace: notify: nil pool, perf alert throttle unavailable")
	}

	channels, err := n.Alerts.Channels(ctx, projectID)
	if err != nil {
		return fmt.Errorf("trace: notify: project channels: %w", err)
	}
	// Каналы читаются ДО клейма слота: проект без включённых каналов не должен
	// выжигать часовой лимит алертами, которые всё равно некуда доставлять.
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
		// Не молчим: проблема ЗАПИСАНА (perf_issues), просто не разослана — иначе
		// дежурный, увидев тишину, решил бы, что новых проблем нет.
		slog.Warn("perf alert throttled, issue recorded but not delivered",
			"project_id", projectID, "perf_issue_id", iss.ID, "title", iss.Title,
			"regression", regression, "limit_per_hour", MaxPerfAlertsPerHour)
		return nil
	}

	url := fmt.Sprintf("%s/perf-issues/%d", n.BaseURL, iss.ID)
	subject := fmt.Sprintf("[Gotcha] Performance: %s", iss.Title)
	if regression {
		subject = fmt.Sprintf("[Gotcha] Performance regression: %s", iss.Title)
	}
	body := fmt.Sprintf("%s\n\nCulprit: %s\nSeen: %d times\n\n%s", iss.Title, iss.Culprit, iss.Count, url)

	var errs error
	enqueued := 0
	for _, ch := range channels {
		if !ch.Enabled {
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
			"title":         iss.Title,
			"culprit":       iss.Culprit,
			"count":         iss.Count,
			"url":           url,
			"subject":       subject,
			"body":          body,
			"channel_kind":  ch.Kind,
			"target":        ch.Target,
			"secret":        ch.Secret,
		}
		// Гейт трансграничной передачи: во внешние каналы без ExternalDetails
		// уходит обезличенный payload (см. notify.RedactExternalPayload).
		if !n.ExternalDetails && (ch.Kind == alert.ChannelTelegram || ch.Kind == alert.ChannelWebhook) {
			payload = notify.RedactExternalPayload(payload)
		}
		if err := n.Outbox.Enqueue(ctx, ch.ID, payload); err != nil {
			slog.Error("trace: notify: enqueue failed", "channel_id", ch.ID, "error", err)
			errs = errors.Join(errs, fmt.Errorf("trace: notify: enqueue channel %d: %w", ch.ID, err))
			continue
		}
		enqueued++
	}
	// Слот занимается ДО Enqueue — иначе четыре воркера обошли бы лимит гонкой. Но
	// если не встало НИ ОДНО сообщение (PG моргнула на всех каналах), рассылки не
	// было, и часовой бюджет проекта не должен быть потрачен на неё: возвращаем
	// слот. Частичный успех слот удерживает — что-то ушло.
	if enqueued == 0 && errs != nil {
		if err := n.releaseAlert(ctx, projectID); err != nil {
			slog.Error("trace: notify: release throttle slot", "project_id", projectID, "error", err)
		}
	}
	return errs
}

// releaseAlert возвращает один занятый claimAlert-ом слот: sent - 1, но не ниже
// нуля и только в пределах текущего окна (истёкшее окно перезапишет claimAlert
// сам, коррекция там ни к чему). Best-effort: провал release оставляет слот
// занятым — это потеря одного алерта из часового лимита, не некорректность.
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

// claimAlert атомарно занимает один слот из MaxPerfAlertsPerHour для проекта:
// проверка «влезаем ли в окно» и отметка «занято» — один statement, как в
// alert.Evaluator.claimThrottle. Раздельные SELECT и UPDATE здесь дали бы гонку
// четырёх воркеров пайплайна ровно там, где лавину и надо остановить.
//
// Строки нет → INSERT проходит → слот занят (sent=1). Окно истекло
// (window_start <= cutoff) → ON CONFLICT DO UPDATE открывает новое окно
// (window_start=now(), sent=1). Окно живо и sent < лимита → sent+1, слот занят.
// Окно живо и лимит выбран → WHERE отсекает UPDATE → RETURNING пуст → слота нет.
// ON CONFLICT берёт блокировку строки, поэтому лимит соблюдается ТОЧНО, а не
// «примерно» — параллельные клеймы одного проекта сериализуются на ней.
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
