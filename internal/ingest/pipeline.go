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
	"gitflic.ru/otezvikentiy/gotcha/internal/scrub"
	"gitflic.ru/otezvikentiy/gotcha/internal/trace"
)

// Бюджет общий на транзакцию, не на находку: иначе транзакция с maxFindingsPerTransaction
// находок при медленной PG держала бы воркера сотни секунд, деля очередь с приёмом ошибок.
const perfDetectBudget = 10 * time.Second

type AlertSink interface {
	OnIssue(ctx context.Context, ev alert.Event)
}

type SpanSink interface {
	Add(orgID, projectID int64, t trace.Transaction)
}

type PerfSink interface {
	Record(ctx context.Context, projectID int64, f trace.Finding, traceID string) (trace.RecordResult, error)
}

type PerfNotifier interface {
	NotifyNew(ctx context.Context, projectID int64, iss trace.PerfIssue) error
	NotifyRegression(ctx context.Context, projectID int64, iss trace.PerfIssue) error
}

// Не переиспользует trace/host.MaintenanceChecker: perf_issues — throttle-детектор без
// жизненного цикла инцидента, общий интерфейс добавил бы зависимость ради формы.
type MaintenanceChecker interface {
	InMaintenance(ctx context.Context, projectID int64, at time.Time) (bool, error)
}

type issueUpserter interface {
	Upsert(ctx context.Context, projectID int64, fingerprint, title, culprit, level, environment string, seenAt time.Time) (issue.UpsertResult, error)
	Get(ctx context.Context, issueID int64) (issue.Issue, error)
}

type eventSink interface {
	Add(event.Event)
}

type Pipeline struct {
	issues  issueUpserter
	batcher eventSink
	queue   chan task
	workers int
	wg      sync.WaitGroup

	// Событие несёт до 4 сырых JSON-блоков по 256 КиБ — без байтового бюджета
	// очередь в 1000 задач держала бы гигабайт.
	queueBytes    atomic.Int64
	maxQueueBytes atomic.Int64

	// nil — алертинг выключен, process() пропускает вызов.
	Alerts AlertSink

	// nil — трейсинг выключен, Handler не принимает transaction-item'ы.
	Spans SpanSink

	// nil выключает детекцию находок в perf_issues.
	Perf PerfSink

	// nil выключает алерт при первом обнаружении, детекция продолжает работать.
	PerfAlerts PerfNotifier

	// Подавляет только notify в recordFinding; Record (сбор данных) работает как
	// обычно. nil — окна не подавляют алерты.
	Maint MaintenanceChecker

	// Источник порогов детекции (perf_detector_config); nil — дефолтные пороги.
	Projects ProjectSettings

	// nil — scrubbing выключен, методы Scrubber nil-safe (вызываются без проверки).
	Scrub *scrub.Scrubber

	testPerfBudget time.Duration

	testBackpressureBudget time.Duration
	testBackpressurePoll   time.Duration

	closeMu sync.RWMutex
	closed  bool

	// atomic.Bool, не канал: Pipeline{} из тестового литерала не проходит
	// NewPipeline, канал остался бы nil — select на нём не сработал бы.
	stopping atomic.Bool

	// Самотелеметрия ожидания перед записью в насыщенный буфер — отличает
	// воркеров в ожидании от медленно работающих (gotcha_pipeline_backpressure_waits_total).
	backpressureWaits     atomic.Int64
	backpressureWaitNanos atomic.Int64

	dropped map[DropReason]*atomic.Int64

	// Не дублирует Handler.DropCounter: тот — квотные отказы до очереди, этот —
	// потери после списания квоты. Копится в памяти и сливается пачкой.
	DropCounter DropCounter

	dropAggMu sync.Mutex
	dropAgg   map[dropAggKey]int64

	dropFlushStop chan struct{}
	dropFlushDone chan struct{}
}

type dropAggKey struct {
	orgID int64
	kind  dropKind
	month time.Time
}

// Усечение до начала месяца по UTC — иначе каждый дроп получил бы уникальный
// ключ агрегации по наносекундам, и счётчики никогда бы не схлопывались.
func dropMonthKey(t time.Time) time.Time {
	y, m, _ := t.UTC().Date()
	return time.Date(y, m, 1, 0, 0, 0, 0, time.UTC)
}

