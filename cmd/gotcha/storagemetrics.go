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

// Находка №40: полный список самометрик — буферы, очередь, доставка, потолок
// кучи, чистка — и ни слова про диск. Девяносто пять процентов заполнения и
// сто выглядят одинаково: до отказа нет ни одного сигнала, после — только рост
// отброшенных вставок и провал /readyz, по которым скорее заподозришь сеть или
// строку подключения, чем кончившееся место.
//
// storagePollInterval — как часто опрашиваются хранилища. Не «на каждый скрейп
// /metrics» — тот же приём, что у notify.Stats (internal/notify/stats.go):
// частоту скрейпа задаёт чужой Prometheus, и сажать на этот путь запрос к БД
// значит отдать нагрузку под чужой контроль. Заполнение диска меняется
// часами-днями, а не секундами, поэтому интервал можно взять куда шире, чем у
// очереди доставки (там 15с) — 5 минут более чем достаточно.
const storagePollInterval = 5 * time.Minute

// storagePollTimeout — потолок одного опроса. Оба соединения к этому моменту
// уже прошли Ping при старте процесса (db.NewPostgres/db.NewClickHouse); если
// именно этот запрос подвиснет, он не должен подвесить весь цикл сборщика —
// опрос свободного места не имеет права стать новым источником отказа.
const storagePollTimeout = 5 * time.Second

// diskSource — источник данных о свободном/общем месте на ТОМЕ, где
// хранилище держит свои данные (а не о логическом размере самой БД — это
// разные величины, см. pgUsedBytesSource ниже про то, почему PostgreSQL этот
// интерфейс не реализует).
type diskSource interface {
	// storeLabel — значение метки store у обеих метрик ("clickhouse", ...).
	storeLabel() string
	// stat читает свободно/всего байт. Вызывается редко и под таймаутом
	// фоновым опросом (registerStorageMetrics/storagePollers.Run) — НИКОГДА
	// из value() уже зарегистрированной метрики: это нарушило бы инвариант
	// пакета selfmetrics («ни одна метрика не должна ходить в БД» — см. его
	// докблок), который существует именно для того, чтобы /metrics отвечал и
	// тогда, когда БД недоступна.
	stat(ctx context.Context) (free, total uint64, err error)
}

// chDiskSource — ClickHouse: system.disks отдаёт РЕАЛЬНОЕ свободное/общее
// место на смонтированном томе (проверено на живом стенде, 25.3: path
// /var/lib/clickhouse, free_space/total_space — то, что показал бы df на этом
// пути), а не логический размер данных. Берём диск с наименьшим запасом: если
// storage policy размазана по нескольким дискам, отказ придёт от того, что
// кончится первым — не от условного «в среднем есть место».
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

// diskSnapshot — последний успешно прочитанный результат stat().
type diskSnapshot struct{ free, total uint64 }

// diskPoller кеширует последний снимок одного diskSource. Чтение метрики
// (freeBytes/totalBytes) — это atomic.Load, без сети и без блокировок: именно
// это требует докблок selfmetrics от value()-функций.
type diskPoller struct {
	source diskSource
	// snap — nil до первого успешного опроса. См. refresh: при ошибке снимок
	// НЕ обнуляется (тот же приём, что у notify.Stats.refresh — обнулять
	// нельзя, потерянные данные тут страшнее, чем протухшие) и НЕ обнуляется в
	// 0 при самой первой попытке: метрика «свободно» со значением 0 читалась
	// бы как «диск заполнен», хотя на деле просто ещё не было ни одного
	// успешного опроса. NaN тут — тот же приём, каким в графиках (web/svg.go,
	// web/metrics.go) уже помечают дырку в данных: явно «неизвестно», а не
	// ложный ноль. Prometheus text exposition NaN как значение поддерживает.
	snap atomic.Pointer[diskSnapshot]
}

