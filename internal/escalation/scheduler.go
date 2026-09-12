package escalation

import (
	"context"
	"log/slog"
	"math"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Дедлайн тика — доля Interval, но не меньше пола: иначе повисшая проверка
// держала бы тик бесконечно.
const (
	tickBudgetShare = 0.8
	minTickBudget   = 10 * time.Second
)

// Проверяется на каждый инцидент каждого тика: окно могло начаться или
// закончиться уже после открытия инцидента.
type MaintenanceChecker interface {
	InMaintenance(ctx context.Context, projectID int64, at time.Time) (bool, error)
}

// Локальный duck-typing интерфейс: escalation не импортирует пакет depsuppress.
type DepChecker interface {
	CheckIncident(ctx context.Context, source string, incidentID int64) (hasParent, parentDown bool, err error)
	MarkSuppressed(ctx context.Context, source string, incidentID int64) error
}

// Возвращает каналы, реально поставленные в очередь, а не намерение отправить.
type StepNotifier interface {
	NotifyStep(ctx context.Context, incidentID int64, channelIDs []int64, step int) ([]int64, error)
}

type Binding struct {
	Src      Source
	Notifier StepNotifier
}

type Scheduler struct {
	Bindings []Binding
	Policy   *PolicyStore
	Maint    MaintenanceChecker
	// Опционален: nil в тестах/сборках без depsuppress — гейт пропускается,
	// как будто родителя нет ни у кого.
	Dep DepChecker
	// Сколько держать ступень 0 при живом родителе: время ему либо упасть
	// следом, либо остаться живым — после грейса ступень 0 уходит штатно.
	SettleGrace time.Duration
	Pool        *pgxpool.Pool
	Interval    time.Duration
	// В проде time.Now, в тестах фиксируется для детерминированного elapsed.
	Now func() time.Time

	lastTickUnix    atomic.Int64  // unix-время последнего завершённого тика
	lastTickSeconds atomic.Uint64 // длительность последнего тика, math.Float64bits
}

// Self-метрика живости: мёртвый или отставший планировщик снаружи выглядит
// как «эскалировать нечего».
func (s *Scheduler) LastTickUnix() int64 { return s.lastTickUnix.Load() }

func (s *Scheduler) LastTickSeconds() float64 {
	return math.Float64frombits(s.lastTickSeconds.Load())
}

func (s *Scheduler) tickBudget() time.Duration {
	budget := time.Duration(float64(s.Interval) * tickBudgetShare)
	if budget < minTickBudget {
		return minTickBudget
	}
	return budget
}

// Ошибка на одном инциденте логируется и не прерывает обработку остальных.
func (s *Scheduler) Tick(ctx context.Context) {
	started := time.Now()
	ctx, cancel := context.WithTimeout(ctx, s.tickBudget())
	defer cancel()

	now := s.Now()
	for _, b := range s.Bindings {
		if ctx.Err() != nil {
			slog.Warn("escalation scheduler: tick budget exhausted, remaining bindings skipped",
				"budget", s.tickBudget())
			break
		}
		s.releaseSuppressed(ctx, b)
		pending, err := b.Src.OpenUnacked(ctx)
		if err != nil {
			slog.Error("escalation scheduler: open unacked failed", "source", b.Src.Name(), "error", err)
			continue
		}
		for _, p := range pending {
			s.tickOne(ctx, b, p, now)
		}
	}

	s.lastTickSeconds.Store(math.Float64bits(time.Since(started).Seconds()))
	if ctx.Err() != nil {
		slog.Warn("escalation scheduler: tick did not finish within its budget", "budget", s.tickBudget())
		return
	}
	s.lastTickUnix.Store(time.Now().Unix())
}

// dep_released_at ставится часами PG (чуть впереди Go) — ступень может не
// успеть в тот же тик; StartedAt после снятия равен моменту освобождения.
func (s *Scheduler) releaseSuppressed(ctx context.Context, b Binding) {
	ss, ok := b.Src.(SuppressedSource)
	if !ok || s.Dep == nil {
		return
	}
	list, err := ss.OpenSuppressed(ctx)
	if err != nil {
		slog.Error("escalation scheduler: open suppressed failed", "source", b.Src.Name(), "error", err)
		return
	}
	for _, p := range list {
		hasParent, parentDown, err := s.Dep.CheckIncident(ctx, b.Src.Name(), p.ID)
		if err != nil {
			slog.Warn("escalation scheduler: dep check failed while releasing suppressed",
				"source", b.Src.Name(), "incident_id", p.ID, "error", err)
			continue
		}
		if hasParent && parentDown {
			continue
		}
		// !hasParent — зависимость удалена, пока инцидент был подавлен:
		// снимаем так же, как восстановление родителя.
		if err := ss.ClearSuppressed(ctx, p.ID); err != nil {
			slog.Warn("escalation scheduler: clear suppressed failed",
				"source", b.Src.Name(), "incident_id", p.ID, "error", err)
			continue
		}
		slog.Info("escalation scheduler: dependency recovered, incident released",
			"source", b.Src.Name(), "incident_id", p.ID)
	}
}

func (s *Scheduler) tickOne(ctx context.Context, b Binding, p PendingIncident, now time.Time) {
	// Fail-safe: ошибка проверки окна — не эскалируем, ложная эскалация хуже
	// пропущенной ступени (её отправит следующий тик).
	inMaint, err := s.Maint.InMaintenance(ctx, p.ProjectID, now)
	if err != nil {
		slog.Error("escalation scheduler: maintenance check failed", "source", b.Src.Name(), "incident_id", p.ID, "error", err)
		return
	}
	if inMaint {
		return
	}

	// Гейт зависимостей — после maintenance и до резолва лесенки, чтобы не
	// тратить время впустую.
	if s.Dep != nil {
		hasParent, parentDown, err := s.Dep.CheckIncident(ctx, b.Src.Name(), p.ID)
		if err != nil {
			slog.Error("escalation scheduler: dep check failed", "source", b.Src.Name(), "incident_id", p.ID, "error", err)
			// Fail-safe: ошибка проверки зависимости — не подавляем, идём как
			// обычно.
		} else {
			if parentDown {
				// Подавляем на любой ступени, не только step0 — иначе
				// дальнейшие ступени шумят тем же сбоем зависимости.
				if err := s.Dep.MarkSuppressed(ctx, b.Src.Name(), p.ID); err != nil {
					slog.Error("escalation scheduler: mark suppressed failed", "incident_id", p.ID, "error", err)
				} else {
					slog.Info("escalation scheduler: incident suppressed by dependency", "source", b.Src.Name(), "incident_id", p.ID)
				}
				return
			}
			// Родитель жив: держим только ступень 0 и только в течение
			// SettleGrace. Ступени выше 0 уже сигнализировали и не откладываются.
			if hasParent && p.EscalationLevel == 0 && now.Sub(p.StartedAt) < s.SettleGrace {
				return
			}
		}
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
