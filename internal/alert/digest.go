package alert

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/humanize"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/notify"
)

// Интерфейс, не конкретный тип — тот же приём, что у escalation.Enqueuer:
// тест может подставить фейк без похода в Postgres.
type Enqueuer interface {
	Enqueue(ctx context.Context, channelID int64, payload map[string]any) error
}

// Заметно чаще окна бюджета — сводка должна уйти вскоре после его закрытия,
// не ждать следующего всплеска.
const digestInterval = 5 * time.Minute

const digestBatch = 50

// Живёт рядом с доставкой (гейт по Outbox, не по режиму процесса) — контур,
// привязанный к режиму, молча не работает в части конфигураций.
type Digester struct {
	Svc    *Service
	Outbox Enqueuer

	// Префикс ссылки на проект в сводке.
	BaseURL string

	// См. Evaluator.EmailEnabled.
	EmailEnabled bool

	// Нулевое значение не доверяет никому — детали уходят только тем, кого
	// оператор подтвердил как свой контур.
	Details DetailPolicy

	// GOTCHA_LOCALE инстанса — внешний канал не знает языка получателя, язык
	// сводки выбирает оператор.
	Locale i18n.Locale

	// Период тика; 0 → digestInterval.
	Interval time.Duration
}

func (d *Digester) Run(ctx context.Context) {
	interval := d.Interval
	if interval <= 0 {
		interval = digestInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			d.Tick(ctx)
		}
	}
}

// Экспортирован ради теста — цикл Run проверять неудобно.
func (d *Digester) Tick(ctx context.Context) {
	batches, err := d.Svc.ClaimSuppressed(ctx, digestBatch)
	if err != nil {
		slog.Error("alert: claim suppressed for digest failed", "error", err)
		return
	}
	for _, b := range batches {
		if err := d.send(ctx, b); err != nil {
			slog.Error("alert: digest enqueue failed",
				"project_id", b.ProjectID, "suppressed", b.Suppressed, "error", err)
		}
	}
}

// Ошибка одного канала не должна глушить остальные — тот же приём, что у
// escalation.Dispatch (errors.Join + continue, не return).
func (d *Digester) send(ctx context.Context, b SuppressedBatch) error {
	channels, err := d.Svc.Channels(ctx, b.ProjectID)
	if err != nil {
		return fmt.Errorf("alert: digest channels: %w", err)
	}

	// Язык инстанса (GOTCHA_LOCALE), не запроса — у внешнего получателя нет
	// своей локали.
	ctx = i18n.WithLocale(ctx, d.Locale)
	url := fmt.Sprintf("%s/projects/%d/issues", d.BaseURL, b.ProjectID)
	count := strconv.Itoa(b.Suppressed)
	subject := i18n.Tf(ctx, "notify.digest.subject", "count", count)
	// Человекочитаемый момент для письма, не машинный RFC3339 формат.
	body := i18n.Tf(ctx, "notify.digest.body",
		"count", count, "since", humanize.Time(ctx, b.Since, time.UTC), "url", url)

	var errs error
	for _, ch := range channels {
		if !ch.Deliverable() {
			continue
		}
		if ch.Kind == ChannelEmail && !d.EmailEnabled {
			continue
		}
		payload := map[string]any{
			"kind":         "suppressed_digest",
			"project_id":   b.ProjectID,
			"count":        b.Suppressed,
			"url":          url,
			"subject":      subject,
			"body":         body,
			"channel_kind": ch.Kind,
			"target":       ch.Target,
		}
		// Тот же гейт трансграничной передачи, что у остальных нотифаеров.
		if !d.Details.AllowsDetails(ch) {
			payload = notify.RedactExternalPayload(ctx, payload)
		}
		if err := d.Outbox.Enqueue(ctx, ch.ID, payload); err != nil {
			slog.Error("alert: digest: enqueue failed", "channel_id", ch.ID, "error", err)
			errs = errors.Join(errs, fmt.Errorf("alert: digest enqueue channel %d: %w", ch.ID, err))
			continue
		}
	}
	return errs
}
