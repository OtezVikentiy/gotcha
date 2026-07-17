package ingest

import (
	"context"
	"sync"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/org"
)

// QuotaChecker учитывает единицу приёма (событие или транзакцию) и сообщает,
// укладывается ли организация в месячную квоту. Реализация: OrgQuota.
type QuotaChecker interface {
	// CheckAndCount увеличивает счётчик организации за текущий месяц и
	// возвращает, разрешён ли приём. Handler зовёт его один раз на принятый
	// HTTP-запрос (не на событие внутри envelope — envelope может нести
	// несколько событий; считать per-request проще и это принятое
	// приближение к точной per-event квоте).
	CheckAndCount(ctx context.Context, orgID int64) (allowed bool, err error)
}

// quotaResolver — часть org.Service, нужная OrgQuota; *org.Service ей
// удовлетворяет.
type quotaResolver interface {
	Get(ctx context.Context, orgID int64) (org.Org, error)
	CheckAndCountEvents(ctx context.Context, orgID int64, month time.Time, quota int64) (bool, error)
	CheckAndCountTransactions(ctx context.Context, orgID int64, month time.Time, quota int64) (bool, error)
	CheckAndCountMetrics(ctx context.Context, orgID int64, month time.Time, quota int64) (bool, error)
	CheckAndCountProfiles(ctx context.Context, orgID int64, month time.Time, quota int64) (bool, error)
}

// OrgQuota — QuotaChecker поверх org.Service. Квота организации кешируется на
// 30s (как KeyCache кеширует ключи) — чтобы не читать organizations на каждое
// событие; латентность применения новой квоты равна TTL кеша. Сам счётчик
// (IncUsage) кешировать нельзя — это источник правды для usage-репортинга, он
// идёт в БД на каждый вызов.
//
// Один и тот же тип обслуживает две НЕЗАВИСИМЫЕ квоты: ошибок
// (NewOrgQuota → event_quota/events_count) и транзакций
// (NewOrgTransactionQuota → transaction_quota/transactions_count). Разные
// экземпляры, разные колонки и разные кеши: исчерпанный бюджет транзакций не
// закрывает приём ошибок и наоборот.
type OrgQuota struct {
	svc quotaResolver
	ttl time.Duration
	now func() time.Time

	// quotaOf — какую из квот организации проверяет этот экземпляр.
	quotaOf func(org.Org) int64
	// checkCount — условный атомарный инкремент соответствующего счётчика:
	// растит его лишь если приём укладывается в quota, иначе отклоняет БЕЗ
	// инкремента (ARCH-L1: отвергнутое не считается в usage).
	checkCount func(ctx context.Context, orgID int64, month time.Time, quota int64) (bool, error)

	mu      sync.Mutex
	entries map[int64]quotaEntry
}

type quotaEntry struct {
	quota   int64
	expires time.Time
}

// NewOrgQuota — квота ОШИБОК: event_quota против org_usage.events_count.
func NewOrgQuota(svc *org.Service) *OrgQuota {
	return newOrgQuota(svc,
		func(o org.Org) int64 { return o.EventQuota },
		svc.CheckAndCountEvents)
}

// NewOrgTransactionQuota — квота ТРАНЗАКЦИЙ: transaction_quota против
// org_usage.transactions_count. Отдельный счётчик — транзакции не тратят
// бюджет ошибок.
func NewOrgTransactionQuota(svc *org.Service) *OrgQuota {
	return newOrgQuota(svc,
		func(o org.Org) int64 { return o.TransactionQuota },
		svc.CheckAndCountTransactions)
}

// NewOrgMetricQuota — квота МЕТРИК: metric_quota против org_usage.metrics_count.
// Отдельный счётчик — метрики не тратят бюджет ошибок/транзакций.
func NewOrgMetricQuota(svc *org.Service) *OrgQuota {
	return newOrgQuota(svc,
		func(o org.Org) int64 { return o.MetricQuota },
		svc.CheckAndCountMetrics)
}

// NewOrgProfileQuota — квота ПРОФИЛЕЙ: profile_quota против org_usage.profiles_count.
func NewOrgProfileQuota(svc *org.Service) *OrgQuota {
	return newOrgQuota(svc,
		func(o org.Org) int64 { return o.ProfileQuota },
		svc.CheckAndCountProfiles)
}

func newOrgQuota(
	svc quotaResolver,
	quotaOf func(org.Org) int64,
	checkCount func(ctx context.Context, orgID int64, month time.Time, quota int64) (bool, error),
) *OrgQuota {
	return &OrgQuota{
		svc:        svc,
		ttl:        30 * time.Second,
		now:        time.Now,
		quotaOf:    quotaOf,
		checkCount: checkCount,
		entries:    map[int64]quotaEntry{},
	}
}

// quota возвращает квоту организации из кеша или org.Get.
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
	q.entries[orgID] = quotaEntry{quota: quota, expires: now.Add(q.ttl)}
	q.mu.Unlock()
	return quota, nil
}

// CheckAndCount — см. QuotaChecker. Квота 0 означает безлимит: счётчик всё
// равно растёт (для usage-репортинга), но приём никогда не блокируется. При
// исчерпанной квоте счётчик НЕ инкрементится (ARCH-L1: отвергнутое не считается
// в usage) — условный инкремент атомарен в БД, без гонки read-then-write.
func (q *OrgQuota) CheckAndCount(ctx context.Context, orgID int64) (bool, error) {
	quota, err := q.quota(ctx, orgID)
	if err != nil {
		return false, err
	}
	return q.checkCount(ctx, orgID, time.Now(), quota)
}
