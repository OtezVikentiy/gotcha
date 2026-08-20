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
