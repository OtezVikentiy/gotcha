package metric

import (
	"context"
	"time"
)

// Интерфейс, не прямая зависимость от uptime.Service: пакет metric не должен знать о внутреннем устройстве
// окон обслуживания, только о факте. В проде реализует *uptime.Service, структурно. Зеркало host.MaintenanceChecker.
type MaintenanceChecker interface {
	InMaintenance(ctx context.Context, projectID int64, at time.Time) (bool, error)
}
