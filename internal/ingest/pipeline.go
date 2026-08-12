package ingest

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/alert"
	"gitflic.ru/otezvikentiy/gotcha/internal/event"
	"gitflic.ru/otezvikentiy/gotcha/internal/fingerprint"
	"gitflic.ru/otezvikentiy/gotcha/internal/issue"
	"gitflic.ru/otezvikentiy/gotcha/internal/trace"
)

// perfDetectBudget — бюджет ВСЕЙ детекции по одной транзакции: чтение настроек
// проекта, запись всех находок и их алерты. Бюджет именно общий, а не на каждую
// находку: с бюджетом на находку транзакция с максимумом находок
// (maxFindingsPerTransaction = 20) при медленной PG удерживала бы одного из
// четырёх воркеров до 20 x 5с ≈ 100с — а через ту же очередь на 1000 слотов идут
// события об ОШИБКАХ, и они в это время дропаются с warn-логом. Приём ошибок
// важнее полноты детекции: хвост находок, не поместившийся в бюджет,
// пропускается с warn-логом (та же проблема найдётся на следующей транзакции —
// она воспроизводится на каждом запросе к эндпойнту).
const perfDetectBudget = 10 * time.Second

// AlertSink получает сигналы о смене состояния issue (новая группа,
// регрессия), чтобы решить, нужно ли поставить уведомления в очередь.
// Отдельный интерфейс (а не прямая зависимость от *alert.Evaluator) держит
// Pipeline тестируемым без реальной БД под алертингом и делает поле
// необязательным: nil (см. Pipeline.Alerts) значит "алертинг выключен".
type AlertSink interface {
	OnIssue(ctx context.Context, ev alert.Event)
}

// SpanSink принимает семплированные транзакции для записи в ClickHouse;
// *trace.SpanWriter ему удовлетворяет. Отдельный интерфейс (а не прямая
// зависимость от *trace.SpanWriter) держит Pipeline тестируемым без CH и
// делает поле необязательным: nil (см. Pipeline.Spans) значит «трейсинг
// выключен».
type SpanSink interface {
	// orgID нужен только для per-org атрибуции дропов буфера писателя в
	// org_usage.dropped_transactions (см. trace.SpanWriter.SetDropSink); на саму
	// запись в CH не влияет.
	Add(orgID, projectID int64, t trace.Transaction)
}

// PerfSink записывает находку детекторов производительности в perf_issues (PG);
// *trace.IssueService ему удовлетворяет. Отдельный интерфейс (а не прямая
// зависимость от *trace.IssueService) держит Pipeline тестируемым без PG и
// делает поле необязательным: nil (см. Pipeline.Perf) значит «детекторы
// выключены».
type PerfSink interface {
	Record(ctx context.Context, projectID int64, f trace.Finding, traceID string) (trace.RecordResult, error)
}

// PerfNotifier алертит о ПЕРВОМ обнаружении проблемы производительности и о её
// регрессии (была resolved — снова обнаружена); *trace.OutboxNotifier ему
// удовлетворяет. nil (см. Pipeline.PerfAlerts) — алерты по производительности
// выключены, детекция всё равно идёт.
type PerfNotifier interface {
	NotifyNew(ctx context.Context, projectID int64, iss trace.PerfIssue) error
	NotifyRegression(ctx context.Context, projectID int64, iss trace.PerfIssue) error
}

// issueUpserter — то, что нужно пайплайну от issue.Service: апсерт группы и
// чтение (для times_seen в алерте). *issue.Service ему удовлетворяет.
// Отдельный интерфейс (а не прямая зависимость от *issue.Service) держит
// событийный путь Pipeline тестируемым без PG.
type issueUpserter interface {
	Upsert(ctx context.Context, projectID int64, fingerprint, title, culprit, level, environment string, seenAt time.Time) (issue.UpsertResult, error)
	Get(ctx context.Context, issueID int64) (issue.Issue, error)
}

// eventSink — приёмник событий для записи в CH; *event.Batcher ему
// удовлетворяет. Отдельный интерфейс (а не прямая зависимость от
// *event.Batcher) держит событийный путь Pipeline тестируемым без ClickHouse.
type eventSink interface {
	Add(event.Event)
}

