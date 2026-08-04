package alert

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/humanize"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/notify"
)

// digestInterval — как часто проверять, не пора ли разослать сводки.
// Заметно чаще окна бюджета: сводка должна уйти вскоре после того, как окно
// закрылось, а не ждать следующего всплеска.
const digestInterval = 5 * time.Minute

// digestBatch — сколько проектов обрабатывать за тик.
const digestBatch = 50

// Digester рассылает сводки о подавленных уведомлениях: «подавлено ещё N».
//
// Существует потому, что потолок без сводки — это молчаливая потеря. Оператор,
// у которого сработал бюджет, обязан узнать, что часть алертов не доехала, и
// сколько именно: иначе «тишина в Telegram» неотличима от «всё спокойно», а это
// ровно тот исход, ради недопущения которого продукт и существует.
//
// Живёт рядом с доставкой (гейт по наличию Outbox, а не по режиму процесса) —
// тот же урок, что и с воркером доставки: контур, привязанный к режиму, молча
// не работает в половине конфигураций.
type Digester struct {
	Svc    *Service
	Outbox *notify.Outbox

	// BaseURL — префикс ссылки на проект в сводке.
	BaseURL string

	// EmailEnabled — см. Evaluator.EmailEnabled.
	EmailEnabled bool

	// Details — политика раскрытия деталей события получателю уведомления
	// (см. DetailPolicy). Нулевое значение не доверяет никому: детали уходят
	// только тем, кого оператор подтвердил как свой контур.
	Details DetailPolicy

	// Locale — локаль ИНСТАНСА (GOTCHA_LOCALE): внешний канал не знает языка
	// получателя, поэтому язык сводки выбирает оператор (класс №133–136).
	Locale i18n.Locale

	// Interval — период тика; 0 → digestInterval.
	Interval time.Duration
}

// Run крутит рассылку сводок до отмены контекста.
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

// Tick — один проход: забрать накопленное и разослать сводки. Экспортирован
// ради теста: цикл Run проверять неудобно, а сам проход — суть работы.
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

func (d *Digester) send(ctx context.Context, b SuppressedBatch) error {
	channels, err := d.Svc.Channels(ctx, b.ProjectID)
	if err != nil {
		return fmt.Errorf("alert: digest channels: %w", err)
	}

	// Тексты — на языке инстанса (GOTCHA_LOCALE), а не запроса: сводку
	// читает внешний получатель, у которого нет своей локали (№133–136).
	ctx = i18n.WithLocale(ctx, d.Locale)
	url := fmt.Sprintf("%s/projects/%d/issues", d.BaseURL, b.ProjectID)
	count := strconv.Itoa(b.Suppressed)
	subject := i18n.Tf(ctx, "notify.digest.subject", "count", count)
	// humanize.Time вместо голого .Format(time.RFC3339): получателю письма
	// нужен человекочитаемый момент ("2026-07-31 18:00 UTC"), а не машинный
	// "2026-07-31T18:00:00Z" — та же природа находки, что и остальной долг
	// подпроекта единиц, просто в теле письма, а не на веб-странице. ctx уже
	// доступен параметром send (используется ниже для Channels/Outbox), новый
	// прокидывать не пришлось.
	body := i18n.Tf(ctx, "notify.digest.body",
		"count", count, "since", humanize.Time(ctx, b.Since, time.UTC), "url", url)

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
			payload = notify.RedactExternalPayload(payload)
		}
		if err := d.Outbox.Enqueue(ctx, ch.ID, payload); err != nil {
			return fmt.Errorf("alert: digest enqueue channel %d: %w", ch.ID, err)
		}
	}
	return nil
}
