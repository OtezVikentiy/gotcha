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

	// Extra — дополнительные периодические очистки на том же тике (напр.
	// просроченные org_invites — их держит org, а auth про них не знает).
	// Держим здесь, чтобы не плодить отдельные тикеры и не связывать auth с org.
	// Ошибка одной очистки логируется и не мешает остальным.
	Extra []Cleanup
}

// Cleanup — именованная периодическая очистка для Janitor.Extra.
type Cleanup struct {
	Name string
	Fn   func(context.Context) (int64, error)
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
			} else {
				slog.Debug("auth janitor: deleted expired sessions", "count", n)
			}
			for _, c := range j.Extra {
				m, err := c.Fn(ctx)
				if err != nil {
					slog.Error("auth janitor: cleanup failed", "cleanup", c.Name, "error", err)
					continue
				}
				slog.Debug("auth janitor: cleanup done", "cleanup", c.Name, "count", m)
			}
		}
	}
}