// Pipeline — асинхронная обработка принятых событий:
// fingerprint → upsert issue (PG) → буфер батчера (CH). Транзакции идут через
// ту же очередь вторым типом задачи: у них нет ни fingerprint'а, ни issue —
// запись в SpanSink (CH) и детекция проблем производительности (PG + outbox).
type Pipeline struct {
	issues  issueUpserter
	batcher eventSink
	queue   chan task
	workers int
	wg      sync.WaitGroup

	// queueBytes/maxQueueBytes — байтовый потолок очереди в дополнение к
	// счётному (её ёмкости).
	//
	// Счётный потолок сам по себе ничего не гарантирует: событие несёт до
	// четырёх сырых JSON-блоков по 256 КиБ каждый (contexts, breadcrumbs,
	// request, stacktrace), то есть до мегабайта на задачу, а очередь держит
	// тысячу задач. Гигабайт резидентной памяти, и это на пути приёма, куда
	// пишет кто угодно с публичным ключом. Все пять писателей получили
	// байтовый бюджет ровно по этой причине; очередь была единственным
	// буфером без него.
	queueBytes    atomic.Int64
	maxQueueBytes atomic.Int64

	// Alerts — опциональный колбэк для new_issue/regression (план 6).
	// nil (значение по умолчанию) означает, что алертинг выключен —
	// process() просто пропускает вызов.
	Alerts AlertSink

	// Spans — приёмник транзакций; nil означает, что трейсинг выключен и
	// Handler не принимает transaction-item'ы (см. TracingEnabled).
	Spans SpanSink

	// Perf — запись находок детекторов в perf_issues; nil выключает детекцию.
	Perf PerfSink

	// PerfAlerts — алерт при первом обнаружении проблемы; nil выключает алерты
	// (детекция при этом продолжает работать).
	PerfAlerts PerfNotifier

	// Projects — источник настроек проекта, из которого детекция берёт пороги
	// (projects.perf_detector_config); nil означает «на дефолтах».
	Projects ProjectSettings

	// Scrub — зачистка ПДн перед записью событий и транзакций (152-ФЗ). nil
	// означает «scrubbing выключен» — все методы Scrubber nil-safe, поэтому
	// вызовы на p.Scrub делаются напрямую без проверки на nil.
	Scrub *Scrubber

	// testPerfBudget подменяет perfDetectBudget в тестах; 0 — обычный бюджет.
	testPerfBudget time.Duration

	closeMu sync.RWMutex
	closed  bool

	// dropped — потери по причинам. ПРОЦЕСС-ЛОКАЛЬНЫЙ счётчик для
	// самотелеметрии (gotcha_pipeline_dropped_tasks_total): живёт, пока жив
	// процесс, и ничего не знает про организацию задачи. Он и раньше
	// существовал, но org_usage.dropped_* эти потери не видел вовсе — оператор
	// не мог узнать, ЧЬИ события не доехали и сколько. Per-org учёт — отдельный
	// путь, см. DropCounter/dropAgg ниже.
	dropped map[DropReason]*atomic.Int64

	// DropCounter — учёт дропов ПАЙПЛАЙНА (queue_full/queue_bytes/
	// storage_error/panic/closed) per-org в org_usage.dropped_*; *org.Service
	// ему удовлетворяет. nil (дефолт) — как раньше: только process-local
	// dropped выше, без записи в БД.
	//
	// Это НЕ дублирует Handler.DropCounter: тот считает квотные отказы
	// (envelope/OTLP-путь, ДО постановки в очередь), этот — потери самой
	// очереди и обработки (ПОСЛЕ того, как квота уже списана). Точки жизненного
	// цикла не пересекаются.
	//
	// Дропы идут ШТОРМОМ при перегрузке — синхронный UPSERT в PostgreSQL на
	// каждый добил бы базу, которая и так деградирует (тот же org_usage, что
	// под исключительной блокировкой строки списывает квоту, см.
	// org.Service.checkAndCount). Поэтому Pipeline копит потери per-(org,kind)
	// в памяти (dropAgg) и сливает пачкой по тику (см. runDropFlush) и на
	// Close — а не пишет в БД на каждый дроп.
	DropCounter DropCounter

	dropAggMu sync.Mutex
	// dropAgg — накопленные с прошлого флаша дропы по (orgID, kind).
	dropAgg map[dropAggKey]int64

	// dropFlushStop/dropFlushDone — управление фоновым флашем dropAgg; nil, пока
	// Start() не запустила runDropFlush (запускает, только если DropCounter
	// задан — иначе копить нечего и некуда сливать).
	dropFlushStop chan struct{}
	dropFlushDone chan struct{}
}

// dropAggKey — ключ агрегата дропов пайплайна: организация + класс задачи.
// kind — dropKind из handler.go (dropEvent/dropTransaction); Pipeline не
// видит дропы метрик/профилей — они идут мимо очереди (см. handler.go).
type dropAggKey struct {
	orgID int64
	kind  dropKind
}

// DropReason — почему задача потеряна. Причина отделена от факта потери
// намеренно: рост потерь от переполнения очереди лечится размером очереди и
// числом воркеров, а рост от отказа хранилища — не лечится ничем из этого.
// Общий счётчик заставлял оператора гадать, какую из двух проблем он видит.
type DropReason string

const (
	// DropQueueFull — очередь заполнена: обработка не успевает за приёмом.
	DropQueueFull DropReason = "queue_full"
	// DropQueueBytes — исчерпан байтовый бюджет очереди: задачи крупнее
	// обычного (большие стектрейсы, много атрибутов).
	DropQueueBytes DropReason = "queue_bytes"
	// DropStorageError — не удалось записать в хранилище. Обычно деградация
	// PostgreSQL: апсерт issue отвалился по таймауту.
	DropStorageError DropReason = "storage_error"
	// DropPanic — обработчик упал на конкретном элементе.
	DropPanic DropReason = "panic"
	// DropClosed — приём уже остановлен, а задача пришла из in-flight
	// HTTP-запроса.
	DropClosed DropReason = "closed"
)

// dropReasons — полный набор причин. Существует, чтобы счётчики создавались
// один раз при инициализации: тогда countDropped на горячем пути обходится
// атомарным инкрементом без блокировки и без записи в map.
var dropReasons = []DropReason{
	DropQueueFull, DropQueueBytes, DropStorageError, DropPanic, DropClosed,
}

