package ingest

import (
	"context"
	"sync"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/org"
)

type QuotaChecker interface {
	// 0 — квота исчерпана, want — влезло всё, иначе выбросить остаток и посчитать
	// в дропы. Считается за элемент, а не за HTTP-запрос.
	CheckAndCount(ctx context.Context, orgID int64, want int64) (granted int64, chargedAt time.Time, err error)

	// n списано в chargedAt (значение от CheckAndCount) — не пересчитывать.
	// Best-effort: вызывающий логирует ошибку и не меняет статус ответа клиенту.
	Refund(ctx context.Context, orgID int64, n int64, chargedAt time.Time) error
}

type quotaResolver interface {
	Get(ctx context.Context, orgID int64) (org.Org, error)
	CheckAndCountEvents(ctx context.Context, orgID int64, month time.Time, quota, want int64) (int64, error)
	CheckAndCountTransactions(ctx context.Context, orgID int64, month time.Time, quota, want int64) (int64, error)
	CheckAndCountMetrics(ctx context.Context, orgID int64, month time.Time, quota, want int64) (int64, error)
	CheckAndCountProfiles(ctx context.Context, orgID int64, month time.Time, quota, want int64) (int64, error)
	CheckAndCountLogs(ctx context.Context, orgID int64, month time.Time, quota, want int64) (int64, error)
	RefundEvents(ctx context.Context, orgID int64, month time.Time, n int64) error
	RefundTransactions(ctx context.Context, orgID int64, month time.Time, n int64) error
	RefundMetrics(ctx context.Context, orgID int64, month time.Time, n int64) error
	RefundProfiles(ctx context.Context, orgID int64, month time.Time, n int64) error
	RefundLogs(ctx context.Context, orgID int64, month time.Time, n int64) error
}

// квота кешируется на ttl, счётчик usage — нет: это источник правды, идёт в
// БД на каждый вызов.
type OrgQuota struct {
	svc quotaResolver
	ttl time.Duration
	now func() time.Time

	quotaOf    func(org.Org) int64
	checkCount func(ctx context.Context, orgID int64, month time.Time, quota, want int64) (int64, error)
	// месяц приходит извне — тот же chargedAt, что вернул CheckAndCount, не свежий q.now().
	refundCount func(ctx context.Context, orgID int64, month time.Time, n int64) error

	// короткий TTL: при over-quota флуде повторные обращения обслуживаются из
	// памяти, не бьют PG условным INSERT..ON CONFLICT.
	quotaNegTTL time.Duration

	mu      sync.Mutex
	entries map[int64]quotaEntry
	// orgID → момент истечения негативной записи «квота исчерпана».
	exhausted map[int64]time.Time
}

type quotaEntry struct {
	quota   int64
	expires time.Time
}

func NewOrgQuota(svc *org.Service) *OrgQuota {
	return newOrgQuota(svc,
		func(o org.Org) int64 { return o.EventQuota },
		svc.CheckAndCountEvents, svc.RefundEvents)
}

func NewOrgTransactionQuota(svc *org.Service) *OrgQuota {
	return newOrgQuota(svc,
		func(o org.Org) int64 { return o.TransactionQuota },
		svc.CheckAndCountTransactions, svc.RefundTransactions)
}

func NewOrgMetricQuota(svc *org.Service) *OrgQuota {
	return newOrgQuota(svc,
		func(o org.Org) int64 { return o.MetricQuota },
		svc.CheckAndCountMetrics, svc.RefundMetrics)
}

func NewOrgProfileQuota(svc *org.Service) *OrgQuota {
	return newOrgQuota(svc,
		func(o org.Org) int64 { return o.ProfileQuota },
		svc.CheckAndCountProfiles, svc.RefundProfiles)
}

func NewOrgLogQuota(svc *org.Service) *OrgQuota {
	return newOrgQuota(svc,
		func(o org.Org) int64 { return o.LogQuota },
		svc.CheckAndCountLogs, svc.RefundLogs)
}

