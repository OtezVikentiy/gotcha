package incidentgroup

import (
	"context"
	"errors"
	"log/slog"

	"github.com/jackc/pgx/v5"
)

// Duck-typing — не тянет depsuppress напрямую.
type B5Checker interface {
	CheckIncident(ctx context.Context, source string, incidentID int64) (hasParent, parentDown bool, err error)
	MarkSuppressed(ctx context.Context, source string, incidentID int64) error
}

// Подавленный B5 host-инцидент тем же вызовом получает членство в группе своего down-корня.
// Гейт уведомлений у таких членов остаётся B5-шным навсегда — B5 строже D3.
type DepGate struct {
	Dep     B5Checker
	Grouper *Grouper
}

func (d *DepGate) CheckIncident(ctx context.Context, source string, incidentID int64) (bool, bool, error) {
	return d.Dep.CheckIncident(ctx, source, incidentID)
}

func (d *DepGate) MarkSuppressed(ctx context.Context, source string, incidentID int64) error {
	if err := d.Dep.MarkSuppressed(ctx, source, incidentID); err != nil {
		return err
	}
	// Attach best-effort: подавление уже состоялось и важнее состава — ошибка членства только логируется.
	if source != "host" || d.Grouper == nil {
		return nil // uptime помечает depsuppress-ом сам uptime (см. detector)
	}
	var hostID int64
	err := d.Grouper.Pool.QueryRow(ctx,
		`SELECT host_id FROM host_incidents WHERE id = $1`, incidentID).Scan(&hostID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // гонка с закрытием — подавлять/присоединять нечего
	}
	if err != nil {
		slog.Error("incidentgroup: depgate load host_id failed", "incident_id", incidentID, "error", err)
		return nil
	}
	if _, _, err := d.Grouper.Attach(ctx, "host", incidentID, "host", hostID); err != nil {
		slog.Error("incidentgroup: depgate attach failed", "incident_id", incidentID, "error", err)
	}
	return nil
}