func newDropCounters() map[DropReason]*atomic.Int64 {
	m := make(map[DropReason]*atomic.Int64, len(dropReasons))
	for _, r := range dropReasons {
		m[r] = new(atomic.Int64)
	}
	return m
}

// countDropped увеличивает счётчик потерянных задач по причине.
func (p *Pipeline) countDropped(reason DropReason) {
	if c, ok := p.dropped[reason]; ok {
		c.Add(1)
	}
}

// taskDropKind — класс дропа по типу задачи для per-org агрегации: событие
// или транзакция. Pipeline не видит других классов (метрики/профили идут
// мимо очереди, см. handler.go).
func taskDropKind(t task) dropKind {
	if t.tx != nil {
		return dropTransaction
	}
	return dropEvent
}

// countDroppedOrg добавляет n к накопленному дропу (orgID, kind) между
// флашами (см. Pipeline.dropAgg). orgID<=0 (задача не провела orgID) или
// DropCounter==nil — no-op: атрибутировать некуда или некому.
func (p *Pipeline) countDroppedOrg(orgID int64, kind dropKind, n int64) {
	if p.DropCounter == nil || orgID <= 0 || n <= 0 {
		return
	}
	p.dropAggMu.Lock()
	p.dropAgg[dropAggKey{orgID: orgID, kind: kind}] += n
	p.dropAggMu.Unlock()
}

// CountDroppedEvents и CountDroppedTransactions — стоки per-org дропов БУФЕРА
// ПИСАТЕЛЯ (event.Batcher / trace.SpanWriter): их переполнение выбрасывает самое
// старое, и без атрибуции потеря невидима per-org — тот же класс, что дропы
// очереди (arch P1-1), но другой слой. main ставит их через SetDropSink; дропы
// стекаются в тот же dropAgg и тот же 60с-флаш в org_usage.dropped_*, что и
// дропы очереди — единый путь до БД. n<=0 или orgID<=0 — no-op (см. countDroppedOrg).
func (p *Pipeline) CountDroppedEvents(orgID, n int64) {
	p.countDroppedOrg(orgID, dropEvent, n)
}

// CountDroppedTransactions — см. CountDroppedEvents; для дропов txBuf SpanWriter.
func (p *Pipeline) CountDroppedTransactions(orgID, n int64) {
	p.countDroppedOrg(orgID, dropTransaction, n)
}

// drainDropAgg забирает накопленное и обнуляет агрегат под тем же мьютексом,
// что и countDroppedOrg — окно между флашами не теряет и не задваивает дропы.
func (p *Pipeline) drainDropAgg() map[dropAggKey]int64 {
	p.dropAggMu.Lock()
	defer p.dropAggMu.Unlock()
	if len(p.dropAgg) == 0 {
		return nil
	}
	out := p.dropAgg
	p.dropAgg = make(map[dropAggKey]int64, len(out))
	return out
}

// dropFlushInterval — как часто пайплайн сливает накопленные per-org дропы в
// org_usage. Крупнее, чем интервал Batcher/SpanWriter (5с, см.
// event/batcher.go): тем важна свежесть данных в UI, а отчёту о потерях
// секундная точность не нужна — важно лишь не потерять его насовсем. И
// крупнее, чем нужно было бы для UPSERT под низкой нагрузкой — специально:
// именно при шторме дропов (перегрузка) частый флаш добивал бы БД, которая и
// так деградирует, — см. докблок Pipeline.DropCounter.
const dropFlushInterval = 20 * time.Second

// dropFlushTimeout — бюджет ОДНОЙ попытки флаша, отдельный от parent ctx: как
// у Batcher.flushWithTimeout, тикер всегда зовёт с context.Background(), а
// Close передаёт свой ctx — в обоих случаях один медленный UPSERT не должен
// зависать дольше разумного.
const dropFlushTimeout = 5 * time.Second

// runDropFlush — цикл периодического флаша; запускается горутиной из Start(),
// только если DropCounter задан. Завершается через Close.
func (p *Pipeline) runDropFlush() {
	defer close(p.dropFlushDone)
	ticker := time.NewTicker(dropFlushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-p.dropFlushStop:
			return
		case <-ticker.C:
			p.flushDropped(context.Background())
		}
	}
}

// flushDropped сливает накопленные per-org дропы в org_usage.dropped_* через
// тот же DropCounter-интерфейс, что и Handler (см. handler.go countDrop) —
// одна реализация (*org.Service), разные вызывающие и разный темп вызовов.
// Best-effort, как и handler.countDrop: ошибка флаша логируется и не
// ретраится — drainDropAgg уже забрал накопленное, поэтому неудачный флаш
// теряет ровно это окно, а не блокирует приём или следующий флаш.
func (p *Pipeline) flushDropped(parent context.Context) {
	if p.DropCounter == nil {
		return
	}
	agg := p.drainDropAgg()
	if len(agg) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(parent, dropFlushTimeout)
	defer cancel()
	month := time.Now().UTC()
	for key, n := range agg {
		var err error
		switch key.kind {
		case dropEvent:
			err = p.DropCounter.IncDroppedEvents(ctx, key.orgID, month, n)
		case dropTransaction:
			err = p.DropCounter.IncDroppedTransactions(ctx, key.orgID, month, n)
		}
		if err != nil {
			slog.Warn("ingest: pipeline drop flush failed, this window's count lost",
				"org_id", key.orgID, "kind", key.kind, "n", n, "error", err)
		}
	}
}

