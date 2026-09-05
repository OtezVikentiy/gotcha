package escalation

import (
	"context"
	"time"
)

// PendingIncident — открытый неподтверждённый инцидент, кандидат на эскалацию.
type PendingIncident struct {
	ID              int64
	ProjectID       int64
	StartedAt       time.Time
	Severity        string // 'critical' | 'warning'
	EscalationLevel int    // сколько ступеней уже отправлено
}

// Source — инцидент-источник для планировщика эскалаций (T7). Реализуется 5
// сторами (host/metric/trace/profile/slo). Name() совпадает с incident_source
// в incident_escalations (миграция 0077).
//
// Метод бампа уровня эскалации назван BumpEscalation, а не Bump: на 4 из 5
// сторов (host/metric/trace/profile) уже есть Bump(ctx, id, current[, peak]
// float64) error — эволюаторский метод обновления current_value/peak_value
// на тике, никак не связанный с эскалацией. Тот же receiver не может нести
// два метода с именем Bump разной сигнатуры — переименование интерфейсного
// метода снимает коллизию, не трогая существующий Bump.
type Source interface {
	Name() string
	OpenUnacked(ctx context.Context) ([]PendingIncident, error)
	BumpEscalation(ctx context.Context, id int64, from int) (bool, error)
}

// SuppressedSource — источник, чьи инциденты умеют быть подавлены
// зависимостью и умеют её отпустить (K1-4, аудит перед 1.0): подавленный
// инцидент, чей родитель восстановился, обязан возобновить эскалацию, а не
// молчать до конца времён — до этой правки писателя в false у
// suppressed_by_dep не было вовсе ни у host, ни у uptime.
//
// Необязательный интерфейс: Scheduler.Tick проверяет его через type
// assertion на каждом Binding.Src, а не требует его от Source напрямую —
// только host реализует его здесь. uptime подавляется и освобождается ПО
// ДРУГОМУ пути (Detector.settleHeldIncident, не Scheduler): uptime-инцидент
// до первой доставки "down" планировщику не виден вовсе (Service.OpenUnacked
// фильтрует escalation_level > 0, см. её докблок), и подавленный инцидент
// эскалацию ещё не начинал — снятие подавления для него означает "отправить
// step0", а не "продолжить лесенку с текущего уровня", поэтому Detector
// решает это сам, минуя общий гейт планировщика.
type SuppressedSource interface {
	// OpenSuppressed возвращает открытые, неподтверждённые, подавленные
	// зависимостью инциденты (suppressed_by_dep = true) — кандидаты на
	// освобождение, если их родитель восстановился.
	OpenSuppressed(ctx context.Context) ([]PendingIncident, error)
	// ClearSuppressed снимает подавление (suppressed_by_dep = false,
	// dep_released_at = now(), миграция 0090) — часы лесенки перезапускаются
	// от момента снятия, как от выхода из группы инцидентов (0067).
	ClearSuppressed(ctx context.Context, id int64) error
}
