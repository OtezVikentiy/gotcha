package notify

import (
	"context"
	"log/slog"
	"time"
)

// defaultOutboxJanitorInterval — период тика OutboxJanitor по умолчанию.
const defaultOutboxJanitorInterval = time.Hour

// OutboxJanitor периодически чистит доставленные/проваленные строки
// notification_outbox (см. Outbox.PurgeOld): без этого таблица растёт
// бесконечно и хранит секреты каналов в payload дольше необходимого.
type OutboxJanitor struct {
	Outbox    *Outbox
	Retention time.Duration // старше — удаляется
	Interval  time.Duration // период тика, дефолт 1 час
}

// Run тикает с Interval, на каждом тике зовёт PurgeOld(Retention). Ошибка
// логируется и не роняет цикл. Запускать как "go j.Run(ctx)".
func (j *OutboxJanitor) Run(ctx context.Context) {
	interval := j.Interval
	if interval <= 0 {
		interval = defaultOutboxJanitorInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Первый проход — сразу, не дожидаясь тика (как telemetry.EntityJanitor):
	// иначе после каждого рестарта чаще Interval (час по умолчанию) очередь с
	// секретами каналов в payload не чистится вовсе.
	j.tick(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			j.tick(ctx)
		}
	}
}

func (j *OutboxJanitor) tick(ctx context.Context) {
	n, err := j.Outbox.PurgeOld(ctx, j.Retention)
	if err != nil {
		slog.Error("notify outbox janitor: purge failed", "error", err)
		return
	}
	if n > 0 {
		slog.Info("notify outbox janitor: purged old rows", "deleted", n)
	}
}