// Dropped — сколько задач потеряно за время жизни процесса, всего.
func (p *Pipeline) Dropped() int64 {
	var total int64
	for _, c := range p.dropped {
		total += c.Load()
	}
	return total
}

// DroppedBy — сколько задач потеряно по конкретной причине. Для
// самотелеметрии: метка reason у gotcha_pipeline_dropped_tasks_total.
func (p *Pipeline) DroppedBy(reason DropReason) int64 {
	if c, ok := p.dropped[reason]; ok {
		return c.Load()
	}
	return 0
}

// DropReasons — все причины, по которым продукт умеет терять задачи. main
// регистрирует по метрике на причину.
func DropReasons() []DropReason { return append([]DropReason(nil), dropReasons...) }

// Queued — сколько задач ждёт обработки прямо сейчас.
func (p *Pipeline) Queued() int64 { return int64(len(p.queue)) }

// QueueCap — вместимость очереди (знаменатель для глубины).
func (p *Pipeline) QueueCap() int64 { return int64(cap(p.queue)) }

// task — единица работы воркера: ЛИБО событие (ev), ЛИБО транзакция (tx).
type task struct {
	projectID int64
	// orgID — организация задачи, для per-org учёта дропов (см.
	// countDroppedOrg). Заполняется в Enqueue/EnqueueTransaction из аргумента
	// вызывающего (handler знает key.OrgID из уже пройденной аутентификации).
	orgID int64
	ev    *ParsedEvent
	tx    *trace.Transaction
	// bytes — вес задачи, посчитанный при постановке. Хранится в самой
	// задаче, чтобы возврат бюджета не зависел от того, что с полями сделала
	// обработка (скрубер, например, укорачивает строки).
	bytes int64
}

func NewPipeline(issues *issue.Service, batcher *event.Batcher) *Pipeline {
	return &Pipeline{
		issues:  issues,
		batcher: batcher,
		queue:   make(chan task, 1000),
		workers: 4,
		dropped: newDropCounters(),
		dropAgg: make(map[dropAggKey]int64),
	}
}

// defaultMaxQueueBytes — байтовый потолок очереди по умолчанию.
//
// 64 МиБ подобраны так, чтобы для нормального трафика первым срабатывал
// счётный лимит (событие в среднем единицы килобайт — тысяча таких далеко не
// добирает до потолка), а байтовый ловил патологию: поток событий, набитых
// предельными JSON-блоками.
const defaultMaxQueueBytes = 64 << 20

// SetMaxQueueBytes задаёт байтовый потолок очереди; 0 или меньше —
// defaultMaxQueueBytes.
func (p *Pipeline) SetMaxQueueBytes(n int64) {
	if n <= 0 {
		n = defaultMaxQueueBytes
	}
	p.maxQueueBytes.Store(n)
}

// queueLimit — действующий потолок; нулевое значение поля означает, что
// SetMaxQueueBytes не звали, и берётся дефолт. Так Pipeline остаётся
// собираемым литералом, как и раньше.
func (p *Pipeline) queueLimit() int64 {
	if n := p.maxQueueBytes.Load(); n > 0 {
		return n
	}
	return defaultMaxQueueBytes
}

// taskBytes — вес задачи в очереди. Считает только крупные поля (сырые
// JSON-блоки и текст) плюс постоянную цену задачи: как и rowOverheadBytes у
// батчера, она не даёт обойти учёт потоком пустых событий.
func taskBytes(t task) int64 {
	const taskOverheadBytes = 256
	n := 0
	if ev := t.ev; ev != nil {
		n += len(ev.ContextsJSON) + len(ev.BreadcrumbsJSON) + len(ev.RequestJSON) +
			len(ev.StacktraceJSON) + len(ev.Message) + len(ev.Culprit) + len(ev.Title)
		for _, exc := range ev.Exceptions {
			n += len(exc.Type) + len(exc.Value)
			for _, fr := range exc.Frames {
				n += len(fr.Function) + len(fr.Module)
			}
		}
		for k, v := range ev.Tags {
			n += len(k) + len(v)
		}
	}
	if tx := t.tx; tx != nil {
		n += len(tx.Name) + len(tx.TraceID) + len(tx.Environment)
		for k, v := range tx.Tags {
			n += len(k) + len(v)
		}
		for _, sp := range tx.Spans {
			n += len(sp.Description) + len(sp.Op) + len(sp.SpanID) + len(sp.Status)
			n += dataMapBytes(sp.Data)
		}
	}
	return int64(n) + taskOverheadBytes
}

// dataMapBytes оценивает вес span.Data (map[string]any, «сырой JSON» из SDK) —
// то самое поле, ради которого байтовый бюджет очереди и заведён (см.
// taskBytes), но которое таскBytes раньше не считал вовсе. capDataMap
// (transaction.go/otlp.go) ограничивает Data сверху 64 ключами по 64 руны и
// строковые значения — 2000 рунами (maxDataValue), но НЕ трогает размер
// нестроковых значений: вложенные map/slice из JSON-парсинга остаются как
// есть, поэтому вес считается рекурсивно, а не константой на ключ.
func dataMapBytes(m map[string]any) int {
	n := 0
	for k, v := range m {
		n += len(k) + dataValueBytes(v)
	}
	return n
}

