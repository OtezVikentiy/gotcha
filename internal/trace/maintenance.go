package trace

import (
	"context"
	"time"
)

// Интерфейс, а не прямая зависимость от uptime.Service: пакет trace не должен знать о
// внутреннем устройстве окон обслуживания, только о факте.
type MaintenanceChecker interface {
	InMaintenance(ctx context.Context, projectID int64, at time.Time) (bool, error)
}
