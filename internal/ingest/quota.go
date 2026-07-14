package ingest

import (
	"context"
	"sync"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/org"
)

// QuotaChecker учитывает событие организации и сообщает, укладывается ли
// она в месячную квоту. Реализация: OrgQuota.
type QuotaChecker interface {
	// CheckAndCount увеличивает счётчик событий организации за текущий месяц
	// и возвращает, разрешён ли приём. Handler зовёт его один раз на принятый
	// HTTP-запрос (не на событие внутри envelope — envelope может нести
	// несколько событий; считать per-request проще и это принятое
	// приближение к точной per-event квоте).
	CheckAndCount(ctx context.Context, orgID int64) (allowed bool, err error)
}

// quotaResolver — часть org.Service, нужная OrgQuota; *org.Service ей
// удовлетворяет.
type quotaResolver interface {
	Get(ctx context.Context, orgID int64) (org.Org, error)
	IncUsage(ctx context.Context, orgID int64, month time.Time) (int64, error)
}

// OrgQuota — QuotaChecker поверх org.Service. EventQuota организации
// кешируется на 30s (как KeyCache кеширует ключи) — чтобы не читать
// organizations на каждое событие; латентность применения новой квоты
// равна TTL кеша. Сам счётчик (IncUsage) кешировать нельзя — это источник
// правды для usage-репортинга, он идёт в БД на каждый вызов.
type OrgQuota struct {
	svc quotaResolver
	ttl time.Duration
	now func() time.Time

	mu      sync.Mutex
	entries map[int64]quotaEntry
}

type quotaEntry struct {
	quota   int64
	expires time.Time
}

func NewOrgQuota(svc *org.Service) *OrgQuota {
	return &OrgQuota{
		svc:     svc,
		ttl:     30 * time.Second,
		now:     time.Now,
		entries: map[int64]quotaEntry{},
	}
}

// quota возвращает EventQuota организации из кеша или org.Get.
func (q *OrgQuota) quota(ctx context.Context, orgID int64) (int64, error) {
	now := q.now()
	q.mu.Lock()
	if e, ok := q.entries[orgID]; ok && e.expires.After(now) {
		q.mu.Unlock()
		return e.quota, nil
	}
	q.mu.Unlock()

	o, err := q.svc.Get(ctx, orgID)
	if err != nil {
		return 0, err
	}
	q.mu.Lock()
	q.entries[orgID] = quotaEntry{quota: o.EventQuota, expires: now.Add(q.ttl)}
	q.mu.Unlock()
	return o.EventQuota, nil
}

// CheckAndCount — см. QuotaChecker. Квота 0 означает безлимит: счётчик всё
// равно растёт (для usage-репортинга), но приём никогда не блокируется.
func (q *OrgQuota) CheckAndCount(ctx context.Context, orgID int64) (bool, error) {
	quota, err := q.quota(ctx, orgID)
	if err != nil {
		return false, err
	}
	n, err := q.svc.IncUsage(ctx, orgID, time.Now())
	if err != nil {
		return false, err
	}
	if quota == 0 {
		return true, nil
	}
	return n <= quota, nil
}
