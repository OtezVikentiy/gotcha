package auth

import (
	"context"
	"log/slog"
	"time"
)

const defaultJanitorInterval = time.Hour

// Без него sessions растёт бесконечно — expires_at сам по себе строки не удаляет.
type Janitor struct {
	Svc *Service

	Interval time.Duration

	// Доп. периодические очистки на том же тике (напр. org_invites) — чтобы не плодить тикеры и не
	// связывать auth с org. Ошибка одной очистки не останавливает остальные.
	Extra []Cleanup
}

type Cleanup struct {
	Name string
	Fn   func(context.Context) (int64, error)
}

func (j *Janitor) Run(ctx context.Context) {
	interval := j.Interval
	if interval <= 0 {
		interval = defaultJanitorInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Первый прогон — сразу, не дожидаясь тика: иначе очистки не запустятся до первого тика.
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

func (j *Janitor) tick(ctx context.Context) {
	n, err := j.Svc.DeleteExpiredSessions(ctx)
	if err != nil {
		slog.Error("auth janitor: delete expired sessions failed", "error", err)
	} else {
		slog.Debug("auth janitor: deleted expired sessions", "count", n)
	}
	if m, err := j.Svc.PurgeExpiredPasswordResets(ctx); err != nil {
		slog.Error("auth janitor: purge expired password resets failed", "error", err)
	} else {
		slog.Debug("auth janitor: purged expired password resets", "count", m)
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
