package host

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
)

type RetireNotifier interface {
	HostRetired(ctx context.Context, h Host, open []Incident) error
}

// DELETE хоста каскадит его открытые инциденты — без этого шага «Тишина» исчезла бы бесследно.
// Альтернатива (не удалять такой хост) раздула бы реестр мёртвых машин до MaxHostsPerProject.
type Retirer struct {
	Hosts     *Store
	Incidents *IncidentService
	Notifier  RetireNotifier
}

// Ошибка одного хоста не прерывает батч (errors.Join) — иначе один сломанный канал держал бы всех.
// Шаг обязан быть идемпотентен: у уже снятых хостов повтор просто не найдёт открытых инцидентов.
func (r *Retirer) Retire(ctx context.Context, hostIDs []int64) error {
	hosts, err := r.Hosts.ListByIDs(ctx, hostIDs)
	if err != nil {
		return fmt.Errorf("host: retire: load hosts: %w", err)
	}

	var errs error
	for _, h := range hosts {
		if err := r.retireOne(ctx, h); err != nil {
			slog.Error("host: retire failed", "host_id", h.ID, "project_id", h.ProjectID, "error", err)
			errs = errors.Join(errs, err)
		}
	}
	return errs
}

// «Уведомить, потом закрыть»: обратный порядок на сбое уведомления тихо потерял бы инцидент.
// Цена — возможный дубль сообщения при сбое закрытия: дубль виден и безвреден, пропажа инцидента нет.
func (r *Retirer) retireOne(ctx context.Context, h Host) error {
	open, err := r.Incidents.ListOpenByHost(ctx, h.ID)
	if err != nil {
		return fmt.Errorf("host: retire: open incidents of host %d: %w", h.ID, err)
	}
	if len(open) == 0 {
		return nil
	}
	if err := r.Notifier.HostRetired(ctx, h, open); err != nil {
		return fmt.Errorf("host: retire: notify host %d: %w", h.ID, err)
	}
	for _, in := range open {
		// current_value не пересчитывается — свежих метрик у молчащего хоста нет.
		if _, err := r.Incidents.Resolve(ctx, in.ID, in.CurrentValue); err != nil {
			return fmt.Errorf("host: retire: resolve incident %d: %w", in.ID, err)
		}
		// Инцидент может пережить хост, ожившего между выборкой батча и DELETE — запись остаётся честной.
		if err := r.Incidents.MarkNotified(ctx, in.ID, false); err != nil {
			return fmt.Errorf("host: retire: mark incident %d notified: %w", in.ID, err)
		}
	}
	slog.Info("host: retired by retention", "host_id", h.ID, "project_id", h.ProjectID,
		"incidents_closed", len(open))
	return nil
}
