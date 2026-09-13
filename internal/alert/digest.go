package alert

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"sync/atomic"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/humanize"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/notify"
)

// EnqueueIdempotent, не Enqueue: ретрай восстановленной сводки (см. Tick)
// повторяет тот же канал — без дедупа по ключу тот получил бы её дважды.
type Enqueuer interface {
	EnqueueIdempotent(ctx context.Context, channelID int64, payload map[string]any, key string) (bool, error)
}

// Заметно чаще окна бюджета — сводка должна уйти вскоре после его закрытия,
// не ждать следующего всплеска.
const digestInterval = 5 * time.Minute

const digestBatch = 50

// Дедлайн тика — доля Interval, но не меньше пола: та же защита от
// зависшего прохода, что у escalation.Scheduler.
const (
	digestTickBudgetShare = 0.8
	minDigestTickBudget   = 10 * time.Second
)

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

	lastTickUnix    atomic.Int64  // unix-время последнего завершённого тика
	lastTickSeconds atomic.Uint64 // длительность последнего тика, math.Float64bits

	// Подавленные, для которых и отправка, и восстановление счётчика
	// отказали — безвозвратная потеря, а не отложенный ретрай.
	lostSuppressed atomic.Int64
}

// Self-метрика живости: остановленный или зависший дайджестер снаружи
// неотличим от «подавленных не было».
func (d *Digester) LastTickUnix() int64 { return d.lastTickUnix.Load() }

func (d *Digester) LastTickSeconds() float64 {
	return math.Float64frombits(d.lastTickSeconds.Load())
}

// Число подавленных алертов, чью сводку не удалось ни доставить, ни вернуть
// в счётчик для повторной попытки — оператор не узнает о них никак иначе.
func (d *Digester) LostSuppressed() int64 { return d.lostSuppressed.Load() }

func (d *Digester) effectiveInterval() time.Duration {
	if d.Interval <= 0 {
		return digestInterval
	}
	return d.Interval
}

// Считается от effectiveInterval, не от сырого Interval — иначе прод (Interval
// не задан) получал бы minDigestTickBudget вместо ~4 минут.
func (d *Digester) tickBudget() time.Duration {
	budget := time.Duration(float64(d.effectiveInterval()) * digestTickBudgetShare)
	if budget < minDigestTickBudget {
		return minDigestTickBudget
	}
	return budget
}

func (d *Digester) Run(ctx context.Context) {
	t := time.NewTicker(d.effectiveInterval())
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
	started := time.Now()
	ctx, cancel := context.WithTimeout(ctx, d.tickBudget())
	defer cancel()
	defer func() {
		d.lastTickSeconds.Store(math.Float64bits(time.Since(started).Seconds()))
		if ctx.Err() != nil {
			return
		}
		d.lastTickUnix.Store(time.Now().Unix())
	}()

	batches, err := d.Svc.ClaimSuppressed(ctx, digestBatch)
	if err != nil {
		slog.Error("alert: claim suppressed for digest failed", "error", err)
		return
	}
	for _, b := range batches {
		if ctx.Err() != nil {
			slog.Warn("alert digest: tick budget exhausted, remaining batches skipped")
			return
		}
		if err := d.send(ctx, b); err != nil {
			// claim уже необратимо обнулил счётчик — без возврата сводка
			// потерялась бы навсегда, а не ушла на следующем тике.
			if rerr := d.Svc.RestoreSuppressed(ctx, b.ProjectID, b.Suppressed); rerr != nil {
				d.lostSuppressed.Add(int64(b.Suppressed))
				slog.Error("alert: digest enqueue failed, suppressed count lost",
					"project_id", b.ProjectID, "suppressed", b.Suppressed, "error", err, "restore_error", rerr)
				continue
			}
			slog.Error("alert: digest enqueue failed, suppressed count restored for retry",
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
		// Ключ стабилен для одного и того же окна: ретрай после Tick.RestoreSuppressed
		// не задвоит сводку каналу, уже получившему её в предыдущей попытке.
		key := fmt.Sprintf("alert:digest:%d:%d", ch.ID, b.Since.Unix())
		if _, err := d.Outbox.EnqueueIdempotent(ctx, ch.ID, payload, key); err != nil {
			slog.Error("alert: digest: enqueue failed", "channel_id", ch.ID, "error", err)
			errs = errors.Join(errs, fmt.Errorf("alert: digest enqueue channel %d: %w", ch.ID, err))
			continue
		}
	}
	return errs
}