// Причина отделена от факта потери: переполнение очереди лечится размером/числом
// воркеров, отказ хранилища — не лечится ничем из этого.
type DropReason string

const (
	// Очередь заполнена: обработка не успевает за приёмом.
	DropQueueFull DropReason = "queue_full"
	// Исчерпан байтовый бюджет очереди: задачи крупнее обычного.
	DropQueueBytes DropReason = "queue_bytes"
	// Не удалось записать в хранилище — обычно деградация PostgreSQL.
	DropStorageError DropReason = "storage_error"
	DropPanic        DropReason = "panic"
	// Приём уже остановлен, а задача пришла из in-flight запроса.
	DropClosed DropReason = "closed"
)

// Создаются один раз при инициализации, чтобы countDropped на горячем пути
// обходился атомарным инкрементом без записи в map.
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

func (p *Pipeline) countDropped(reason DropReason) {
	if c, ok := p.dropped[reason]; ok {
		c.Add(1)
	}
}

func taskDropKind(t task) dropKind {
	if t.tx != nil {
		return dropTransaction
	}
	return dropEvent
}

func (p *Pipeline) countDroppedOrg(orgID int64, kind dropKind, n int64) {
	if p.DropCounter == nil || orgID <= 0 || n <= 0 {
		return
	}
	key := dropAggKey{orgID: orgID, kind: kind, month: dropMonthKey(time.Now())}
	p.dropAggMu.Lock()
	p.dropAgg[key] += n
	p.dropAggMu.Unlock()
}

func (p *Pipeline) CountDroppedEvents(orgID, n int64) {
	p.countDroppedOrg(orgID, dropEvent, n)
}

func (p *Pipeline) CountDroppedTransactions(orgID, n int64) {
	p.countDroppedOrg(orgID, dropTransaction, n)
}

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

const dropFlushInterval = 20 * time.Second

const dropFlushTimeout = 5 * time.Second

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

// best-effort: ошибка логируется и не ретраится — drainDropAgg уже забрал накопленное.
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
	for key, n := range agg {
		var err error
		switch key.kind {
		case dropEvent:
			err = p.DropCounter.IncDroppedEvents(ctx, key.orgID, key.month, n)
		case dropTransaction:
			err = p.DropCounter.IncDroppedTransactions(ctx, key.orgID, key.month, n)
		}
		if err != nil {
			slog.Warn("ingest: pipeline drop flush failed, this window's count lost",
				"org_id", key.orgID, "kind", key.kind, "n", n, "error", err)
		}
	}
}

func (p *Pipeline) Dropped() int64 {
	var total int64
	for _, c := range p.dropped {
		total += c.Load()
	}
	return total
}

func (p *Pipeline) DroppedBy(reason DropReason) int64 {
	if c, ok := p.dropped[reason]; ok {
		return c.Load()
	}
	return 0
}

func DropReasons() []DropReason { return append([]DropReason(nil), dropReasons...) }

// Приём мог честно отвечать 503, хотя очередь пайплайна не полна: воркеры стоят
// перед батчером/SpanWriter, а не простаивают.
func (p *Pipeline) BackpressureWaits() int64 { return p.backpressureWaits.Load() }

func (p *Pipeline) BackpressureWaitSeconds() float64 {
	return time.Duration(p.backpressureWaitNanos.Load()).Seconds()
}

func (p *Pipeline) Queued() int64 { return int64(len(p.queue)) }

func (p *Pipeline) QueueCap() int64 { return int64(cap(p.queue)) }

type task struct {
	projectID int64
	orgID     int64
	ev        *ParsedEvent
	tx        *trace.Transaction
	// Вес задачи на момент постановки — не пересчитывается, чтобы возврат
	// бюджета не зависел от того, что обработка сделала с полями.
	bytes int64
}

// nil-указатель, присвоенный интерфейсному полю напрямую, дал бы typed-nil —
// saturationOf запаниковал бы в Saturation() при разыменовании.
func NewPipeline(issues *issue.Service, batcher *event.Batcher) *Pipeline {
	p := &Pipeline{
		issues:  issues,
		queue:   make(chan task, 1000),
		workers: 4,
		dropped: newDropCounters(),
		dropAgg: make(map[dropAggKey]int64),
	}
	if batcher != nil {
		p.batcher = batcher
	}
	return p
}

