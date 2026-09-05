package slo

import (
	"context"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/trace"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
)

// Bucket — корзина ряда good/total за интервал времени: T — начало корзины
// (UTC), Good — «хорошие» события (успешные транзакции / быстрее порога /
// успешные проверки), Total — все события корзины. Общий тип, на котором
// строится математика бюджета/burn (budget.go) и периодический оценщик.
type Bucket struct {
	T           time.Time
	Good, Total uint64
}

// Provider отдаёт ряд good/total по корзинам времени для одного SLO на окне
// [from, to) с шагом step, исключая окна обслуживания. RetentionCap — насколько
// назад у источника вообще есть данные: оценщик клипует запрошенное окно
// бюджета к этому пределу.
type Provider interface {
	Buckets(ctx context.Context, s SLO, from, to time.Time, step time.Duration) ([]Bucket, error)
	// BucketsExcluding — то же, что Buckets, но окна обслуживания проекта
	// переданы снаружи, а не читаются провайдером: список SLO проекта грузит
	// их один раз на страницу, а не на каждую строку (аудит 2026-09-04,
	// K8-2). nil/пусто — «окон нет», корзины не вырезаются.
	BucketsExcluding(ctx context.Context, s SLO, from, to time.Time, step time.Duration, windows []uptime.Window) ([]Bucket, error)
	RetentionCap() time.Duration
}

// Providers собирает провайдеры для всех трёх типов SLI. retentionDays — общий
// TTL таблиц transactions/check_results (cfg.RetentionDays; 0 = хранить вечно →
// клипа окна нет). maint — служба окон обслуживания (nil отключает исключение).
func Providers(traceQ *trace.Query, uptimeQ *uptime.Query, maint *uptime.Service, retentionDays int) map[SLIKind]Provider {
	return map[SLIKind]Provider{
		SLIAvailability: NewAvailabilityProvider(traceQ, maint, retentionDays),
		SLILatency:      NewLatencyProvider(traceQ, maint, retentionDays),
		SLIUptime:       NewUptimeProvider(uptimeQ, maint, retentionDays),
	}
}

// retentionCap переводит число дней хранения в длительность клипа окна. 0 (или
// меньше) = хранить вечно → 0 (клипа нет).
func retentionCap(retentionDays int) time.Duration {
	if retentionDays <= 0 {
		return 0
	}
	return time.Duration(retentionDays) * 24 * time.Hour
}

// convertTraceBuckets переводит []trace.CountBucket в []Bucket (разрыв цикла
// импорта: trace/uptime не знают про slo.Bucket).
func convertTraceBuckets(cbs []trace.CountBucket) []Bucket {
	out := make([]Bucket, len(cbs))
	for i, c := range cbs {
		out[i] = Bucket{T: c.T, Good: c.Good, Total: c.Total}
	}
	return out
}

// convertUptimeBuckets — то же для []uptime.CountBucket.
func convertUptimeBuckets(cbs []uptime.CountBucket) []Bucket {
	out := make([]Bucket, len(cbs))
	for i, c := range cbs {
		out[i] = Bucket{T: c.T, Good: c.Good, Total: c.Total}
	}
	return out
}

// excludeMaintenance отбрасывает корзины, чей центр (T + step/2) попадает в
// любое окно обслуживания проекта на [from, to): плановое обслуживание не должно
// жечь бюджет. maint == nil → без исключения. Ошибка чтения окон или их
// отсутствие оставляют ряд как есть (расчёт бюджета важнее косметики исключения).
func excludeMaintenance(ctx context.Context, maint *uptime.Service, projectID int64, bs []Bucket, from, to time.Time, step time.Duration) []Bucket {
	if maint == nil || len(bs) == 0 {
		return bs
	}
	ws, err := maint.Windows(ctx, projectID)
	if err != nil {
		return bs
	}
	return excludeWindows(ws, bs, from, to, step)
}

// excludeWindows — excludeMaintenance с уже загруженными окнами (см.
// Provider.BucketsExcluding): вырезает корзины, чья середина попадает в
// интервал обслуживания за [from, to).
func excludeWindows(ws []uptime.Window, bs []Bucket, from, to time.Time, step time.Duration) []Bucket {
	if len(ws) == 0 || len(bs) == 0 {
		return bs
	}
	ivs := uptime.WindowIntervals(ws, from, to)
	if len(ivs) == 0 {
		return bs
	}
	half := step / 2
	out := make([]Bucket, 0, len(bs))
	for _, b := range bs {
		if inAnyInterval(b.T.Add(half), ivs) {
			continue
		}
		out = append(out, b)
	}
	return out
}

// inAnyInterval сообщает, попадает ли t в любой из полуоткрытых [From, To).
func inAnyInterval(t time.Time, ivs []uptime.Interval) bool {
	for _, iv := range ivs {
		if !t.Before(iv.From) && t.Before(iv.To) {
			return true
		}
	}
	return false
}