func newOrgQuota(
	svc quotaResolver,
	quotaOf func(org.Org) int64,
	checkCount func(ctx context.Context, orgID int64, month time.Time, quota, want int64) (int64, error),
	refundCount func(ctx context.Context, orgID int64, month time.Time, n int64) error,
) *OrgQuota {
	return &OrgQuota{
		svc:         svc,
		ttl:         30 * time.Second,
		quotaNegTTL: 5 * time.Second,
		now:         time.Now,
		quotaOf:     quotaOf,
		checkCount:  checkCount,
		refundCount: refundCount,
		entries:     map[int64]quotaEntry{},
		exhausted:   map[int64]time.Time{},
	}
}

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
	quota := q.quotaOf(o)
	q.mu.Lock()
	if len(q.entries) >= maxKeyCacheEntries {
		q.evictEntries(now)
	}
	q.entries[orgID] = quotaEntry{quota: quota, expires: now.Add(q.ttl)}
	q.mu.Unlock()
	return quota, nil
}

// квота 0 — безлимит: счётчик растёт для usage, но приём не блокируется. При
// исчерпании счётчик не растёт.
func (q *OrgQuota) CheckAndCount(ctx context.Context, orgID int64, want int64) (int64, time.Time, error) {
	if want <= 0 {
		return 0, time.Time{}, nil
	}
	// при недавнем исчерпании не идём в PG — иначе флуд бьёт транзакцией с row-lock'ом.
	if q.recentlyExhausted(orgID) {
		return 0, time.Time{}, nil
	}
	quota, err := q.quota(ctx, orgID)
	if err != nil {
		return 0, time.Time{}, err
	}
	// q.now(), не time.Now() — часы инжектируются ради тестов. chargedAt
	// фиксируется один раз и уходит в Refund как есть.
	chargedAt := q.now()
	granted, err := q.checkCount(ctx, orgID, chargedAt, quota, want)
	if err != nil {
		return 0, time.Time{}, err
	}
	if granted < want {
		q.markExhausted(orgID)
	}
	return granted, chargedAt, nil
}

func (q *OrgQuota) Refund(ctx context.Context, orgID int64, n int64, chargedAt time.Time) error {
	if n <= 0 {
		return nil
	}
	return q.refundCount(ctx, orgID, chargedAt, n)
}

func (q *OrgQuota) recentlyExhausted(orgID int64) bool {
	now := q.now()
	q.mu.Lock()
	defer q.mu.Unlock()
	exp, ok := q.exhausted[orgID]
	if !ok {
		return false
	}
	if !exp.After(now) {
		delete(q.exhausted, orgID)
		return false
	}
	return true
}

// при переполнении вытесняем истёкшие, не всю карту — иначе организации, стабильно
// упирающиеся в квоту, снова пошли бы в PostgreSQL на каждом событии.
func (q *OrgQuota) markExhausted(orgID int64) {
	now := q.now()
	q.mu.Lock()
	if len(q.exhausted) >= maxKeyCacheEntries {
		q.evictExhausted(now)
	}
	q.exhausted[orgID] = now.Add(q.quotaNegTTL)
	q.mu.Unlock()
}

func (q *OrgQuota) evictEntries(now time.Time) {
	for id, e := range q.entries {
		if !e.expires.After(now) {
			delete(q.entries, id)
		}
	}
	if len(q.entries) < maxKeyCacheEntries {
		return
	}
	drop := len(q.entries) / 10
	if drop == 0 {
		drop = 1
	}
	for id := range q.entries {
		if drop == 0 {
			break
		}
		delete(q.entries, id)
		drop--
	}
}

func (q *OrgQuota) evictExhausted(now time.Time) {
	for id, exp := range q.exhausted {
		if !exp.After(now) {
			delete(q.exhausted, id)
		}
	}
	if len(q.exhausted) < maxKeyCacheEntries {
		return
	}
	drop := len(q.exhausted) / 10
	if drop == 0 {
		drop = 1
	}
	for id := range q.exhausted {
		if drop == 0 {
			break
		}
		delete(q.exhausted, id)
		drop--
	}
}
