package alert

import (
	"context"
	"time"
)

// Локальный интерфейс, не прямая зависимость от uptime.Service — uptime
// импортирует alert, обратный импорт замкнул бы цикл.
type MaintenanceChecker interface {
	InMaintenance(ctx context.Context, projectID int64, at time.Time) (bool, error)
}
