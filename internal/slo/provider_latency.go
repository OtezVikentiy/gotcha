package slo

import (
	"context"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/trace"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
)

type LatencyProvider struct {
	q             *trace.Query
	maint         *uptime.Service
	retentionDays int
}

// maint == nil отключает исключение окон обслуживания.
func NewLatencyProvider(q *trace.Query, maint *uptime.Service, retentionDays int) *LatencyProvider {
	return &LatencyProvider{q: q, maint: maint, retentionDays: retentionDays}
}

func (p *LatencyProvider) Buckets(ctx context.Context, s SLO, from, to time.Time, step time.Duration) ([]Bucket, error) {
	bs, err := p.rawBuckets(ctx, s, from, to, step)
	if err != nil {
		return nil, err
	}
	return excludeMaintenance(ctx, p.maint, s.ProjectID, bs, from, to, step), nil
}

func (p *LatencyProvider) BucketsExcluding(ctx context.Context, s SLO, from, to time.Time, step time.Duration, windows []uptime.Window) ([]Bucket, error) {
	bs, err := p.rawBuckets(ctx, s, from, to, step)
	if err != nil {
		return nil, err
	}
	return excludeWindows(windows, bs, from, to, step), nil
}

func (p *LatencyProvider) rawBuckets(ctx context.Context, s SLO, from, to time.Time, step time.Duration) ([]Bucket, error) {
	// мс → мкс (duration_us в CH); ThresholdMS <= 0 даёт порог 0 (good=0) — это
	// следствие конфигурации SLO, валидация порога живёт на слое формы.
	var thresholdUS uint64
	if s.ThresholdMS > 0 {
		thresholdUS = uint64(s.ThresholdMS) * 1000
	}
	cbs, err := p.q.LatencyGoodBuckets(ctx, s.ProjectID, s.Transaction, s.Environment, thresholdUS, from, to, step)
	if err != nil {
		return nil, err
	}
	return convertTraceBuckets(cbs), nil
}

func (p *LatencyProvider) RetentionCap() time.Duration { return retentionCap(p.retentionDays) }
