package alert

import (
	"context"
	"time"
)

// MaintenanceChecker — проверка «проект сейчас в окне обслуживания» (B3:
// подавление уведомлений всех источников инцидентов). Локальный интерфейс, а
// не прямая зависимость от uptime.Service: uptime импортирует alert
// (uptime/notifier.go), так что обратный импорт замкнул бы цикл. В проде
// интерфейс удовлетворяет *uptime.Service структурно (main.go), без явного
// приведения типов.
type MaintenanceChecker interface {
	InMaintenance(ctx context.Context, projectID int64, at time.Time) (bool, error)
}
