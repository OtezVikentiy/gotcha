package ingest

import (
	"context"
	"sync"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/org"
)

// QuotaChecker учитывает единицы приёма (события, транзакции, метрики,
// профили) и сообщает, сколько из них укладывается в месячную квоту.
// Реализация: OrgQuota.
type QuotaChecker interface {
	// CheckAndCount списывает want единиц из квоты организации за текущий
	// месяц и возвращает, СКОЛЬКО удалось списать: 0 — квота исчерпана, want —
	// влезло всё, промежуточное значение — влезла часть, остаток вызывающий
	// обязан выбросить и посчитать в дропы.
	//
	// Считается ЗА ЭЛЕМЕНТ, а не за HTTP-запрос. Раньше за запрос: конверт с
	// тысячей событий стоил столько же, сколько одно событие, — квота
	// обходилась на три-четыре порядка, и ровно на столько же врал org_usage,
	// который для оператора является источником правды по потреблению.
	CheckAndCount(ctx context.Context, orgID int64, want int64) (granted int64, err error)
}

// quotaResolver — часть org.Service, нужная OrgQuota; *org.Service ей
// удовлетворяет.
type quotaResolver interface {
	Get(ctx context.Context, orgID int64) (org.Org, error)
	CheckAndCountEvents(ctx context.Context, orgID int64, month time.Time, quota, want int64) (int64, error)
	CheckAndCountTransactions(ctx context.Context, orgID int64, month time.Time, quota, want int64) (int64, error)
	CheckAndCountMetrics(ctx context.Context, orgID int64, month time.Time, quota, want int64) (int64, error)
	CheckAndCountProfiles(ctx context.Context, orgID int64, month time.Time, quota, want int64) (int64, error)
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
	checkCount func(ctx context.Context, orgID int64, month time.Time, quota, want int64) (int64, error)

	// quotaNegTTL — время жизни НЕГАТИВНОЙ записи «квота исчерпана». Короткий TTL
	// (аналог negTTL у KeyCache): при over-quota флуде повторные обращения той же
	// орги обслуживаются из памяти и НЕ бьют по PG условным INSERT..ON CONFLICT
	// (row-lock). Квота обнуляется раз в месяц, так что задержка «снова
	// принимаем» после роста квоты ≤ этого TTL — приемлемо.
	quotaNegTTL time.Duration

	mu      sync.Mutex
	entries map[int64]quotaEntry
	// exhausted — orgID → момент истечения негативной записи «квота исчерпана».
	exhausted map[int64]time.Time
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
	checkCount func(ctx context.Context, orgID int64, month time.Time, quota, want int64) (int64, error),
) *OrgQuota {
	return &OrgQuota{
		svc:         svc,
		ttl:         30 * time.Second,
		quotaNegTTL: 5 * time.Second,
		now:         time.Now,
		quotaOf:     quotaOf,
		checkCount:  checkCount,
		entries:     map[int64]quotaEntry{},
		exhausted:   map[int64]time.Time{},
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
	// Кеш квот раньше не имел границы вообще: записи только перезаписывались, а
	// по истечении TTL не удалялись никогда, поэтому карта росла до числа
	// организаций, когда-либо приходивших на приём, и обратно не сжималась.
	if len(q.entries) >= maxKeyCacheEntries {
		q.evictEntries(now)
	}
	q.entries[orgID] = quotaEntry{quota: quota, expires: now.Add(q.ttl)}
	q.mu.Unlock()
	return quota, nil
}

// CheckAndCount — см. QuotaChecker. Квота 0 означает безлимит: счётчик всё
// равно растёт (для usage-репортинга), но приём никогда не блокируется. При
// исчерпанной квоте счётчик НЕ инкрементится (отвергнутое не считается в usage).
func (q *OrgQuota) CheckAndCount(ctx context.Context, orgID int64, want int64) (int64, error) {
	if want <= 0 {
		return 0, nil
	}
	// Короткое замыкание over-quota: если недавно уже видели исчерпание, не идём
	// в PG вовсе (иначе флуд при исчерпанной квоте бьёт транзакцией с row-lock'ом).
	// Кешируем ТОЛЬКО негатив — позитив обязан инкрементить счётчик usage в БД.
	if q.recentlyExhausted(orgID) {
		return 0, nil
	}
	quota, err := q.quota(ctx, orgID)
	if err != nil {
		return 0, err
	}
	// q.now(), а НЕ time.Now(): часы инжектируются ради тестов, и единственное
	// место, где здесь стояло реальное время, — это же и есть граница месяца
	// (checkCount считает usage за месяц от переданного момента). Из-за неё
	// поведение «квота обнулилась 1-го числа» было непроверяемым в принципе,
	// хотя это биллинговая логика.
	granted, err := q.checkCount(ctx, orgID, q.now(), quota, want)
	if err != nil {
		return 0, err
	}
	// Негативную запись ставим, когда влезло НЕ ВСЁ: значит квота уперлась в
	// потолок, и следующему запросу тоже ловить нечего. Ставим и при частичном
	// списании — остаток квоты нулевой по построению.
	if granted < want {
		q.markExhausted(orgID)
	}
	return granted, nil
}

// recentlyExhausted сообщает, есть ли живая негативная запись «квота исчерпана»
// для орги.
func (q *OrgQuota) recentlyExhausted(orgID int64) bool {
	now := q.now()
	q.mu.Lock()
	defer q.mu.Unlock()
	exp, ok := q.exhausted[orgID]
	if !ok {
		return false
	}
	if !exp.After(now) {
		delete(q.exhausted, orgID) // истёкшую запись подчищаем на месте
		return false
	}
	return true
}

// markExhausted кладёт негативную запись «квота исчерпана» на quotaNegTTL.
//
// При переполнении вытесняем истёкшие записи, а не стираем карту целиком. Полный
// сброс здесь не ломал учёт (квота всё равно проверяется в БД), но выбрасывал
// именно те записи, ради которых кеш и заведён: организации, стабильно
// упирающиеся в квоту, снова начинали ходить в PostgreSQL на каждом событии —
// то есть нагрузка подскакивала ровно в момент, когда инстанс и так под потоком.
func (q *OrgQuota) markExhausted(orgID int64) {
	now := q.now()
	q.mu.Lock()
	if len(q.exhausted) >= maxKeyCacheEntries {
		q.evictExhausted(now)
	}
	q.exhausted[orgID] = now.Add(q.quotaNegTTL)
	q.mu.Unlock()
}

// evictEntries освобождает место в кеше квот: сперва истёкшие (их потеря
// бесплатна), затем десятая часть произвольных — порядок обхода map и есть
// случайность. Вызывается под mu.
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

// evictExhausted — то же для негативных записей.
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
