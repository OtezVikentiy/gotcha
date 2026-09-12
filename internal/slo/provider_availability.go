package slo

import (
	"context"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/trace"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
)

type AvailabilityProvider struct {
	q             *trace.Query
	maint         *uptime.Service
	retentionDays int
}

// maint == nil отключает исключение окон обслуживания.
func NewAvailabilityProvider(q *trace.Query, maint *uptime.Service, retentionDays int) *AvailabilityProvider {
	return &AvailabilityProvider{q: q, maint: maint, retentionDays: retentionDays}
}

func (p *AvailabilityProvider) Buckets(ctx context.Context, s SLO, from, to time.Time, step time.Duration) ([]Bucket, error) {
	bs, err := p.rawBuckets(ctx, s, from, to, step)
	if err != nil {
		return nil, err
	}
	return excludeMaintenance(ctx, p.maint, s.ProjectID, bs, from, to, step), nil
}

func (p *AvailabilityProvider) BucketsExcluding(ctx context.Context, s SLO, from, to time.Time, step time.Duration, windows []uptime.Window) ([]Bucket, error) {
	bs, err := p.rawBuckets(ctx, s, from, to, step)
	if err != nil {
		return nil, err
	}
	return excludeWindows(windows, bs, from, to, step), nil
}

func (p *AvailabilityProvider) rawBuckets(ctx context.Context, s SLO, from, to time.Time, step time.Duration) ([]Bucket, error) {
	cbs, err := p.q.GoodTotalBuckets(ctx, s.ProjectID, s.Transaction, s.Environment, from, to, step)
	if err != nil {
		return nil, err
	}
	return convertTraceBuckets(cbs), nil
}

func (p *AvailabilityProvider) RetentionCap() time.Duration { return retentionCap(p.retentionDays) }
