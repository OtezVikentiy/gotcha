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

// Необязательный интерфейс — реализуют host и uptime. У uptime есть ещё и
// реактивный путь через Detector.settleHeldIncident (снимает подавление сразу
// при новом результате пробы), а этот — подстраховка на случай, если новый
// результат для монитора больше не придёт (пауза, удаление региона).
type SuppressedSource interface {
	OpenSuppressed(ctx context.Context) ([]PendingIncident, error)
	// Часы лесенки перезапускаются от момента снятия, как при выходе из группы.
	ClearSuppressed(ctx context.Context, id int64) error
}