// dataValueBytes — вес одного значения span.Data. Конкретные типы после
// encoding/json.Unmarshal в map[string]any: string, float64, bool, nil,
// map[string]interface{}, []interface{} — остальные (не встречаются на этом
// пути) оцениваются как 8 байт, по аналогии со стоимостью measurement'а в
// txRowBytes.
func dataValueBytes(v any) int {
	switch val := v.(type) {
	case string:
		return len(val)
	case map[string]interface{}:
		n := 0
		for k, vv := range val {
			n += len(k) + dataValueBytes(vv)
		}
		return n
	case []interface{}:
		n := 0
		for _, vv := range val {
			n += dataValueBytes(vv)
		}
		return n
	default:
		return 8
	}
}

// admit резервирует место под задачу. false — очередь уже держит столько,
// сколько позволено: вызывающий дропает задачу, как и при переполнении по
// счёту.
func (p *Pipeline) admit(size int64) bool {
	limit := p.queueLimit()
	for {
		cur := p.queueBytes.Load()
		if cur+size > limit {
			return false
		}
		if p.queueBytes.CompareAndSwap(cur, cur+size) {
			return true
		}
	}
}

// QueuedBytes — сколько байтов сейчас держат задачи в очереди. Для
// самотелеметрии: без неё исчерпание байтового бюджета видно только по
// счётчику дропов, а он не отличает переполнение по объёму от переполнения по
// количеству.
func (p *Pipeline) QueuedBytes() int64 { return p.queueBytes.Load() }

// release возвращает бюджет после обработки задачи.
func (p *Pipeline) release(size int64) { p.queueBytes.Add(-size) }

func (p *Pipeline) Start() {
	for i := 0; i < p.workers; i++ {
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			for t := range p.queue {
				p.processGuarded(t)
			}
		}()
	}
	// Флаш дропов запускается, только если есть DropCounter — иначе копить
	// dropAgg некуда сливать, и тикер впустую просыпался бы всю жизнь процесса.
	if p.DropCounter != nil {
		p.dropFlushStop = make(chan struct{})
		p.dropFlushDone = make(chan struct{})
		go p.runDropFlush()
	}
}

// processGuarded обрабатывает одну задачу под recover(). Паника в разборе
// одного события/транзакции (битый payload, nil-разыменование в детекторе и
// т.п.) обязана терять РОВНО это событие, а не убивать воркер и через него весь
// процесс приёма. Точно как recover вокруг detectPerfIssues, но на ОСНОВНОМ
// пути: без него паника на горячем пути роняла бы go-процесс целиком.
func (p *Pipeline) processGuarded(t task) {
	// Бюджет возвращается и при панике: иначе очередь, пережившая несколько
	// битых событий, навсегда считала бы себя заполненной.
	defer p.release(t.bytes)
	defer func() {
		if r := recover(); r != nil {
			var eventID, traceID string
			if t.ev != nil {
				eventID = t.ev.EventID
			}
			if t.tx != nil {
				traceID = t.tx.TraceID
			}
			p.countDropped(DropPanic)
			p.countDroppedOrg(t.orgID, taskDropKind(t), 1)
			slog.Error("ingest task panicked, item dropped",
				"project_id", t.projectID, "event_id", eventID,
				"trace_id", traceID, "panic", r)
		}
	}()
	p.process(t)
}

// Enqueue не блокирует: при полной очереди событие дропается с warn-логом —
// приём ошибок не должен вставать из-за медленной обработки. После Close
// событие тоже дропается — send в закрытый канал иначе паникует, если
// in-flight HTTP-хендлер зовёт Enqueue параллельно с drain'ом.
//
// orgID — организация задачи (handler знает key.OrgID из аутентификации,
// сделанной выше по стеку); нужен только для per-org учёта дропов (см.
// countDroppedOrg) — на сам приём не влияет. 0 у вызывающих, которым
// атрибутировать некуда (в проде такого не бывает).
func (p *Pipeline) Enqueue(projectID, orgID int64, ev *ParsedEvent) {
	p.closeMu.RLock()
	defer p.closeMu.RUnlock()
	if p.closed {
		p.countDropped(DropClosed)
		p.countDroppedOrg(orgID, dropEvent, 1)
		slog.Warn("ingest pipeline closed, dropping event",
			"project_id", projectID, "event_id", ev.EventID)
		return
	}
	t := task{projectID: projectID, orgID: orgID, ev: ev}
	t.bytes = taskBytes(t)
	if !p.admit(t.bytes) {
		p.countDropped(DropQueueBytes)
		p.countDroppedOrg(orgID, dropEvent, 1)
		slog.Warn("ingest queue byte budget exhausted, dropping event",
			"project_id", projectID, "event_id", ev.EventID, "task_bytes", t.bytes)
		return
	}
	select {
	case p.queue <- t:
	default:
		p.release(t.bytes)
		p.countDropped(DropQueueFull)
		p.countDroppedOrg(orgID, dropEvent, 1)
		slog.Warn("ingest queue full, dropping event",
			"project_id", projectID, "event_id", ev.EventID)
	}
}

