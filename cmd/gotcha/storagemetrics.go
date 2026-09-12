package main

import (
	"context"
	"log/slog"
	"math"
	"sync/atomic"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/export"
	"gitflic.ru/otezvikentiy/gotcha/internal/selfmetrics"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Не привязан к скрейпу /metrics — иначе частоту опроса БД задавал бы чужой Prometheus.
const storagePollInterval = 5 * time.Minute

// Каждый опрос — под этим таймаутом: подвисшее хранилище не должно подвесить
// весь цикл сборщика.
const storagePollTimeout = 5 * time.Second

// Место на ТОМЕ хранилища, не логический размер БД — см. pgUsedBytesSource,
// почему PostgreSQL этот интерфейс не реализует.
type diskSource interface {
	storeLabel() string
	// Только фоновым опросом (registerStorageMetrics/storagePollers.Run), никогда
	// из value() метрики — нарушит инвариант selfmetrics: метрики не ходят в БД.
	stat(ctx context.Context) (free, total uint64, err error)
}

// system.disks отдаёт реальное место на томе, не логический размер данных.
// Берём диск с наименьшим запасом — отказ придёт от того, что кончится первым.
type chDiskSource struct{ conn driver.Conn }

func (chDiskSource) storeLabel() string { return "clickhouse" }

func (s chDiskSource) stat(ctx context.Context) (free, total uint64, err error) {
	row := s.conn.QueryRow(ctx,
		"SELECT free_space, total_space FROM system.disks ORDER BY free_space ASC LIMIT 1")
	if err := row.Scan(&free, &total); err != nil {
		return 0, 0, err
	}
	return free, total, nil
}

type diskSnapshot struct{ free, total uint64 }

// freeBytes/totalBytes читают atomic.Load, без сети и блокировок — того
// требует selfmetrics от value()-функций.
type diskPoller struct {
	source diskSource
	// nil до первого опроса; при ошибке снимок не обнуляется — NaN тут значит
	// «неизвестно», а не «ноль свободно».
	snap atomic.Pointer[diskSnapshot]
}

// Текст ошибки "storage metrics: poll failed" — контракт: self-monitoring.md
// называет его дословно.
func (p *diskPoller) poll(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, storagePollTimeout)
	defer cancel()
	free, total, err := p.source.stat(ctx)
	if err != nil {
		slog.Warn("storage metrics: poll failed", "store", p.source.storeLabel(), "error", err)
		return err
	}
	p.snap.Store(&diskSnapshot{free: free, total: total})
	return nil
}

// Ошибка уже залогирована в poll; сбой одного источника не должен портить
// остальные метрики.
func (p *diskPoller) refresh(ctx context.Context) {
	_ = p.poll(ctx)
}

func (p *diskPoller) freeBytes() float64 {
	s := p.snap.Load()
	if s == nil {
		return math.NaN()
	}
	return float64(s.free)
}

func (p *diskPoller) totalBytes() float64 {
	s := p.snap.Load()
	if s == nil {
		return math.NaN()
	}
	return float64(s.total)
}

type storagePollers struct{ pollers []*diskPoller }

// Опрос синхронный перед регистрацией — иначе метрика NaN до первого тика.
// Ошибка игнорируется намеренно: опрос диска не имеет права быть фатальным.
func registerStorageMetrics(r *selfmetrics.Registry, sources ...diskSource) *storagePollers {
	sp := &storagePollers{}
	for _, src := range sources {
		p := &diskPoller{source: src}
		label := src.storeLabel()
		_ = startupStage("storage poll", label, func() error { return p.poll(context.Background()) })
		lbl := map[string]string{"store": label}
		r.Add(selfmetrics.Gauge, "gotcha_storage_free_bytes",
			"Free bytes on the volume backing the store's data (not the logical size of the data itself). NaN until the first successful poll.",
			lbl, p.freeBytes)
		r.Add(selfmetrics.Gauge, "gotcha_storage_total_bytes",
			"Total bytes on the volume backing the store's data. NaN until the first successful poll.",
			lbl, p.totalBytes)
		sp.pollers = append(sp.pollers, p)
	}
	return sp
}

// Последовательно, не по горутине на источник: запросы лёгкие, не стоит
// платить синхронизацией.
func (sp *storagePollers) Run(ctx context.Context) {
	pollLoop(ctx, func(ctx context.Context) {
		for _, p := range sp.pollers {
			p.refresh(ctx)
		}
	})
}

func pollLoop(ctx context.Context, refresh func(ctx context.Context)) {
	ticker := time.NewTicker(storagePollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			refresh(ctx)
		}
	}
}

// PostgreSQL без superuser не узнаёт свободное место НА ТОМЕ, только размер БД —
// реализует usedBytesSource, а не diskSource, чтобы не выдать занятое за свободное.
type pgUsedBytesSource struct{ pool *pgxpool.Pool }

func (s pgUsedBytesSource) stat(ctx context.Context) (used uint64, err error) {
	err = s.pool.QueryRow(ctx, "SELECT pg_database_size(current_database())").Scan(&used)
	return used, err
}

type exportDirUsedBytesSource struct{ dir string }

func (s exportDirUsedBytesSource) stat(context.Context) (uint64, error) {
	n, err := export.DirSize(s.dir)
	if err != nil {
		return 0, err
	}
	return uint64(n), nil
}

type usedBytesSource interface {
	stat(ctx context.Context) (uint64, error)
}

// Как diskPoller, но для одного значения вместо пары free/total.
type usedBytesPoller struct {
	source usedBytesSource
	label  string
	v      atomic.Pointer[uint64]
}

func (p *usedBytesPoller) poll(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, storagePollTimeout)
	defer cancel()
	v, err := p.source.stat(ctx)
	if err != nil {
		slog.Warn("storage metrics: poll failed", "store", p.label, "error", err)
		return err
	}
	p.v.Store(&v)
	return nil
}

func (p *usedBytesPoller) refresh(ctx context.Context) {
	_ = p.poll(ctx)
}

func (p *usedBytesPoller) value() float64 {
	v := p.v.Load()
	if v == nil {
		return math.NaN()
	}
	return float64(*v)
}

func (p *usedBytesPoller) Run(ctx context.Context) { pollLoop(ctx, p.refresh) }

func registerUsedBytesMetric(r *selfmetrics.Registry, label string, source usedBytesSource) *usedBytesPoller {
	p := &usedBytesPoller{source: source, label: label}
	_ = startupStage("storage poll", label, func() error { return p.poll(context.Background()) })
	r.Add(selfmetrics.Gauge, "gotcha_storage_used_bytes",
		"Bytes the store's own data currently occupies on disk (not free/total volume space — see gotcha_storage_free_bytes/total_bytes for stores that can report that). NaN until the first successful poll.",
		map[string]string{"store": label}, p.value)
	return p
}
