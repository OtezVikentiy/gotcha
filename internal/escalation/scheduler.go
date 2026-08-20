package escalation

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// MaintenanceChecker сообщает, идёт ли сейчас окно обслуживания проекта —
// живая проверка на каждый инцидент каждого тика (BLOCKER-3): решение
// «эскалировать/подавить» принимается в момент отправки ступени, а не
// заморожено на момент открытия инцидента, потому что окно могло начаться
// (или закончиться) уже после того, как инцидент открылся.
type MaintenanceChecker interface {
	InMaintenance(ctx context.Context, projectID int64, at time.Time) (bool, error)
}

// StepNotifier — общий интерфейс пяти product-нотифаеров (T6): шлёт ступень
// эскалации [step] инцидента incidentID в channelIDs и возвращает КАНАЛЫ,
// реально поставленные в очередь (см. SendStepIfDue) — не факт намерения.
type StepNotifier interface {
	NotifyStep(ctx context.Context, incidentID int64, channelIDs []int64, step int) ([]int64, error)
}

// Binding — один из пяти источников инцидентов (host/metric/trace/profile/
// slo), спаренный со своим нотифаером. Src и Notifier — те же объекты, что
// уже сконструированы для эволюаторов в main.go (T4/T6), переиспользуются
// как есть.
type Binding struct {
	Src      Source
	Notifier StepNotifier
}

// Scheduler — централизованный планировщик эскалаций (T8): один тикер на
// процесс вместо того, чтобы каждый из пяти эволюаторов сам гонял свою
// лесенку — эскалация ортогональна открытию инцидента (открывает эволюатор
// один раз, а лесенка идёт своим шагом, пока инцидент не закрыт/не
// подтверждён), поэтому и живёт отдельным циклом.
type Scheduler struct {
	Bindings []Binding
	Policy   *PolicyStore
	Maint    MaintenanceChecker
	Pool     *pgxpool.Pool
	Interval time.Duration
	// Now — источник текущего времени; в проде time.Now, в тестах
	// фиксируется, чтобы детерминированно управлять elapsed.
	Now func() time.Time
}

// Tick — один проход по всем источникам: для каждого открытого неподтверж-
// дённого инцидента проверяет живое окно обслуживания, резолвит лесенку и
// шлёт очередную ступень, если её задержка от открытия инцидента настала.
// Ошибка на одном инциденте логируется и не прерывает обработку остальных —
// один плохой инцидент не должен глушить эскалацию по всем прочим.
func (s *Scheduler) Tick(ctx context.Context) {
	now := s.Now()
	for _, b := range s.Bindings {
		pending, err := b.Src.OpenUnacked(ctx)
		if err != nil {
			slog.Error("escalation scheduler: open unacked failed", "source", b.Src.Name(), "error", err)
			continue
		}
		for _, p := range pending {
			s.tickOne(ctx, b, p, now)
		}
	}
}

func (s *Scheduler) tickOne(ctx context.Context, b Binding, p PendingIncident, now time.Time) {
	// Fail-safe: ошибка проверки окна обслуживания — НЕ эскалируем. Окно
	// важнее: ложная эскалация во время обслуживания хуже пропущенной
	// ступени, которую следующий тик всё равно отправит.
	inMaint, err := s.Maint.InMaintenance(ctx, p.ProjectID, now)
	if err != nil {
		slog.Error("escalation scheduler: maintenance check failed", "source", b.Src.Name(), "incident_id", p.ID, "error", err)
		return
	}
	if inMaint {
		return
	}

	ladder, err := s.Policy.Ladder(ctx, p.ProjectID, p.Severity)
	if err != nil {
		slog.Error("escalation scheduler: ladder resolve failed", "source", b.Src.Name(), "incident_id", p.ID, "error", err)
		return
	}

	elapsed := now.Sub(p.StartedAt)
	_, err = SendStepIfDue(ctx, ladder, b.Src.Name(), s.Pool, p.ID, p.EscalationLevel, elapsed,
		func(channelIDs []int64, step int) ([]int64, error) {
			return b.Notifier.NotifyStep(ctx, p.ID, channelIDs, step)
		},
		func(id int64, from int) (bool, error) {
			return b.Src.BumpEscalation(ctx, id, from)
		})
	if err != nil {
		slog.Error("escalation scheduler: send step failed", "source", b.Src.Name(), "incident_id", p.ID, "error", err)
	}
}

// Run тикает с Interval до отмены ctx. Запускать как "go sched.Run(ctx)".
func (s *Scheduler) Run(ctx context.Context) {
	ticker := time.NewTicker(s.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.Tick(ctx)
		}
	}
}