// TracingEnabled сообщает, есть ли куда писать транзакции. Handler смотрит на
// это до квоты: не тратить бюджет транзакций организации, если писать их
// всё равно некуда.
func (p *Pipeline) TracingEnabled() bool {
	return p.Spans != nil
}

// EnqueueTransaction — как Enqueue, но для транзакции: не блокирует, дропает
// с warn-логом при полной очереди или после Close. orgID — см. докблок Enqueue.
func (p *Pipeline) EnqueueTransaction(projectID, orgID int64, tx trace.Transaction) {
	p.closeMu.RLock()
	defer p.closeMu.RUnlock()
	if p.closed {
		p.countDropped(DropClosed)
		p.countDroppedOrg(orgID, dropTransaction, 1)
		slog.Warn("ingest pipeline closed, dropping transaction",
			"project_id", projectID, "trace_id", tx.TraceID)
		return
	}
	t := task{projectID: projectID, orgID: orgID, tx: &tx}
	t.bytes = taskBytes(t)
	if !p.admit(t.bytes) {
		p.countDropped(DropQueueBytes)
		p.countDroppedOrg(orgID, dropTransaction, 1)
		slog.Warn("ingest queue byte budget exhausted, dropping transaction",
			"project_id", projectID, "trace_id", tx.TraceID, "task_bytes", t.bytes)
		return
	}
	select {
	case p.queue <- t:
	default:
		p.release(t.bytes)
		p.countDropped(DropQueueFull)
		p.countDroppedOrg(orgID, dropTransaction, 1)
		slog.Warn("ingest queue full, dropping transaction",
			"project_id", projectID, "trace_id", tx.TraceID)
	}
}

// Close перестаёт принимать и дожидается обработки очереди, но не дольше, чем
// позволяет ctx. Идемпотентен. Возвращает ctx.Err(), если бюджет исчерпан раньше,
// чем воркеры разобрали очередь.
//
// Дедлайн обязателен: каждая задача — upsert в PostgreSQL с таймаутом 5с, очередь
// 1000 задач, воркеров 4. При деградации PG безлимитное ожидание держало бы
// shutdown до ~20 минут, за которые остальные писатели даже не начали бы дренаж —
// а внешний stop_grace_period всё равно убьёт процесс раньше, и тогда теряется
// содержимое ВСЕХ буферов, а не только этой очереди.
func (p *Pipeline) Close(ctx context.Context) error {
	p.closeMu.Lock()
	if p.closed {
		p.closeMu.Unlock()
		return nil
	}
	p.closed = true
	close(p.queue)
	p.closeMu.Unlock()

	done := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(done)
	}()
	var drainErr error
	select {
	case <-done:
	case <-ctx.Done():
		slog.Warn("ingest pipeline drain timed out, remaining queue dropped")
		drainErr = ctx.Err()
	}

	// Финальный слив ПОСЛЕ дренажа очереди: воркеры дописали в dropAgg свои
	// последние storage_error/panic-дропы, и это единственный шанс дать их
	// увидеть org_usage — dropAgg живёт только в памяти процесса, который
	// сейчас останавливается.
	//
	// СВОЙ context.Background(), а не ctx: на пути таймаута дренажа ctx уже
	// Done(), и flushDropped (WithTimeout от НЕГО) отвалился бы немедленно —
	// смысл финального флаша ровно в том, чтобы не потерять последнее окно, а
	// унаследованный истёкший ctx его как раз терял. dropFlushTimeout внутри
	// flushDropped и так ограничивает попытку, так что Close на happy-path не
	// удлиняется дольше него.
	if p.dropFlushStop != nil {
		close(p.dropFlushStop)
		<-p.dropFlushDone
	}
	p.flushDropped(context.Background())
	return drainErr
}