// poll — один опрос под собственным таймаутом. Ошибка не паникует: она
// логируется здесь же, ОДНИМ местом на оба вызывающих (фоновый refresh и
// приоритетный опрос при регистрации, см. startupStage-обёртку в
// registerStorageMetrics) — это тот самый текст, "storage metrics: poll
// failed", который документация (self-monitoring.md) называет дословно как
// то, что нужно искать в логе при виде NaN: он обязан звучать одинаково
// независимо от того, был ли это первый опрос или сотый по счёту, иначе
// инструкция «ищите эту строку» перестала бы быть верной для части случаев.
// Ошибка также возвращается вызывающему — тот решает, что с ней делать
// дальше (refresh её больше никак не использует; startupStage добавляет
// поверх начало/конец/длительность как отдельный, более крупный кадр).
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

// refresh — фоновый опрос (см. storagePollers.Run). Опрос молча оставляет
// прежний (возможно, ещё не установленный) снимок при ошибке — она уже
// залогирована внутри poll, фоновому циклу больше нечего с ней делать: сбой
// одного источника не должен портить остальные метрики и не должен
// становиться новым источником отказа.
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

// storagePollers — все зарегистрированные diskSource разом; storagePollers.Run
// обновляет их периодически, пока не отменят ctx (тот же приём вызова, что у
// notifyStats.RunSnapshots(ctx, outbox) в main — см. run()).
type storagePollers struct{ pollers []*diskPoller }

// registerStorageMetrics регистрирует gotcha_storage_free_bytes и
// gotcha_storage_total_bytes для каждого источника под меткой store=<имя> и
// делает начальный синхронный опрос (под тем же таймаутом, что и все
// последующие), чтобы сразу после регистрации, ещё до первого тика фонового
// обновления, метрика показывала настоящее значение, а не NaN. Дальнейшее
// обновление запускается отдельно — go registerStorageMetrics(...).Run(ctx),
// как и остальные фоновые сборщики в run() (main.go).
//
// Приоритетный опрос обёрнут в startupStage (тот же приём, что и у этапов
// миграций в migrate.go, вынесенный в общий примитив ради этого повторного
// использования): без него регистрация двух источников подряд, оба на
// таймауте по storagePollTimeout, могла молча съесть до 10с между строкой
// «применяю миграции» и строкой «слушаю» — то самое молчание, ради победы
// над которым и заведён весь этот файл (см. находку №40 выше). На ошибке
// строк в логе будет две, на разных уровнях кадра — как и у миграций (см.
// докблок migrationStage про run() и "gotcha failed"): poll сам пишет Warn
// "storage metrics: poll failed" с причиной (этот текст — контракт,
// self-monitoring.md называет его дословно, он не должен зависеть от того,
// первый это опрос или сотый), startupStage поверх пишет Error "storage poll
// failed" с длительностью — это уровень «застрял именно этот шаг старта».
// Ошибка не прерывает регистрацию (`_ =`, намеренно): опрос диска, в отличие
// от миграций, не имеет права быть фатальным — поллер и так переживает
// неудачный опрос, оставаясь в состоянии NaN (см. diskPoller.snap) до
// следующего тика.
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

// Run обновляет показатели всех источников на каждый тик, пока не отменят
// ctx. Опрос идёт последовательно по срезу, а не параллельно на горутину:
// это лёгкие запросы (system.disks, pg_database_size), и не стоит платить
// отдельной синхронизацией за то, что и так укладывается в доли секунды.
func (sp *storagePollers) Run(ctx context.Context) {
	pollLoop(ctx, func(ctx context.Context) {
		for _, p := range sp.pollers {
			p.refresh(ctx)
		}
	})
}

// pollLoop — общий тикер-цикл для storagePollers.Run и usedBytesPoller.Run:
// оба сборщика опрашивают с одним и тем же интервалом и одинаково реагируют
// на отмену ctx, дублировать select незачем.
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