const defaultMaxQueueBytes = 64 << 20

func (p *Pipeline) SetMaxQueueBytes(n int64) {
	if n <= 0 {
		n = defaultMaxQueueBytes
	}
	p.maxQueueBytes.Store(n)
}

func (p *Pipeline) queueLimit() int64 {
	if n := p.maxQueueBytes.Load(); n > 0 {
		return n
	}
	return defaultMaxQueueBytes
}

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

// capDataMap ограничивает число ключей и длину строк, но не размер вложенных
// map/slice, поэтому вес считается рекурсивно, а не константой на ключ.
func dataMapBytes(m map[string]any) int {
	n := 0
	for k, v := range m {
		n += len(k) + dataValueBytes(v)
	}
	return n
}

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

func (p *Pipeline) QueuedBytes() int64 { return p.queueBytes.Load() }

// Максимум по обоим потолкам — упереться достаточно в один. Не обрезается
// единицей: очередь может перебрать потолок между admit и постановкой.
func (p *Pipeline) QueueSaturation() float64 {
	rows := queueSaturation(int64(len(p.queue)), int64(cap(p.queue)))
	bytes := queueSaturation(p.QueuedBytes(), p.queueLimit())
	if bytes > rows {
		return bytes
	}
	return rows
}

// den<=0 — лимит выключен, а не «делить не на что»: не паникует и не показывает насыщение.
func queueSaturation(num, den int64) float64 {
	if den <= 0 {
		return 0
	}
	return float64(num) / float64(den)
}

// Максимум очереди пайплайна и буфера батчера (p.batcher) — обе стадии до записи
// в CH. p.batcher обязателен (не nil-safe, в отличие от Spans/Perf/Alerts).
func (p *Pipeline) EventSaturation() float64 {
	return max(p.QueueSaturation(), saturationOf(p.batcher))
}

// p.Spans == nil (трейсинг выключен) — saturationOf вернёт 0, верно: сигнал уже
// отвечает успехом без записи, overloaded preflight не должен его трогать.
func (p *Pipeline) TransactionSaturation() float64 {
	return max(p.QueueSaturation(), saturationOf(p.Spans))
}

func (p *Pipeline) release(size int64) { p.queueBytes.Add(-size) }

// Цель — не дождаться восстановления хранилища, а дать очереди перед воркерами
// время заполниться, чтобы Handler.overloaded успел честно ответить 503.
const backpressureWaitBudget = 2 * time.Second

const backpressurePollInterval = 50 * time.Millisecond

func (p *Pipeline) backpressureBudget() time.Duration {
	if p.testBackpressureBudget > 0 {
		return p.testBackpressureBudget
	}
	return backpressureWaitBudget
}

func (p *Pipeline) backpressurePoll() time.Duration {
	if p.testBackpressurePoll > 0 {
		return p.testBackpressurePoll
	}
	return backpressurePollInterval
}

// sink без Saturation() (тестовые двойники) — saturationOf вернёт 0, цикл не
// откроется вовсе.
func (p *Pipeline) waitForRoom(sink any) {
	if saturationOf(sink) < 1.0 {
		return
	}
	start := time.Now()
	p.backpressureWaits.Add(1)
	defer func() { p.backpressureWaitNanos.Add(int64(time.Since(start))) }()

	ticker := time.NewTicker(p.backpressurePoll())
	defer ticker.Stop()
	deadline := time.NewTimer(p.backpressureBudget())
	defer deadline.Stop()
	for {
		select {
		case <-deadline.C:
			return
		case <-ticker.C:
			if p.stopping.Load() || saturationOf(sink) < 1.0 {
				return
			}
		}
	}
}

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
	if p.DropCounter != nil {
		p.dropFlushStop = make(chan struct{})
		p.dropFlushDone = make(chan struct{})
		go p.runDropFlush()
	}
}