func (p *Pipeline) process(t task) {
	if t.tx != nil {
		p.processTransaction(t.orgID, t.projectID, *t.tx)
		return
	}
	ev := t.ev
	fp := fingerprint.Compute(fingerprint.Input{
		Custom:     ev.Fingerprint,
		Exceptions: ev.Exceptions,
		Message:    ev.Message,
	})

	// RA-L10 (проход 4): маскируем email в свободном тексте title ДО первого его
	// использования — Upsert (issues.title в PG) и OnIssue (payload алерта).
	// Раньше скраб title/message ехал только перед записью в CH, и email утекал в
	// PG и в алерт открытым. fingerprint уже посчитан выше на ИСХОДНОМ тексте
	// (Message/Exceptions, не Title), поэтому scrubbing здесь не трогает
	// группировку. No-op при ScrubFreeText=false.
	ev.Title = p.Scrub.ScrubMessage(ev.Title)

	// arch P2-1 (2026-08-12): Upsert (PG times_seen++) идёт ДО batcher.Add (CH,
	// ниже) по необходимости — res.IssueID из Upsert нужен для самой CH-строки
	// события (event.Event.IssueID), так что порядок иначе не построить без
	// двухфазной записи. Следствие: если CH-батч потом дропнется (переполнение
	// батчера/деградация CH), issues.times_seen в PG окажется больше числа
	// реально доехавших до CH событий issue — инвариант «times_seen == count(*)
	// событий issue в CH» не держится в общем случае. Осознанный компромисс:
	// телеметрия дропа событий (DropStorageError/countDropped) best-effort и не
	// продана как точная бухгалтерия; дрейф ограничен размером окна деградации
	// CH, а откат times_seen на дропе CH-батча потребовал бы либо синхронной
	// записи в CH перед Upsert (теряет типобезопасность IssueID), либо
	// компенсирующей транзакции PG на асинхронный дроп батча — оба дороже, чем
	// стоит эта метрика. Не путать с DropStorageError выше — тот считает
	// дропнутые ДО Upsert события и НЕ покрывает этот путь.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := p.issues.Upsert(ctx,
		t.projectID, fp, ev.Title, ev.Culprit, ev.Level, ev.Environment, ev.Timestamp)
	if err != nil {
		// Потеря по отказу хранилища. Считается наравне с переполнением
		// очереди: событие не доехало, и молчащий счётчик отправлял оператора
		// проверять SDK вместо базы.
		p.countDropped(DropStorageError)
		p.countDroppedOrg(t.orgID, dropEvent, 1) // process() обрабатывает только события (tx уходит через processTransaction)
		slog.Error("issue upsert failed, event dropped",
			"project_id", t.projectID, "event_id", ev.EventID, "error", err)
		return
	}
	if (res.New || res.Regression) && p.Alerts != nil {
		kind := alert.KindNewIssue
		if res.Regression {
			kind = alert.KindRegression
		}
		// times_seen требует отдельного чтения: Upsert его не возвращает.
		// New/Regression — редкие переходы состояния (не каждое событие),
		// так что лишний round-trip к PG здесь не на горячем пути приёма.
		timesSeen := int64(1)
		if iss, err := p.issues.Get(ctx, res.IssueID); err != nil {
			slog.Error("issue lookup for alert failed", "issue_id", res.IssueID, "error", err)
		} else {
			timesSeen = iss.TimesSeen
		}
		p.Alerts.OnIssue(ctx, alert.Event{
			ProjectID: t.projectID,
			IssueID:   res.IssueID,
			Kind:      kind,
			Title:     ev.Title,
			Culprit:   ev.Culprit,
			Level:     ev.Level,
			TimesSeen: timesSeen,
		})
	}

	var excType, excValue string
	if n := len(ev.Exceptions); n > 0 {
		excType, excValue = ev.Exceptions[n-1].Type, ev.Exceptions[n-1].Value
	}

	// Зачистка ПДн перед записью: обнуляем ip/email по флагам и редактим
	// denylist-поля в tags/contexts/stacktrace. ScrubJSON вдобавок прогоняет
	// текстовые ЗНАЧЕНИЯ (кадры стектрейса, поля contexts) через free-text
	// маскирование email (RA-L10) — no-op при ScrubFreeText=false. p.Scrub == nil —
	// no-op (методы Scrubber nil-safe).
	p.Scrub.ScrubUser(&ev.UserIP, &ev.UserEmail)
	p.Scrub.ScrubTags(ev.Tags)
	ev.ContextsJSON = p.Scrub.ScrubJSON(ev.ContextsJSON)
	ev.StacktraceJSON = p.Scrub.ScrubJSON(ev.StacktraceJSON)
	ev.BreadcrumbsJSON = p.Scrub.ScrubJSON(ev.BreadcrumbsJSON)
	// Тело/заголовки/куки запроса часто несут PII и секреты (Authorization,
	// session-cookie, пароли в form-data) — прогоняем через тот же denylist-скраб,
	// что и contexts, до записи в CH.
	ev.RequestJSON = p.Scrub.ScrubJSON(ev.RequestJSON)
	// RA-L10: опционально маскируем email в свободном тексте (message/exception
	// value). No-op при ScrubFreeText=false — текущее поведение не меняется.
	ev.Message = p.Scrub.ScrubMessage(ev.Message)
	excValue = p.Scrub.ScrubMessage(excValue)

	p.batcher.Add(event.Event{
		ID:             ev.EventID,
		OrgID:          t.orgID,
		ProjectID:      t.projectID,
		IssueID:        res.IssueID,
		Timestamp:      ev.Timestamp,
		Level:          ev.Level,
		Message:        ev.Message,
		ExceptionType:  excType,
		ExceptionValue: excValue,
		Stacktrace:     ev.StacktraceJSON,
		Environment:    ev.Environment,
		Release:        ev.Release,
		ServerName:     ev.ServerName,
		SDK:            ev.SDK,
		UserID:         ev.UserID,
		UserIP:         ev.UserIP,
		UserEmail:      ev.UserEmail,
		Tags:           ev.Tags,
		Contexts:       ev.ContextsJSON,
		Breadcrumbs:    ev.BreadcrumbsJSON,
		Request:        ev.RequestJSON,
		TraceID:        ev.TraceID,
		SpanID:         ev.SpanID,
	})
}

