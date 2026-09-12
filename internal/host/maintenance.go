package host

import (
	"context"
	"time"
)

type MaintenanceChecker interface {
	InMaintenance(ctx context.Context, projectID int64, at time.Time) (bool, error)
}