// pgUsedBytesSource — почему PostgreSQL не реализует diskSource.
//
// На открытом соединении обычного (не superuser) сервисного пользователя нет
// способа узнать свободное/общее место НА ТОМЕ:
//   - pg_database_size(current_database()) — логический размер занятого самой
//     БД места, не остаток на диске;
//   - pg_stat_file даёт метаданные ОДНОГО файла (не системный df) и по
//     умолчанию требует superuser или роль pg_read_server_files — новых прав
//     заводить не просили (задача явно требует обойтись без них), а в проде
//     сервисный пользователь их обычно и не имеет;
//   - стандартных SQL-функций уровня «df для тома» в PostgreSQL нет вовсе —
//     это ОС-уровневая величина, а не СУБД-уровневая.
//
// Раздать pg_database_size под именем «free»/«total» значило бы солгать
// метрикой: занятое место с ростом БД растёт, а не убывает, то есть метрика
// «свободно» показывала бы противоположность своему названию — именно то,
// от чего предостерегает задача («метрика с именем «свободно» и значением
// «занято» хуже отсутствующей»). Поэтому PostgreSQL публикует отдельную,
// честно названную метрику — сколько места УЖЕ занято данными БД; сопоставить
// её с известным размером тома (обычно один том на инстанс PostgreSQL) —
// дело оператора, а не выдумка за него.
type pgUsedBytesSource struct{ pool *pgxpool.Pool }

func (s pgUsedBytesSource) stat(ctx context.Context) (used uint64, err error) {
	err = s.pool.QueryRow(ctx, "SELECT pg_database_size(current_database())").Scan(&used)
	return used, err
}

// exportDirUsedBytesSource — источник для registerUsedBytesMetric под меткой
// store="exports" (P1-OPS-1): export.DirSize суммирует файлы каталога тем же
// приёмом, что и Worker.process перед проверкой DiskBudget (worker.go) —
// каталог выгрузок единственный кусок диска, которым распоряжается само
// приложение, и до этой метрики был единственным неизмеряемым в
// gotcha_storage_used_bytes (соседняя store="postgres" уже зарегистрирована).
// Опрашивается фоновым pollLoop (та же гигиена, что у
// storagePollers/usedBytesPoller выше), а не на каждый скрап /metrics.
type exportDirUsedBytesSource struct{ dir string }

func (s exportDirUsedBytesSource) stat(context.Context) (uint64, error) {
	n, err := export.DirSize(s.dir)
	if err != nil {
		return 0, err
	}
	return uint64(n), nil
}

// usedBytesSource — источник для registerUsedBytesMetric; pgUsedBytesSource
// ему удовлетворяет.
type usedBytesSource interface {
	stat(ctx context.Context) (uint64, error)
}

// usedBytesPoller — как diskPoller, но для одного числа вместо пары
// free/total; та же логика NaN-до-первого-успеха и сохранения снимка при
// ошибке (см. diskPoller.refresh).
type usedBytesPoller struct {
	source usedBytesSource
	label  string
	v      atomic.Pointer[uint64]
}

// poll — см. diskPoller.poll: тот же приём и тот же текст лога на ошибку
// ("storage metrics: poll failed"), общий на фоновый и приоритетный опрос.
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

// Run — см. storagePollers.Run.
func (p *usedBytesPoller) Run(ctx context.Context) { pollLoop(ctx, p.refresh) }

// registerUsedBytesMetric регистрирует gotcha_storage_used_bytes{store=label}
// — см. pgUsedBytesSource про то, почему это отдельная метрика, а не
// free_bytes/total_bytes под чужим смыслом. Начальный синхронный опрос — тем
// же приёмом (startupStage), что в registerStorageMetrics, и по той же
// причине (см. её докблок).
func registerUsedBytesMetric(r *selfmetrics.Registry, label string, source usedBytesSource) *usedBytesPoller {
	p := &usedBytesPoller{source: source, label: label}
	_ = startupStage("storage poll", label, func() error { return p.poll(context.Background()) })
	r.Add(selfmetrics.Gauge, "gotcha_storage_used_bytes",
		"Bytes the store's own data currently occupies on disk (not free/total volume space — see gotcha_storage_free_bytes/total_bytes for stores that can report that). NaN until the first successful poll.",
		map[string]string{"store": label}, p.value)
	return p
}