// Паника при разборе одного события/транзакции обязана терять только его, а
// не убивать воркер и весь процесс приёма.
func (p *Pipeline) processGuarded(t task) {
	// Возвращается и при панике — иначе очередь после нескольких битых событий
	// считала бы себя заполненной.
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

// false — дропнуто: вызывающий должен знать это из-за окна между
// preflight-проверкой и постановкой.
func (p *Pipeline) Enqueue(projectID, orgID int64, ev *ParsedEvent) bool {
	p.closeMu.RLock()
	defer p.closeMu.RUnlock()
	if p.closed {
		p.countDropped(DropClosed)
		p.countDroppedOrg(orgID, dropEvent, 1)
		slog.Warn("ingest pipeline closed, dropping event",
			"project_id", projectID, "event_id", ev.EventID)
		return false
	}
	t := task{projectID: projectID, orgID: orgID, ev: ev}
	t.bytes = taskBytes(t)
	if !p.admit(t.bytes) {
		p.countDropped(DropQueueBytes)
		p.countDroppedOrg(orgID, dropEvent, 1)
		slog.Warn("ingest queue byte budget exhausted, dropping event",
			"project_id", projectID, "event_id", ev.EventID, "task_bytes", t.bytes)
		return false
	}
	select {
	case p.queue <- t:
		return true
	default:
		p.release(t.bytes)
		p.countDropped(DropQueueFull)
		p.countDroppedOrg(orgID, dropEvent, 1)
		slog.Warn("ingest queue full, dropping event",
			"project_id", projectID, "event_id", ev.EventID)
		return false
	}
}

// Handler смотрит на это до квоты — не тратить бюджет, если писать некуда.
func (p *Pipeline) TracingEnabled() bool {
	return p.Spans != nil
}

func (p *Pipeline) EnqueueTransaction(projectID, orgID int64, tx trace.Transaction) bool {
	p.closeMu.RLock()
	defer p.closeMu.RUnlock()
	if p.closed {
		p.countDropped(DropClosed)
		p.countDroppedOrg(orgID, dropTransaction, 1)
		slog.Warn("ingest pipeline closed, dropping transaction",
			"project_id", projectID, "trace_id", tx.TraceID)
		return false
	}
	t := task{projectID: projectID, orgID: orgID, tx: &tx}
	t.bytes = taskBytes(t)
	if !p.admit(t.bytes) {
		p.countDropped(DropQueueBytes)
		p.countDroppedOrg(orgID, dropTransaction, 1)
		slog.Warn("ingest queue byte budget exhausted, dropping transaction",
			"project_id", projectID, "trace_id", tx.TraceID, "task_bytes", t.bytes)
		return false
	}
	select {
	case p.queue <- t:
		return true
	default:
		p.release(t.bytes)
		p.countDropped(DropQueueFull)
		p.countDroppedOrg(orgID, dropTransaction, 1)
		slog.Warn("ingest queue full, dropping transaction",
			"project_id", projectID, "trace_id", tx.TraceID)
		return false
	}
}

// Дедлайн обязателен: без него деградация PG держала бы shutdown ~20 минут — а
// внешний stop_grace_period всё равно убьёт процесс раньше, потеряв все буферы.
func (p *Pipeline) Close(ctx context.Context) error {
	p.closeMu.Lock()
	if p.closed {
		p.closeMu.Unlock()
		return nil
	}
	p.closed = true
	close(p.queue)
	// Обрывает ожидание в waitForRoom на ближайшем тике — иначе дренаж мог бы
	// стоять на каждой из тысячи задач по backpressureBudget().
	p.stopping.Store(true)
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

	// После дренажа: последние storage_error/panic-дропы попадают в org_usage.
	// Свой context.Background(), не ctx — на таймауте дренажа ctx уже Done().
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

	// Email в title маскируем до Upsert/OnIssue — иначе утечёт в PG/алерт открытым.
	// Fingerprint уже посчитан на исходном тексте, группировка не меняется.
	ev.Title = p.Scrub.ScrubMessage(ev.Title)

	// Upsert идёт до batcher.Add: IssueID нужен для строки события — из-за этого
	// issues.times_seen может разойтись со счётом в CH при дропе CH-батча.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := p.issues.Upsert(ctx,
		t.projectID, fp, ev.Title, ev.Culprit, ev.Level, ev.Environment, ev.Timestamp)
	if err != nil {
		p.countDropped(DropStorageError)
		p.countDroppedOrg(t.orgID, dropEvent, 1)
		slog.Error("issue upsert failed, event dropped",
			"project_id", t.projectID, "event_id", ev.EventID, "error", err)
		return
	}
	if (res.New || res.Regression) && p.Alerts != nil {
		kind := alert.KindNewIssue
		if res.Regression {
			kind = alert.KindRegression
		}
		// Свой бюджет, не остаток от Upsert: иначе медленный Upsert оставлял бы
		// постановке алерта считаные миллисекунды и терял её по таймауту.
		alertCtx, alertCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer alertCancel()
		timesSeen := int64(1)
		if iss, err := p.issues.Get(alertCtx, res.IssueID); err != nil {
			slog.Error("issue lookup for alert failed", "issue_id", res.IssueID, "error", err)
		} else {
			timesSeen = iss.TimesSeen
		}
		p.Alerts.OnIssue(alertCtx, alert.Event{
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

	// ScrubJSON дополнительно маскирует email в текстовых значениях полей —
	// no-op при ScrubFreeText=false.
	p.Scrub.ScrubUser(&ev.UserIP, &ev.UserEmail)
	p.Scrub.ScrubTags(ev.Tags)
	ev.ContextsJSON = p.Scrub.ScrubJSON(ev.ContextsJSON)
	ev.StacktraceJSON = p.Scrub.ScrubJSON(ev.StacktraceJSON)
	ev.BreadcrumbsJSON = p.Scrub.ScrubJSON(ev.BreadcrumbsJSON)
	// Тело/заголовки/куки часто несут PII и секреты (Authorization, session-cookie,
	// пароли в form-data) — тот же denylist-скраб, что и contexts.
	ev.RequestJSON = p.Scrub.ScrubJSON(ev.RequestJSON)
	ev.Message = p.Scrub.ScrubMessage(ev.Message)
	excValue = p.Scrub.ScrubMessage(excValue)

	p.waitForRoom(p.batcher)
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

// Порядок важен: Spans.Add идёт первым — запись в CH не ждёт ни PG, ни outbox.
func (p *Pipeline) processTransaction(orgID, projectID int64, tx trace.Transaction) {
	if p.Spans == nil { // трейсинг выключен — Handler сюда не должен доходить
		slog.Warn("tracing disabled, dropping transaction",
			"project_id", projectID, "trace_id", tx.TraceID)
		return
	}
	// Теги — как у событий; данные спанов — отдельно: заголовки/куки/токены
	// часто оседают в span.Data (http.*).
	p.Scrub.ScrubTags(tx.Tags)
	// Имя транзакции нередко URL-образное — ScrubMessage чистит query-токены
	// всегда, email — по флагу.
	tx.Name = p.Scrub.ScrubMessage(tx.Name)
	for i := range tx.Spans {
		p.Scrub.ScrubData(tx.Spans[i].Data)
		tx.Spans[i].Description = p.Scrub.ScrubMessage(tx.Spans[i].Description)
	}

	p.waitForRoom(p.Spans)
	p.Spans.Add(orgID, projectID, tx)
	p.detectPerfIssues(projectID, tx)
}

// Детекция не имеет права ронять приём: паника детектора логируется и на этом
// заканчивается — транзакция уже записана в CH.
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

func (p *Pipeline) perfBudget() time.Duration {
	if p.testPerfBudget > 0 {
		return p.testPerfBudget
	}
	return perfDetectBudget
}

// Используется общий бюджет детекции, не персональный.
func (p *Pipeline) recordFinding(ctx context.Context, projectID int64, tx trace.Transaction, f trace.Finding) {
	res, err := p.Perf.Record(ctx, projectID, f, tx.TraceID)
	if err != nil {
		slog.Error("perf issue record failed",
			"project_id", projectID, "trace_id", tx.TraceID, "kind", f.Kind, "error", err)
		return
	}
	// На повторные обнаружения молчим — иначе алерт на каждый запрос к эндпойнту.
	if p.PerfAlerts == nil || (!res.Created && !res.Regression) {
		return
	}
	// Подавляет только notify — у perf_issues нет флага на записи, Record выше
	// уже отработал.
	if p.Maint != nil {
		if inMaint, err := p.Maint.InMaintenance(ctx, projectID, time.Now()); err != nil {
			slog.Error("perf issue maintenance check failed", "project_id", projectID, "error", err)
		} else if inMaint {
			return
		}
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
