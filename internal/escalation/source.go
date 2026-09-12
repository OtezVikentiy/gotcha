package escalation

import (
	"context"
	"time"
)

type PendingIncident struct {
	ID              int64
	ProjectID       int64
	StartedAt       time.Time
	Severity        string // 'critical' | 'warning'
	EscalationLevel int    // сколько ступеней уже отправлено
}

// BumpEscalation, не Bump: у 4 из 5 сторов уже есть Bump(current, peak) эволюатора.
type Source interface {
	Name() string
	OpenUnacked(ctx context.Context) ([]PendingIncident, error)
	BumpEscalation(ctx context.Context, id int64, from int) (bool, error)
}

// Необязательный интерфейс — реализует только host. uptime подавляется и
// освобождается отдельно через Detector.settleHeldIncident, минуя Scheduler.
type SuppressedSource interface {
	OpenSuppressed(ctx context.Context) ([]PendingIncident, error)
	// Часы лесенки перезапускаются от момента снятия, как при выходе из группы.
	ClearSuppressed(ctx context.Context, id int64) error
}