// processTransaction пишет транзакцию в SpanWriter и прогоняет по ней детекторы
// производительности. Порядок важен: Spans.Add идёт ПЕРВЫМ, и запись в CH не
// ждёт ни PG, ни outbox — трейс попадает в хранилище независимо от того, что
// случится в детекции.
func (p *Pipeline) processTransaction(orgID, projectID int64, tx trace.Transaction) {
	if p.Spans == nil { // трейсинг выключен — Handler сюда не должен доходить
		slog.Warn("tracing disabled, dropping transaction",
			"project_id", projectID, "trace_id", tx.TraceID)
		return
	}
	// Зачистка ПДн перед записью. Теги транзакции чистятся так же, как у
	// событий (см. process): denylist-тег на транзакции/OTLP-атрибуте иначе
	// уехал бы в CH сырым. Данные спанов — отдельно: заголовки/куки/токены
	// часто оседают в span.Data (напр. http.*). p.Scrub == nil — no-op
	// (методы Scrubber nil-safe). Детекция работает поверх уже зачищенных спанов.
	p.Scrub.ScrubTags(tx.Tags)
	// SEC-L2/M2: имя транзакции нередко URL-образное (GET /u?token=...&email=...);
	// ScrubMessage чистит и email во free-text (по флагу), и query-токены в
	// встроенном URL (всегда), иначе токены из query string оседают в CH-колонке
	// transactions.transaction даже при дефолтном scrubbing.
	tx.Name = p.Scrub.ScrubMessage(tx.Name)
	for i := range tx.Spans {
		p.Scrub.ScrubData(tx.Spans[i].Data)
		// span.description часто "METHOD https://host/path?token=…" — ScrubMessage
		// вычищает query-токены всегда, email — при ScrubFreeText.
		tx.Spans[i].Description = p.Scrub.ScrubMessage(tx.Spans[i].Description)
	}

	p.Spans.Add(orgID, projectID, tx)
	p.detectPerfIssues(projectID, tx)
}

// detectPerfIssues прогоняет детекторы по спанам транзакции, апсертит находки в
// perf_issues и алертит о тех, что увидены впервые или вернулись после resolve.
//
// Детекция не имеет права ронять приём: паника детектора или сбой PG здесь
// логируются и на этом заканчиваются — транзакция уже записана в CH (см.
// processTransaction), а воркер продолжает разбирать очередь.
func (p *Pipeline) detectPerfIssues(projectID int64, tx trace.Transaction) {
	if p.Perf == nil { // детекторы выключены
		return
	}
	defer func() {
		if r := recover(); r != nil {
			slog.Error("perf detection panicked, transaction still written",
				"project_id", projectID, "trace_id", tx.TraceID, "panic", r)
		}
	}()

	// ОДИН бюджет на всю детекцию: настройки, все Record и все алерты (см.
	// perfDetectBudget). Дольше него воркер этой транзакцией не занимается.
	ctx, cancel := context.WithTimeout(context.Background(), p.perfBudget())
	defer cancel()

	cfg := p.detectorConfig(ctx, projectID)

	findings := trace.Detect(tx, cfg)
	for i, f := range findings {
		if err := ctx.Err(); err != nil {
			slog.Warn("perf detection budget exhausted, remaining findings skipped",
				"project_id", projectID, "trace_id", tx.TraceID,
				"recorded", i, "skipped", len(findings)-i, "error", err)
			return
		}
		p.recordFinding(ctx, projectID, tx, f)
	}
}

// perfBudget — бюджет детекции; отдельный метод, чтобы тесты подменяли его через
// поле, не трогая глобальную переменную.
func (p *Pipeline) perfBudget() time.Duration {
	if p.testPerfBudget > 0 {
		return p.testPerfBudget
	}
	return perfDetectBudget
}

// recordFinding пишет одну находку и алертит о ней, если она новая или вернулась.
// ctx — общий бюджет детекции (см. detectPerfIssues), а не персональный.
func (p *Pipeline) recordFinding(ctx context.Context, projectID int64, tx trace.Transaction, f trace.Finding) {
	res, err := p.Perf.Record(ctx, projectID, f, tx.TraceID)
	if err != nil {
		slog.Error("perf issue record failed",
			"project_id", projectID, "trace_id", tx.TraceID, "kind", f.Kind, "error", err)
		return
	}
	// Алерт — при ПЕРВОМ обнаружении и при регрессии (проблему починили, и она
	// вернулась). На повторные обнаружения — молчим: проблема воспроизводится на
	// каждом запросе к эндпойнту, и алерт на каждое повторение был бы лавиной.
	if p.PerfAlerts == nil || (!res.Created && !res.Regression) {
		return
	}
	notify := p.PerfAlerts.NotifyNew
	if res.Regression {
		notify = p.PerfAlerts.NotifyRegression
	}
	if err := notify(ctx, projectID, res.Issue); err != nil {
		slog.Error("perf issue alert failed", "project_id", projectID,
			"perf_issue_id", res.Issue.ID, "regression", res.Regression, "error", err)
	}
}

// detectorConfig — пороги проекта (projects.perf_detector_config). Любая
// проблема с их чтением или разбором — не повод не детектить: возвращаются
// дефолты.
func (p *Pipeline) detectorConfig(ctx context.Context, projectID int64) trace.DetectorConfig {
	if p.Projects == nil {
		return trace.DefaultDetectorConfig()
	}
	proj, err := p.Projects.Resolve(ctx, projectID)
	if err != nil {
		slog.Error("perf detector config lookup failed, using defaults",
			"project_id", projectID, "error", err)
		return trace.DefaultDetectorConfig()
	}
	cfg, err := trace.ConfigFromJSON([]byte(proj.PerfDetectorConfig))
	if err != nil {
		slog.Error("perf detector config parse failed, using defaults",
			"project_id", projectID, "error", err)
	}
	return cfg
}
