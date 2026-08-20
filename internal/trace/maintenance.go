package trace

import (
	"context"
	"time"
)

// MaintenanceChecker — проверка «проект сейчас в окне обслуживания»
// (B3: подавление уведомлений всех источников инцидентов). Интерфейс, а не
// прямая зависимость от uptime.Service: пакет trace не должен знать о
// внутреннем устройстве окон обслуживания, только о факте (пакет и так
// импортирует uptime для другого, но интерфейс единообразнее с host/
// maintenance.go и без риска цикла). В проде реализует *uptime.Service
// (main.go, startEvaluators), структурно — без явного приведения типов.
type MaintenanceChecker interface {
	InMaintenance(ctx context.Context, projectID int64, at time.Time) (bool, error)
}
