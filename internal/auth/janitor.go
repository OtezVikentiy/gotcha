package auth

import (
	"context"
	"log/slog"
	"time"
)

// defaultJanitorInterval — период тика Janitor, если Janitor.Interval не
// задан.
const defaultJanitorInterval = time.Hour

// Janitor периодически удаляет просроченные сессии: сами по себе они не
// исчезают из БД, DeleteExpiredSessions/SessionUser лишь отвергают их по
// expires_at на чтении — без Janitor таблица sessions растёт бесконечно.
type Janitor struct {
	Svc *Service

	// Interval — период тика. По умолчанию defaultJanitorInterval (1 час).
	Interval time.Duration
}

// Run тикает с Janitor.Interval, на каждом тике удаляет просроченные сессии
// и пишет их число debug-логом. Возвращается, когда ctx отменяется.
func (j *Janitor) Run(ctx context.Context) {
	interval := j.Interval
	if interval <= 0 {
		interval = defaultJanitorInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n, err := j.Svc.DeleteExpiredSessions(ctx)
			if err != nil {
				slog.Error("auth janitor: delete expired sessions failed", "error", err)
				continue
			}
			slog.Debug("auth janitor: deleted expired sessions", "count", n)
		}
	}
}
