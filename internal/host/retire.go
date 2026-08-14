package host

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
)

// RetireNotifier — то, что Retirer требует от нотифаера. Узкий интерфейс, а не
// host.Notifier целиком: оценщику снятие с наблюдения не нужно, и фейкам его
// тестов незачем отвечать за метод, которого они не зовут.
type RetireNotifier interface {
	HostRetired(ctx context.Context, h Host, open []Incident) error
}

// Retirer снимает хосты с наблюдения перед тем, как их удалит чистильщик
// сущностей (telemetry.EntityJanitor, правило hosts по last_seen).
//
// Существует потому, что удаление хоста каскадит его host_incidents
// (hosts.id ON DELETE CASCADE), включая ОТКРЫТЫЕ. Сервер, замолчавший
// навсегда, к моменту истечения ретенции держит открытым инцидент «Тишина» —
// то самое «сервер мёртв», ради которого раздел и существует, — и без этого
// шага оно исчезало бы вместе со строкой хоста, без единого события. Обратный
// выбор (защитить такой хост от удаления) означал бы, что реестр мёртвых машин
// растёт без границы и упирается в потолок MaxHostsPerProject: у хоста,
// который уже не вернётся, инцидент не закроет никто.
//
// Поэтому исчезновение оформляется: открытые инциденты закрываются, о снятии
// уходит уведомление, и только потом строку удаляют.
type Retirer struct {
	Hosts     *Store
	Incidents *IncidentService
	Notifier  RetireNotifier
}

// Retire — telemetry.PreDeleteHook: получает идентификаторы хостов, которые
// чистильщик собирается удалить в этом батче, и снимает их с наблюдения.
//
// Хост без открытых инцидентов не порождает ни уведомления, ни записи в
// журнале: рассказывать не о чем, он просто перестал присылать данные и
// удаляется молча, как удалялся всегда.
//
// Ошибка любого хоста возвращается наверх и отменяет удаление ВСЕГО батча в
// этом проходе (см. purgeTableHooked) — остальные хосты батча к этому моменту
// уже сняты, и повтор через час для них будет пустым: открытых инцидентов у
// них больше нет. Именно поэтому шаг обязан быть безопасен к повтору, и
// именно поэтому ошибка одного хоста не прерывает цикл (errors.Join): иначе
// один сломанный канал держал бы весь батч на месте, снимая по одному хосту
// за проход.
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

// retireOne — снятие одного хоста: уведомить, потом закрыть.
//
// Порядок именно такой, и он не косметический. Закрой мы инцидент первым, а
// отправка уведомления сорвись — повтор прохода не нашёл бы открытых
// инцидентов, промолчал бы и удалил хост: ровно тот молчаливый исход, ради
// которого весь шаг и существует. При обратном порядке цена сбоя — возможный
// повтор сообщения «хост снят с наблюдения» (уведомление ушло, закрытие не
// прошло, следующий проход начнёт сначала). Дубль сообщения виден и безвреден,
// пропажа инцидента — нет.
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
		// current_value инцидента не пересчитывается: свежих метрик у хоста
		// нет по определению — он молчит дольше срока их хранения. В карточке
		// остаётся последнее известное значение, как и при обычном закрытии.
		if _, err := r.Incidents.Resolve(ctx, in.ID, in.CurrentValue); err != nil {
			return fmt.Errorf("host: retire: resolve incident %d: %w", in.ID, err)
		}
		// notified_close — не досылочный флаг (досылки в подсистеме хостов
		// нет, см. IncidentService.ResolveOpenByProjectKind), но строка
		// инцидента переживёт удаление хоста, если тот успел ожить между
		// выборкой батча и DELETE. Тогда честное «о закрытии сообщили»
		// сохранится вместе с ней.
		if err := r.Incidents.MarkNotified(ctx, in.ID, false); err != nil {
			return fmt.Errorf("host: retire: mark incident %d notified: %w", in.ID, err)
		}
	}
	slog.Info("host: retired by retention", "host_id", h.ID, "project_id", h.ProjectID,
		"incidents_closed", len(open))
	return nil
}
