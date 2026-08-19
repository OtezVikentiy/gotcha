package slo

import (
	"context"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/trace"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
)

// AvailabilityProvider — SLI доли успешных транзакций (good/total) из MV
// transactions_5m. Фильтруется по Transaction/Environment SLO (пустые → любой).
type AvailabilityProvider struct {
	q             *trace.Query
	maint         *uptime.Service
	retentionDays int
}

// NewAvailabilityProvider собирает провайдер availability. maint == nil
// отключает исключение окон обслуживания (для тестов/безмейнтенанс-инсталляций).
func NewAvailabilityProvider(q *trace.Query, maint *uptime.Service, retentionDays int) *AvailabilityProvider {
	return &AvailabilityProvider{q: q, maint: maint, retentionDays: retentionDays}
}

func (p *AvailabilityProvider) Buckets(ctx context.Context, s SLO, from, to time.Time, step time.Duration) ([]Bucket, error) {
	cbs, err := p.q.GoodTotalBuckets(ctx, s.ProjectID, s.Transaction, s.Environment, from, to, step)
	if err != nil {
		return nil, err
	}
	bs := convertTraceBuckets(cbs)
	return excludeMaintenance(ctx, p.maint, s.ProjectID, bs, from, to, step), nil
}

func (p *AvailabilityProvider) RetentionCap() time.Duration { return retentionCap(p.retentionDays) }
