package slo

import (
	"context"
	"fmt"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
)

type UptimeProvider struct {
	q             *uptime.Query
	maint         *uptime.Service
	retentionDays int
}

// maint == nil отключает исключение окон обслуживания.
func NewUptimeProvider(q *uptime.Query, maint *uptime.Service, retentionDays int) *UptimeProvider {
	return &UptimeProvider{q: q, maint: maint, retentionDays: retentionDays}
}

func (p *UptimeProvider) Buckets(ctx context.Context, s SLO, from, to time.Time, step time.Duration) ([]Bucket, error) {
	bs, err := p.rawBuckets(ctx, s, from, to, step)
	if err != nil {
		return nil, err
	}
	return excludeMaintenance(ctx, p.maint, s.ProjectID, bs, from, to, step), nil
}

func (p *UptimeProvider) BucketsExcluding(ctx context.Context, s SLO, from, to time.Time, step time.Duration, windows []uptime.Window) ([]Bucket, error) {
	bs, err := p.rawBuckets(ctx, s, from, to, step)
	if err != nil {
		return nil, err
	}
	return excludeWindows(windows, bs, from, to, step), nil
}

func (p *UptimeProvider) rawBuckets(ctx context.Context, s SLO, from, to time.Time, step time.Duration) ([]Bucket, error) {
	// MonitorID nullable: не паникуем на разыменовании, возвращаем ошибку.
	if s.MonitorID == nil {
		return nil, fmt.Errorf("slo: uptime provider: SLO %d has no monitor_id", s.ID)
	}
	cbs, err := p.q.UpBuckets(ctx, *s.MonitorID, from, to, step)
	if err != nil {
		return nil, err
	}
	return convertUptimeBuckets(cbs), nil
}

func (p *UptimeProvider) RetentionCap() time.Duration { return retentionCap(p.retentionDays) }
