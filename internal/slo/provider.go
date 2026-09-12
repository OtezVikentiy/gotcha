package slo

import (
	"context"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/trace"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
)

type Bucket struct {
	T           time.Time // начало корзины, UTC
	Good, Total uint64    // «хорошие» события/всего; смысл good зависит от SLI kind
}

// насколько назад у источника вообще есть данные: оценщик клипует запрошенное
// окно бюджета к этому пределу.
type Provider interface {
	Buckets(ctx context.Context, s SLO, from, to time.Time, step time.Duration) ([]Bucket, error)
	// окна обслуживания переданы снаружи, не читаются провайдером — список SLO
	// проекта грузит их раз на страницу, не на каждую строку. nil/пусто — не вырезать.
	BucketsExcluding(ctx context.Context, s SLO, from, to time.Time, step time.Duration, windows []uptime.Window) ([]Bucket, error)
	RetentionCap() time.Duration
}

// retentionDays — общий TTL таблиц transactions/check_results (0 = хранить вечно, клипа нет).
func Providers(traceQ *trace.Query, uptimeQ *uptime.Query, maint *uptime.Service, retentionDays int) map[SLIKind]Provider {
	return map[SLIKind]Provider{
		SLIAvailability: NewAvailabilityProvider(traceQ, maint, retentionDays),
		SLILatency:      NewLatencyProvider(traceQ, maint, retentionDays),
		SLIUptime:       NewUptimeProvider(uptimeQ, maint, retentionDays),
	}
}

func retentionCap(retentionDays int) time.Duration {
	if retentionDays <= 0 {
		return 0
	}
	return time.Duration(retentionDays) * 24 * time.Hour
}

// разрыв цикла импорта: trace/uptime не знают про slo.Bucket.
func convertTraceBuckets(cbs []trace.CountBucket) []Bucket {
	out := make([]Bucket, len(cbs))
	for i, c := range cbs {
		out[i] = Bucket{T: c.T, Good: c.Good, Total: c.Total}
	}
	return out
}

func convertUptimeBuckets(cbs []uptime.CountBucket) []Bucket {
	out := make([]Bucket, len(cbs))
	for i, c := range cbs {
		out[i] = Bucket{T: c.T, Good: c.Good, Total: c.Total}
	}
	return out
}

// плановое обслуживание не должно жечь бюджет. Ошибка чтения окон оставляет
// ряд как есть — расчёт бюджета важнее косметики исключения.
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

// интервалы полуоткрытые: [From, To).
func inAnyInterval(t time.Time, ivs []uptime.Interval) bool {
	for _, iv := range ivs {
		if !t.Before(iv.From) && t.Before(iv.To) {
			return true
		}
	}
	return false
}
