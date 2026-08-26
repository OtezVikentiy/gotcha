package incidentgroup

import (
	"context"
	"errors"
	"log/slog"

	"github.com/jackc/pgx/v5"
)

// B5Checker — подмножество depsuppress.Suppressor, нужное DepGate
// (duck-typing, без импорта depsuppress).
type B5Checker interface {
	CheckIncident(ctx context.Context, source string, incidentID int64) (hasParent, parentDown bool, err error)
	MarkSuppressed(ctx context.Context, source string, incidentID int64) error
}

// DepGate — escalation.DepChecker поверх B5-суппрессора с D3-хуком: host-
// инцидент, подавленный B5 в планировщике эскалаций (scheduler.tickOne →
// MarkSuppressed), тем же вызовом получает членство в группе своего down-
// корня — «B5-подавленные дети видны в составе» (§4.2). Гейт уведомлений
// у таких членов остаётся B5-шным навсегда (Р4: B5 строже D3).
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
	// Attach — best-effort: ошибка членства не должна отменить подавление
	// (подавление уже состоялось и важнее состава; лог — и дальше).
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
