package ingest

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/klauspost/compress/zstd"

	"gitflic.ru/otezvikentiy/gotcha/internal/deploy"
	"gitflic.ru/otezvikentiy/gotcha/internal/host"
	"gitflic.ru/otezvikentiy/gotcha/internal/ingestsignal"
	"gitflic.ru/otezvikentiy/gotcha/internal/log"
	"gitflic.ru/otezvikentiy/gotcha/internal/metric"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/profile"
	"gitflic.ru/otezvikentiy/gotcha/internal/trace"
)

// KeyRejectReason — почему приём отверг запрос на этапе аутентификации по
// ключу, ДО разбора тела и до квот. Причина отделена от факта отказа по той
// же логике, что DropReason у Pipeline: "отсутствует ключ" (клиент вообще не
// прислал sentry_key/DSN-bearer) и "ключ не совпадает с проектом" (валидный
// ключ чужого проекта/окружения, обычно чей-то скопированный не туда DSN) —
// разные проблемы на стороне клиента, и общий счётчик не давал их различить.
type KeyRejectReason string

const (
	// KeyRejectMissingKey — Sentry-запрос вовсе не содержит sentry_key.
	KeyRejectMissingKey KeyRejectReason = "missing_key"
	// KeyRejectInvalidKey — sentry_key прислан, но не резолвится ни в один
	// проект (опечатка, отозванный ключ).
	KeyRejectInvalidKey KeyRejectReason = "invalid_key"
	// KeyRejectProjectMismatch — sentry_key резолвится, но в чужой проект
	// относительно project id из пути запроса.
	KeyRejectProjectMismatch KeyRejectReason = "project_mismatch"
	// KeyRejectMissingBearer — OTLP-запрос без заголовка Authorization: Bearer.
	KeyRejectMissingBearer KeyRejectReason = "missing_bearer"
	// KeyRejectInvalidDSNKey — OTLP bearer-токен не резолвится ни в один DSN.
	KeyRejectInvalidDSNKey KeyRejectReason = "invalid_dsn_key"
	// KeyRejectScope — ключ валиден и принадлежит проекту, но его тип не
	// допущен к этому эндпойнту. В отличие от прочих причин файла, это НЕ
	// проблема доставки ключа: источник настроен ключом не того класса. Лог
	// пишет path (countKeyReject) — дежурный видит, по какому эндпойнту бьют.
	KeyRejectScope KeyRejectReason = "scope"
)

// keyRejectReasons — полный набор причин отказа по ключу. Существует, чтобы
// счётчики создавались один раз при инициализации (как dropReasons у
// Pipeline): тогда countKeyReject на горячем пути обходится атомарным
// инкрементом без блокировки и без записи в map.
var keyRejectReasons = []KeyRejectReason{
	KeyRejectMissingKey, KeyRejectInvalidKey, KeyRejectProjectMismatch,
	KeyRejectMissingBearer, KeyRejectInvalidDSNKey, KeyRejectScope,
}

// KeyRejectReasons — все причины, по которым приём умеет отказывать по
// ключу. main регистрирует по self-метрике на причину (см. DropReasons).
func KeyRejectReasons() []KeyRejectReason {
	return append([]KeyRejectReason(nil), keyRejectReasons...)
}

func newKeyRejectCounters() map[KeyRejectReason]*atomic.Int64 {
	m := make(map[KeyRejectReason]*atomic.Int64, len(keyRejectReasons))
	for _, r := range keyRejectReasons {
		m[r] = new(atomic.Int64)
	}
	return m
}

// Handler — HTTP-слой Sentry-протокола.
type Handler struct {
	keys     *KeyCache
	quota    QuotaChecker
	pipeline *Pipeline
	maxBytes int64

	// keyRejected — отказы аутентификации по ключу, по причине (PROD-P?:
	// раньше ни одна из шести веток отказа authenticate/otlpAuthenticate не
	// была видна нигде — ни self-метрикой, ни логом — при том что соседние
	// отказы того же файла (rate limit, квота, лимит item'ов envelope'а) уже
	// логируют slog.Warn. Процесс-локальный, как Pipeline.dropped: приём не
	// знает организацию запроса, пока ключ не резолвился, поэтому это НЕ
	// per-org учёт (DropCounter), а просто self-телеметрия процесса.
	keyRejected map[KeyRejectReason]*atomic.Int64

	// rejected — отказы приёма по (reason, signal), огрублённые до вида,
	// одинаково читаемого по всем шести входам (см. IngestRejectReason).
	// Отдельная карта от keyRejected: та детальна по ключу, эта — по всем
	// причинам отказа сразу и с меткой вида телеметрии, которой у keyRejected
	// нет.
	rejected map[IngestRejectionKey]*atomic.Int64

	// deprecated — попадания в старые пути приёма, оставленные алиасами до 2.0
	// (см. deprecated.go). Отдельная карта от rejected: та про отказы, эта —
	// про запросы, пришедшие не туда, куда сегодня зовёт документация. Счётчик
	// двигается ДО аутентификации и лимитера (см. deprecatedAlias), поэтому
	// сюда попадают и отказы 401/429 на устаревшем пути — это не только
	// успешно принятые запросы.
	deprecated map[DeprecatedPath]*atomic.Int64

	// deprecatedLogged — по одному sync.Once на путь: предупреждение об
	// устаревшем пути пишется один раз за жизнь процесса, а не на каждый запрос.
	deprecatedLogged map[DeprecatedPath]*sync.Once

	// hostScopeSkipped — сколько экспортов метрик пришло с host.*-атрибутами
	// ключом, которому регистрация хоста не разрешена. Отдельный счётчик, а не
	// метка существующих: gotcha_ingest_rejected_total считает ОТКАЗАННЫЕ
	// запросы, а здесь запрос принят (метрики пишутся, не регистрируется
	// только хост); а Toucher.RejectedNames означает «упёрлись в потолок
	// хостов на проект» — слить их значило бы сделать две причины
	// неразличимыми ровно тогда, когда дежурный выясняет, почему хост не
	// появился.
	hostScopeSkipped atomic.Int64

	// rate — дешёвый per-DSN (по project id) токен-бакет ПЕРЕД quota-проверкой:
	// срезает флуд с одного ключа до похода в PG (см. ratelimit.go). Задаётся в
	// NewHandler дефолтом; заменяем на nil/свой через SetRateLimit для тестов и
	// тонкой настройки. nil → лимит выключен.
	rate *rateLimiter

	// overloadLogMu/lastOverloadLog — throttle предупреждения overloaded по
	// signal'у (см. overloadLogInterval). В отличие от rateLimited (тот бьёт
	// один флудящий ключ, объём лога ограничен его же лимитом), overloaded
	// срабатывает на КАЖДЫЙ запрос КАЖДОГО клиента, пока просажен общий буфер
	// (например, лежит ClickHouse) — без троттлинга лог сам стал бы нагрузкой
	// в момент, когда система и так не справляется (см. event.Batcher.lastDropLog,
	// тот же приём).
	overloadLogMu   sync.Mutex
	lastOverloadLog map[IngestSignal]time.Time

	// TxQuota — квота ТРАНЗАКЦИЙ, отдельная от quota (квоты ошибок): у них
	// разные лимиты и разные счётчики, исчерпание одной не закрывает приём по
	// другой. nil → транзакции не квотируются.
	TxQuota QuotaChecker

	// Projects — настройки проекта (transaction_sample_rate). nil → семплируем
	// все транзакции (rate = 1).
	Projects ProjectSettings

	// Metrics — приёмник OTLP-метрик (этап 6): /v1/metrics кладёт распарсенные
	// точки сюда (metric.Writer ему удовлетворяет). nil → метрики выключены,
	// эндпоинт отвечает успехом без записи (коллектор не ретраит вечно).
	Metrics MetricSink
	// MetricQuota — квота МЕТРИК (metric_quota против org_usage.metrics_count),
	// отдельный счётчик. nil → метрики не квотируются.
	MetricQuota QuotaChecker

	// Profiles — приёмник профилей (этап 7): Sentry-профили из envelope и
	// pprof из /api/v1/profiles/pprof кладут распарсенные Profile сюда (*profile.Writer).
	// nil → профили выключены (не пишутся).
	Profiles ProfileSink
	// ProfileQuota — квота ПРОФИЛЕЙ (profile_quota против org_usage.profiles_count).
	// nil → профили не квотируются.
	ProfileQuota QuotaChecker

	// DropCounter — учёт ОТКЛОНЁННЫХ (drop) единиц по орге/месяцу (PROD-P1: конец
	// молчаливых потерь). Инкрементируется в каждой ветке дропа best-effort:
	// ошибка логируется, но не меняет статус ответа. nil → не считаем.
	// Присваивается опционально (как Metrics/TxQuota); *org.Service ему удовлетворяет.
	DropCounter DropCounter

	// Scrub — зачистка ПДн атрибутов OTLP-метрик перед записью (152-ФЗ). Путь
	// метрик идёт МИМО ingest.Pipeline (и его Scrubber'а), поэтому scrubber
	// нужен и здесь. Присваивается опционально (как Metrics/DropCounter); nil →
	// scrubbing выключен (методы Scrubber nil-safe, вызов делается без проверки).
	Scrub *Scrubber

	// Cardinality ограничивает число различных значений полей на проект.
	// nil — ограничение выключено (методы nil-safe).
	Cardinality *CardinalityGuard

	// Hosts регистрирует хосты, приславшие метрики (PG-сущность «хост», см.
	// internal/host). Опциональный: nil — приём работает без регистрации
	// (режимы без PG). *host.Toucher ему удовлетворяет.
	Hosts HostRegistry

	// Logs — приёмник логов (C1): /v1/logs (OTLP) и /api/v1/logs (NDJSON, алиас
	// /logs до 2.0) кладут распарсенные записи сюда (*log.Writer ему
	// удовлетворяет). nil → логи выключены, эндпоинты отвечают успехом без
	// записи (как Metrics nil).
	Logs LogSink
	// LogQuota — квота ЛОГОВ (log_quota против org_usage.logs_count),
	// отдельный счётчик. nil → логи не квотируются.
	LogQuota QuotaChecker

	// Deploy — реестр деплоев проекта (C5): ingest-эндпоинт деплоя пишет сюда
	// событие выкладки из CI (*deploy.Store, PG-таблица deployments). nil →
	// приём деплоев выключен, эндпоинт отвечает 503.
	Deploy *deploy.Store

	// Signals — per-project учёт сигналов приёма (аудит перед 1.0, K7-5/K7-6):
	// отказ по ключу и попадание на устаревший путь, отдельно от
	// процесс-локальных self-метрик, у которых нет метки проекта. nil →
	// сигналы не пишутся (методы touchSignal/touchDeprecatedSignal nil-safe,
	// как у остальных опциональных полей выше). *ingestsignal.Recorder ему
	// удовлетворяет.
	Signals SignalRecorder
}

// SignalRecorder принимает попадание одного per-project сигнала приёма.
// Реализация — *ingestsignal.Recorder; duck-typing интерфейс, а не прямая
// зависимость от конкретного типа, по тому же поводу, что HostRegistry/
// MetricSink ниже — пакет ingest не должен знать способ агрегации сигнала,
// только факт «сигнал случился».
type SignalRecorder interface {
	Touch(projectID int64, kind ingestsignal.Kind)
}

// HostRegistry регистрирует хосты, приславшие метрики (PG-сущность «хост»).
// Опциональный: nil — приём работает без регистрации (режимы без PG).
// Семантика: «приём принял экспорт», не «данные записаны в CH» — поэтому
// вызывается и при отказе по квоте (живость хоста ≠ запись точек).
type HostRegistry interface {
	Touch(ctx context.Context, projectID int64, entries []host.TouchEntry)
}

// DropCounter учитывает отклонённые единицы приёма по орге за текущий месяц.
// Реализация — *org.Service (методы IncDropped*). Сигнатуры совпадают с ним, так
// что сервис подставляется в поле напрямую.
type DropCounter interface {
	IncDroppedEvents(ctx context.Context, orgID int64, month time.Time, n int64) error
	IncDroppedTransactions(ctx context.Context, orgID int64, month time.Time, n int64) error
	IncDroppedMetrics(ctx context.Context, orgID int64, month time.Time, n int64) error
	IncDroppedProfiles(ctx context.Context, orgID int64, month time.Time, n int64) error
	IncDroppedLogs(ctx context.Context, orgID int64, month time.Time, n int64) error
}

// MetricSink принимает распарсенную metric-точку. Реализация — *metric.Writer.
type MetricSink interface {
	Add(projectID int64, p metric.MetricPoint)
}

// ProfileSink принимает распарсенный профиль. Реализация — *profile.Writer.
type ProfileSink interface {
	Add(projectID int64, p profile.Profile)
}

// LogSink принимает распарсенную запись лога. Реализация — *log.Writer.
type LogSink interface {
	Add(projectID int64, r log.LogRecord)
}

func NewHandler(keys *KeyCache, quota QuotaChecker, pipeline *Pipeline, maxEventBytes int64) *Handler {
	return &Handler{
		keys:        keys,
		quota:       quota,
		pipeline:    pipeline,
		maxBytes:    maxEventBytes,
		rate:        newRateLimiter(time.Now, defaultIngestRatePerSec, defaultIngestBurst),
		keyRejected: newKeyRejectCounters(),
		rejected:    newIngestRejectCounters(),

		lastOverloadLog: make(map[IngestSignal]time.Time),

		deprecated:       newDeprecatedCounters(),
		deprecatedLogged: newDeprecatedLogOnce(),
	}
}

// countKeyReject увеличивает self-счётчик отказа по ключу и пишет
// предупреждение в лог — тем же уровнем, что соседние отказы этого файла
// (rate limit, квота, лимит item'ов envelope'а). path — r.URL.Path вызывающего
// эндпоинта, чтобы отличить Sentry-envelope/security-report/deployments/OTLP
// metrics/logs/traces/pprof друг от друга в логе (self-метрика их не
// различает — у неё нет метки эндпоинта, только причина).
func (h *Handler) countKeyReject(reason KeyRejectReason, path string) {
	if c, ok := h.keyRejected[reason]; ok {
		c.Add(1)
	}
	slog.Warn("ingest: key rejected", "reason", string(reason), "path", path)
}

// touchSignal отмечает per-project сигнал приёма (K7-5/K7-6), если Signals
// задан. nil-safe, как остальные опциональные поля Handler.
func (h *Handler) touchSignal(projectID int64, kind ingestsignal.Kind) {
	if h.Signals != nil {
		h.Signals.Touch(projectID, kind)
	}
}

// KeyRejectedBy — сколько запросов отклонено по конкретной причине отказа по
// ключу. Для самотелеметрии: метка reason у gotcha_ingest_key_rejections_total.
func (h *Handler) KeyRejectedBy(reason KeyRejectReason) int64 {
	if c, ok := h.keyRejected[reason]; ok {
		return c.Load()
	}
	return 0
}

// SetRateLimit заменяет per-DSN лимитер приёма (см. Handler.rate): позволяет
// подстроить дефолт или выключить лимит (ratePerSec<=0), не меняя сигнатуру
// NewHandler. rl==nil в вызове также означает «лимит выключен».
func (h *Handler) SetRateLimit(now func() time.Time, ratePerSec, burst float64) {
	if now == nil {
		now = time.Now
	}
	h.rate = newRateLimiter(now, ratePerSec, burst)
}

// rateLimited проверяет per-DSN лимит по project id и, если превышен, пишет 429 с
// коротким Retry-After (в отличие от квоты — окно не месяц, а доли секунды).
// Возвращает true, если запрос НАДО отклонить (ответ уже записан). Вызывается
// ПОСЛЕ аутентификации (нужен project id) и ДО quota-проверки (дешевле её).
// signal — метка gotcha_ingest_rejected_total{reason="rate_limit",signal}.
func (h *Handler) rateLimited(w http.ResponseWriter, orgID, projectID int64, signal IngestSignal) bool {
	if h.rate == nil || h.rate.Allow(projectID) {
		return false
	}
	slog.Warn("ingest: per-DSN rate limit exceeded",
		"project_id", projectID, "org_id", orgID)
	h.countRejected(RejectRateLimit, signal)
	w.Header().Set("Retry-After", "1")
	writeJSONError(w, http.StatusTooManyRequests, "rate limit exceeded")
	return true
}

// overloadThreshold — заполненность буфера (см. saturationSource), начиная с
// которой приём отвечает отказом вместо приёма-с-потерей (см. overloaded).
// Не переменная окружения: это не настройка эксплуатации, а точка, за которой
// буфер начинает выбрасывать данные (drop-oldest у event.Batcher/trace.
// SpanWriter/metric.Writer/log.Writer/profile.Writer, drop у Pipeline.queue) —
// значение технического поведения буфера, а не решение оператора.
const overloadThreshold = 0.95

// saturationSource — опциональная способность приёмника сообщить, насколько
// полон его буфер. Отдельный интерфейс, а не метод Saturation() на eventSink/
// SpanSink/MetricSink/ProfileSink/LogSink: те контракты реализованы кучей
// тестовых двойников по всему пакету (fakeBatcher, fakeSpanSink,
// collectMetricSink и т.п.), и добавление метода в контракт сломало бы их
// разом. Способность спрашивается опционально через type-assert (см.
// saturationOf), а не расширением контракта.
type saturationSource interface{ Saturation() float64 }

// saturationOf возвращает заполненность буфера v в долях единицы, если v умеет
// её сообщать, и 0 иначе — в том числе когда v нетипизированно nil (сигнал
// выключен: h.Logs/h.Metrics/h.Profiles == nil) или не реализует
// saturationSource (существующие тестовые двойники пакета, не знающие о
// Saturation).
//
// Проверка на typed-nil (указатель за интерфейсом ненулевой, а сам указатель
// нулевой) здесь намеренно НЕ нужна: единственный источник такого значения в
// пакете — поле Pipeline.batcher, а оно строится ТОЛЬКО через NewPipeline,
// которая принимает конкретный *event.Batcher и уже сама решает не заворачивать
// nil в интерфейс (см. её докблок). saturationOf — общий хелпер на горячем
// пути всех семи входов приёма; дороже reflect на каждый запрос — держать её
// nil-безопасной для конкретного типа, о котором остальной пакет ничего не
// знает.
//
// Нулевая заполненность у выключенного/немого/нулевого сигнала — намеренно:
// overloaded не должен отбивать то, что и так отвечает успехом без записи
// (см. otlpMetrics/otlpLogs/pprofIngest — h.Metrics/h.Logs/h.Profiles == nil
// уже отвечают успехом раньше, чем дело доходит до preflight).
func saturationOf(v any) float64 {
	if v == nil {
		return 0
	}
	s, ok := v.(saturationSource)
	if !ok {
		return 0
	}
	return s.Saturation()
}

// overloaded проверяет заполненность буфера signal'а ДО постановки элемента и,
// если она достигла overloadThreshold, отвечает 503 вместо приёма-с-потерей.
// Возвращает true, если запрос НАДО отклонить (ответ уже записан) — тот же
// протокол, что у rateLimited.
//
// Вызывающий ОБЯЗАН звать overloaded ДО h.grant: иначе отказ списал бы квоту
// организации за элемент, который дальше всё равно выбросит переполненный
// буфер, и списал бы её ЕЩЁ РАЗ при ретрае клиента на 503 — организация
// платила бы дважды за один и тот же непринятый элемент. Проверка стоит ДО
// постановки и по той же причине: ни один элемент запроса не принят, и
// повторная доставка клиентом не создаёт дублей (дедупликации по event_id в
// продукте нет).
//
// Retry-After — 5 секунд: буфер разгружается воркерами пайплайна/периодическим
// флашем писателя на порядки быстрее месячного окна квоты (в отличие от
// writeQuotaExceeded, где ретраить раньше начала следующего месяца бессмысленно).
func (h *Handler) overloaded(w http.ResponseWriter, orgID, projectID int64, signal IngestSignal, saturation float64) bool {
	if saturation < overloadThreshold {
		return false
	}
	h.logOverloaded(signal, saturation, orgID, projectID)
	h.countRejected(RejectOverloaded, signal)
	w.Header().Set("Retry-After", "5")
	writeJSONError(w, http.StatusServiceUnavailable, "ingest overloaded")
	return true
}

// overloadLogInterval — не чаще одного предупреждения на signal за интервал
// (см. lastOverloadLog). rateLimited (сосед) логирует безусловно на каждый
// отказ, потому что бьёт один флудящий ключ — объём лога ограничен его же
// лимитом запросов. overloaded так не может: он срабатывает на каждый запрос
// КАЖДОГО клиента, пока просажен общий буфер (например, лежит ClickHouse), и
// без троттлинга сам стал бы нагрузкой ровно тогда, когда система и так не
// справляется.
const overloadLogInterval = 5 * time.Second

// logOverloaded пишет предупреждение об отказе по overloaded не чаще одного
// раза в overloadLogInterval на signal. Счётчик gotcha_ingest_rejected_total
// (countRejected) растёт при КАЖДОМ отказе независимо от троттлинга лога —
// дежурный видит точный объём по self-метрике, лог лишь даёт пример «что
// именно и насколько насыщено» без флуда.
func (h *Handler) logOverloaded(signal IngestSignal, saturation float64, orgID, projectID int64) {
	h.overloadLogMu.Lock()
	last, seen := h.lastOverloadLog[signal]
	log := !seen || time.Since(last) > overloadLogInterval
	if log {
		h.lastOverloadLog[signal] = time.Now()
	}
	h.overloadLogMu.Unlock()
	if !log {
		return
	}
	slog.Warn("ingest: buffer overloaded, rejecting instead of dropping",
		"signal", signal, "saturation", saturation, "project_id", projectID, "org_id", orgID)
}

// muxRegistrar — то, что Register нужно от мультиплексора. Сужение ради
// сторожа маршрутов (scope_routes_test.go): http.ServeMux не даёт перечислить
// зарегистрированные паттерны, а подменный регистратор — даёт. В бою сюда
// приходит тот же *http.ServeMux.
type muxRegistrar interface {
	HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request))
}

func (h *Handler) Register(mux muxRegistrar) {
	// Браузерные SDK шлют телеметрию с ПРОИЗВОЛЬНОГО origin (сайт и gotcha —
	// разные домены), поэтому envelope/store отвечают CORS-заголовками и
	// обрабатывают preflight (OPTIONS). DSN (public key) не секрет — как у
	// Sentry, разрешаем любой origin.
	mux.HandleFunc("POST /api/{project}/envelope/{$}", cors(h.envelope))
	mux.HandleFunc("OPTIONS /api/{project}/envelope/{$}", corsPreflight)
	mux.HandleFunc("POST /api/{project}/store/{$}", cors(h.store))
	mux.HandleFunc("OPTIONS /api/{project}/store/{$}", corsPreflight)
	// OTLP — ВТОРОЙ ВХОД в тот же пайплайн (см. otlp.go): своей квоты, своей
	// модели и своих таблиц у него нет.
	mux.HandleFunc("POST /v1/traces", h.otlpTraces)
	// OTLP-метрики (этап 6) — третий вход в ingest: своя квота и своя таблица
	// metric_points (см. otlp.go otlpMetrics).
	mux.HandleFunc("POST /v1/metrics", h.otlpMetrics)
	// Профили pprof (этап 7): свой минимальный эндпоинт (стандарта пуша pprof
	// нет), Bearer-DSN auth + метаданные из query. Канон — собственный
	// неймспейс /api/v1/*; корневой /profiles/pprof остаётся алиасом до 2.0.
	mux.HandleFunc("POST /api/v1/profiles/pprof", h.pprofIngest)
	mux.HandleFunc("POST /profiles/pprof", h.deprecatedAlias(DeprecatedProfilePprof, h.pprofIngest))
	// Логи (C1) — OTLP-вход /v1/logs, четвёртая дверь в тот же ingest-mux (своя
	// квота и своя таблица logs, см. logs.go otlpLogs), и NDJSON-вход для
	// источников без OTLP-экспортёра (см. logsNDJSON). Это НЕ дубликат: два
	// формата одного сигнала поверх общей аутентификации, лимитера и квоты.
	// /v1/logs принадлежит стандарту OTLP и не переезжает никогда; NDJSON-вход
	// наш, поэтому его канон — /api/v1/logs, а корневой /logs остаётся алиасом.
	mux.HandleFunc("POST /v1/logs", h.otlpLogs)
	mux.HandleFunc("POST /api/v1/logs", h.logsNDJSON)
	mux.HandleFunc("POST /logs", h.deprecatedAlias(DeprecatedLogs, h.logsNDJSON))
	// Деплои (C5) — server-to-server вход из CI (не браузер), поэтому без CORS и
	// preflight, в отличие от envelope/store: sentry_key передаётся заголовком
	// или query, тело — JSON одного события выкладки (см. deployments.go).
	// ОБЕ формы канона регистрируются явно: на незарегистрированную форму
	// ServeMux ответил бы 307, а клиенты приёма (CI-скрипты, curl без -L)
	// редиректы на POST не следуют — это была бы тихая потеря маркеров.
	// Каноном в документации объявлена форма без завершающего слэша.
	mux.HandleFunc("POST /api/v1/{project}/deployments", h.deploymentsIngest)
	mux.HandleFunc("POST /api/v1/{project}/deployments/{$}", h.deploymentsIngest)
	mux.HandleFunc("POST /api/{project}/deployments/{$}",
		h.deprecatedAlias(DeprecatedDeployments, h.deploymentsIngest))
}

// corsHeaders разрешает кросс-origin отправку телеметрии из браузера: DSN
// (public key) публичен по замыслу, а браузерные SDK приходят с произвольных
// доменов — как и Sentry, ingest отвечает Access-Control-Allow-Origin: *.
// Credentials не используются, поэтому wildcard-origin безопасен.
func corsHeaders(w http.ResponseWriter) {
	head := w.Header()
	head.Set("Access-Control-Allow-Origin", "*")
	head.Set("Access-Control-Allow-Methods", "POST, OPTIONS")
	head.Set("Access-Control-Allow-Headers", "content-type, x-sentry-auth, x-requested-with, baggage, sentry-trace")
	head.Set("Access-Control-Max-Age", "86400")
}

// cors оборачивает POST-обработчик ingest, добавляя CORS-заголовки к ответу.
func cors(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		corsHeaders(w)
		next(w, r)
	}
}

// corsPreflight отвечает на CORS-preflight (OPTIONS) без тела.
func corsPreflight(w http.ResponseWriter, _ *http.Request) {
	corsHeaders(w)
	w.WriteHeader(http.StatusNoContent)
}

// scopeReject отвечает 403 и считает отказ по скоупу в ОБЕ метрики: узкую
// (gotcha_ingest_key_rejections_total{reason="scope"}, с path в логе) и
// широкую (gotcha_ingest_rejected_total{reason="key_scope",signal}), плюс
// per-project сигнал KindKeyScope (K7-5/K7-6) — projectID приходит от
// вызывающего (authenticate/otlpAuthenticate), там ключ уже резолвлен.
//
// 403, а не 401, ВКЛЮЧАЯ OTLP-вход, где соседние ветки отвечают 401:
// расхождение осознанное и семантически верное — 401 значит «ты не
// представился», 403 — «представился, но сюда нельзя», а ключ здесь
// резолвится успешно.
func (h *Handler) scopeReject(w http.ResponseWriter, r *http.Request, signal IngestSignal, projectID int64) {
	h.countKeyReject(KeyRejectScope, r.URL.Path)
	h.countRejected(RejectKeyScope, signal)
	h.touchSignal(projectID, ingestsignal.KindKeyScope)
	writeJSONError(w, http.StatusForbidden, "key type not allowed for this endpoint")
}

// authenticate проверяет ключ проекта; при успехе возвращает ключ и true. При
// отказе сама пишет ошибку в w и возвращает false. Квоты здесь НЕ проверяются:
// их две (ошибки и транзакции), и какую списывать — видно только после
// разбора envelope'а. signal — метка gotcha_ingest_rejected_total{reason=
// "key_unknown",signal}: все три ветки отказа сводятся к одной причине, см.
// докблок IngestRejectReason.
//
// also — сигналы СВЕРХ signal, которые маршрут может нести (envelope). Пусто
// — маршрут односигнальный. Гейт скоупа стоит ПОСЛЕ резолва ключа и сверки
// проекта: отказ по типу ключа имеет смысл только для ключа, который
// действительно принадлежит этому проекту.
func (h *Handler) authenticate(w http.ResponseWriter, r *http.Request, signal IngestSignal, also ...IngestSignal) (org.Key, bool) {
	projectID, err := strconv.ParseInt(r.PathValue("project"), 10, 64)
	if err != nil {
		writeJSONError(w, http.StatusNotFound, "unknown project")
		return org.Key{}, false
	}
	pub := PublicKeyFromRequest(r)
	if pub == "" {
		h.countKeyReject(KeyRejectMissingKey, r.URL.Path)
		h.countRejected(RejectKeyUnknown, signal)
		// projectID из URL — ключа нет вовсе, резолвить нечего. Проект мог и
		// не существовать (перебор id в пути): Store.Bump на такой id — no-op.
		h.touchSignal(projectID, ingestsignal.KindKeyInvalid)
		writeJSONError(w, http.StatusUnauthorized, "missing sentry_key")
		return org.Key{}, false
	}
	key, err := h.keys.Resolve(r.Context(), pub)
	switch {
	case errors.Is(err, org.ErrNotFound):
		h.countKeyReject(KeyRejectInvalidKey, r.URL.Path)
		h.countRejected(RejectKeyUnknown, signal)
		h.touchSignal(projectID, ingestsignal.KindKeyInvalid)
		writeJSONError(w, http.StatusForbidden, "invalid sentry_key")
		return org.Key{}, false
	case err != nil:
		writeJSONError(w, http.StatusServiceUnavailable, "key lookup failed")
		return org.Key{}, false
	case key.ProjectID != projectID:
		h.countKeyReject(KeyRejectProjectMismatch, r.URL.Path)
		h.countRejected(RejectKeyUnknown, signal)
		h.touchSignal(key.ProjectID, ingestsignal.KindKeyProjectMismatch)
		writeJSONError(w, http.StatusForbidden, "sentry_key does not match project")
		return org.Key{}, false
	}

	if !scopeAllowsRoute(key.Kind, signal, also) {
		h.scopeReject(w, r, signal, key.ProjectID)
		return org.Key{}, false
	}

	h.touchDeprecatedSignal(r.Context(), key.ProjectID)
	return key, true
}

// grant списывает want единиц из квоты q и возвращает, СКОЛЬКО разрешено
// принять: 0 — квота исчерпана, want — влезло всё, промежуточное — влезла
// часть, и остаток вызывающий обязан выбросить и посчитать в дропы.
//
// Считается за элемент, а не за запрос: конверт с тысячей событий стоил ровно
// столько же, сколько одно событие, поэтому квота обходилась на порядки, а
// org_usage — то, по чему оператор судит о потреблении, — врал на столько же.
//
// nil-квота (не сконфигурирована) и сбой счётчика → fail-open: терять данные
// из-за сбоя квот хуже, чем иногда пропустить организацию сверх квоты.
//
// quotaKind — вид телеметрии для КВОТЫ (event/transaction/...); не путать с
// org.KeyKind, типом ключа приёма.
func (h *Handler) grant(ctx context.Context, q QuotaChecker, orgID int64, quotaKind string, want int) int {
	if want <= 0 {
		return 0
	}
	if q == nil {
		return want
	}
	granted, err := q.CheckAndCount(ctx, orgID, int64(want))
	if err != nil {
		slog.Warn("ingest: quota check failed, allowing items",
			"org_id", orgID, "kind", quotaKind, "want", want, "error", err)
		return want
	}
	return int(granted)
}

// dropKind — класс отклонённой единицы для countDrop.
type dropKind int

const (
	dropEvent dropKind = iota
	dropTransaction
	dropMetric
	dropProfile
	dropLog
)

// countDrop списывает n отклонённых единиц класса kind на текущий месяц орги.
// Best-effort: nil-счётчик или n<=0 — no-op, ошибка счётчика логируется, но не
// влияет на ответ (терять статус ответа из-за учёта потерь бессмысленно).
func (h *Handler) countDrop(ctx context.Context, kind dropKind, orgID int64, n int) {
	if h.DropCounter == nil || n <= 0 {
		return
	}
	month := time.Now().UTC()
	var err error
	switch kind {
	case dropEvent:
		err = h.DropCounter.IncDroppedEvents(ctx, orgID, month, int64(n))
	case dropTransaction:
		err = h.DropCounter.IncDroppedTransactions(ctx, orgID, month, int64(n))
	case dropMetric:
		err = h.DropCounter.IncDroppedMetrics(ctx, orgID, month, int64(n))
	case dropProfile:
		err = h.DropCounter.IncDroppedProfiles(ctx, orgID, month, int64(n))
	case dropLog:
		err = h.DropCounter.IncDroppedLogs(ctx, orgID, month, int64(n))
	}
	if err != nil {
		slog.Warn("ingest: drop counter update failed",
			"org_id", orgID, "kind", kind, "n", n, "error", err)
	}
}

// writeQuotaExceeded пишет 429 с Retry-After — числом секунд до 1-го числа
// следующего месяца UTC, когда счётчик организации обнулится. Каждый вызов —
// это ОТКАЗАННЫЙ запрос (не путать с частичным списанием квоты у envelope,
// см. IngestRejectReason.RejectQuota), поэтому он же считает
// gotcha_ingest_rejected_total{reason="quota",signal}.
func (h *Handler) writeQuotaExceeded(w http.ResponseWriter, signal IngestSignal, detail string) {
	h.countRejected(RejectQuota, signal)
	w.Header().Set("Retry-After", strconv.FormatInt(secondsUntilNextMonth(time.Now().UTC()), 10))
	writeJSONError(w, http.StatusTooManyRequests, detail)
}

func secondsUntilNextMonth(now time.Time) int64 {
	now = now.UTC()
	y, m, _ := now.Date()
	next := time.Date(y, m+1, 1, 0, 0, 0, 0, time.UTC)
	secs := int64(next.Sub(now).Seconds())
	if secs < 1 {
		secs = 1
	}
	return secs
}

// noopClose — заглушка для тела без компрессии: закрывать нечего.
func noopClose() {}

// body возвращает reader тела с учётом лимитов и Content-Encoding, и функцию
// закрытия декомпрессора (нужно звать defer'ом у вызывающего: zstd.Decoder
// держит фоновую горутину, gzip.Reader — что-то из sync.Pool у большинства
// реализаций, оба реализуют io.Closer, который раньше терялся в io.LimitReader).
func (h *Handler) body(w http.ResponseWriter, r *http.Request) (io.Reader, func(), error) {
	raw := http.MaxBytesReader(w, r.Body, h.maxBytes)
	switch r.Header.Get("Content-Encoding") {
	case "gzip":
		zr, err := gzip.NewReader(raw)
		if err != nil {
			return nil, noopClose, err
		}
		return newLimitedReader(zr, h.maxBytes*10), func() { _ = zr.Close() }, nil
	case "zstd":
		// Лимиты декодера ОБЯЗАТЕЛЬНЫ: размер окна объявляет КЛИЕНТ в заголовке
		// фрейма, и буфер под него аллоцируется при разборе заголовка — до того,
		// как хоть один байт выхода попадёт под newLimitedReader. Без ограничения
		// 10-байтное тело с windowLog=29 просит ~512 МиБ и убивает процесс
		// (проверено: 10 байт → 513 МиБ), то есть даёт удалённый OOM по одному
		// запросу с публичным DSN-ключом. Окно 8 МиБ с запасом покрывает любой
		// легитимный zstd от SDK, MaxMemory совпадает с потолком распакованного.
		zr, err := zstd.NewReader(raw,
			zstd.WithDecoderMaxWindow(8<<20),
			zstd.WithDecoderMaxMemory(uint64(h.maxBytes*10)),
			zstd.WithDecoderConcurrency(1),
		)
		if err != nil {
			return nil, noopClose, err
		}
		return newLimitedReader(zr.IOReadCloser(), h.maxBytes*10), zr.Close, nil
	default:
		return raw, noopClose, nil
	}
}

// limitedReader отдаёт ErrTooLarge, если из потока прочитано больше limit
// байт — в отличие от io.LimitReader, который тихо обрезает поток до limit
// и возвращает io.EOF, маскируя bomb-подобное переполнение под успешный
// (но усечённый) результат.
type limitedReader struct {
	r    io.Reader
	left int64 // limit+1: чтение (limit+1)-го байта = превышение
}

func newLimitedReader(r io.Reader, limit int64) *limitedReader {
	return &limitedReader{r: r, left: limit + 1}
}

func (l *limitedReader) Read(p []byte) (int, error) {
	if l.left <= 0 {
		return 0, ErrTooLarge
	}
	if int64(len(p)) > l.left {
		p = p[:l.left]
	}
	n, err := l.r.Read(p)
	l.left -= int64(n)
	if l.left <= 0 {
		return n, ErrTooLarge
	}
	return n, err
}

// maxGzipLayers — предел вложенности gzip у pprof-тела. Реальный pprof сжат
// одним слоем; несколько слоёв — это «матрёшка»-бомба, которую отклоняем.
const maxGzipLayers = 3

// gunzipLimited ПОЛНОСТЬЮ распаковывает (потенциально многослойный) gzip с
// ограничением размера КАЖДОГО слоя. pprof-клиенты присылают профиль gzip'ом
// ВНУТРИ тела (по конвенции pprof), без Content-Encoding, поэтому h.body такое
// тело не разжимает и лимит на распакованный размер не применяется. Важно
// размотать ВСЕ слои: pp.ParseData сам повторно ищет gzip-magic и разжимает
// внутренний слой БЕЗ предела — двойной gzip обошёл бы одноразовую распаковку
// (≤1 МБ → 10 МБ внутренний gzip под лимитом → ~1 ГБ в ParseData, OOM). После
// цикла в данных не остаётся gzip-magic, поэтому ParseData уже не разжимает.
// Не-gzip вход возвращается как есть.
func gunzipLimited(raw []byte, limit int64) ([]byte, error) {
	for layer := 0; ; layer++ {
		if len(raw) < 2 || raw[0] != 0x1f || raw[1] != 0x8b {
			return raw, nil // больше не gzip — готово
		}
		if layer >= maxGzipLayers {
			return nil, ErrTooLarge // слишком глубокая вложенность — бомба
		}
		zr, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		out, err := io.ReadAll(newLimitedReader(zr, limit))
		_ = zr.Close()
		if err != nil {
			return nil, err
		}
		raw = out
	}
}

func (h *Handler) envelope(w http.ResponseWriter, r *http.Request) {
	key, ok := h.authenticate(w, r, SignalEvent, envelopeAlsoSignals...)
	if !ok {
		return
	}
	projectID := key.ProjectID
	if h.rateLimited(w, key.OrgID, projectID, SignalEvent) {
		return
	}
	body, closeBody, err := h.body(w, r)
	if err != nil {
		h.countRejected(RejectMalformed, SignalEvent)
		writeJSONError(w, http.StatusBadRequest, "bad body encoding")
		return
	}
	defer closeBody()
	env, err := ParseEnvelope(body, h.maxBytes, func(s IngestSignal) bool {
		return scopeAllows(key.Kind, s)
	})
	if err != nil {
		status := http.StatusBadRequest
		reason := RejectMalformed
		var maxErr *http.MaxBytesError
		if errors.Is(err, ErrTooLarge) || errors.As(err, &maxErr) {
			status = http.StatusRequestEntityTooLarge
			reason = RejectTooLarge
		}
		h.countRejected(reason, SignalEvent)
		writeJSONError(w, status, "malformed envelope")
		return
	}
	// Item'ы, отброшенные по лимиту maxEnvelopeItems (защита от амплификации):
	// считаем их дропом и логируем. Класс точно не известен (перебор мог быть по
	// любому из типов), поэтому списываем best-effort в события — доминирующий
	// класс приёма; сам DropCounter best-effort. Принятые item'ы обрабатываются
	// дальше как обычно (ответ 200 по ним, а не отказ всему envelope'у).
	if env.Dropped > 0 {
		slog.Warn("ingest: envelope item limit exceeded, extra items dropped",
			"limit", maxEnvelopeItems, "dropped", env.Dropped,
			"project_id", projectID, "org_id", key.OrgID)
		h.countDrop(r.Context(), dropEvent, key.OrgID, env.Dropped)
	}
	// Отказ по скоупу считается ОДИН РАЗ НА ЗАПРОС на каждый сигнал, у
	// которого хоть один item отброшен, а не по разу на item:
	// gotcha_ingest_rejected_total — метрика ОТКАЗАННЫХ ЗАПРОСОВ (см. докблок
	// IngestRejectReason), и поштучный счёт сделал бы key_scope несравнимым с
	// соседними причинами на дашборде. Детализация по сигналу при этом
	// сохраняется: видно, ЧТО именно пытались слать не тем ключом.
	//
	// Логировать здесь не нужно: отказ по скоупу на самом маршруте уже пишет
	// countKeyReject с путём, а поштучный отбор внутри принятого запроса —
	// рутина браузерного SDK, который шлёт то, чего не умеет.
	for signal := range env.ScopeRejected {
		h.countRejected(RejectKeyScope, signal)
	}

	// Квоты списываются раздельно и только за те типы item'ов, которые в
	// envelope'е реально есть: транзакции не тратят бюджет ошибок и наоборот.
	// 429 отдаём, только если по ВСЕМ присутствующим типам организация вышла
	// за квоту — иначе приняли бы 200 и молча выбросили половину envelope'а.
	hasEvents := len(env.Events) > 0
	// Транзакции ОТБИРАЮТСЯ ДО СПИСАНИЯ: разбор отсеивает битые item'ы,
	// семплирование — те трейсы, которые проект намеренно не хранит. Квота
	// списывается за то, что действительно будет записано. При выключенном
	// трейсинге отбор не выполняется вовсе, и квота транзакций не тратится:
	// раньше grant вызывался до проверки TracingEnabled, и счётчик организации
	// рос за транзакции, которые не записывались никуда.
	var txSelected []trace.Transaction
	if len(env.Transactions) > 0 && h.pipeline.TracingEnabled() {
		txSelected = h.sampleTransactions(r.Context(),
			projectID, h.parseTransactions(projectID, env.Transactions))
	}
	hasTx := len(txSelected) > 0
	hasProfiles := len(env.Profiles) > 0

	// overload preflight envelope'а сложнее, чем у остальных шести входов: он
	// несёт до трёх независимых классов (события/транзакции/профили), и у
	// каждого свой буфер. «Присутствующий» — класс, у которого в ЭТОМ
	// envelope'е реально есть элементы (hasTx — уже ПОСЛЕ отбора/семплирования:
	// несемплированное отброшено намеренно и к насыщению буфера отношения не
	// имеет). Насыщенность считаем один раз, до развилок ниже, чтобы решение
	// «что пропустить» и лог принятого значения не разъезжались на гонке.
	eventSat := h.pipeline.EventSaturation()
	txSat := h.pipeline.TransactionSaturation()
	profSat := saturationOf(h.Profiles)
	eventOverloaded := hasEvents && eventSat >= overloadThreshold
	txOverloaded := hasTx && txSat >= overloadThreshold
	profOverloaded := hasProfiles && profSat >= overloadThreshold
	// Насыщены буферы ВСЕХ присутствующих классов разом: принимать нечего —
	// целиком отказываем 503 ДО постановки и ДО списания квоты, как и у
	// остальных шести входов (см. Handler.overloaded). signal выбирается тем
	// же правилом, что уже применяет соседний отказ по квоте ниже: event, если
	// события есть, иначе класс, который есть.
	if (hasEvents || hasTx || hasProfiles) &&
		(!hasEvents || eventOverloaded) && (!hasTx || txOverloaded) && (!hasProfiles || profOverloaded) {
		signal, saturation := SignalEvent, eventSat
		switch {
		case hasEvents:
		case hasTx:
			signal, saturation = SignalTransaction, txSat
		default:
			signal, saturation = SignalProfile, profSat
		}
		h.overloaded(w, key.OrgID, projectID, signal, saturation)
		return
	}
	// Смешанный случай: хотя бы один присутствующий класс НЕ насыщен —
	// envelope принимается частично. Насыщенный класс квоту не тратит (грант
	// на него попросту не запрашивается — см. eventsGranted/txGranted ниже):
	// иначе организация платила бы за элемент, который дальше и так выбросит
	// переполненный буфер, а при ретрае клиента — ещё раз.
	//
	// eventQuotaRelevant/txQuotaRelevant — присутствующий и НЕ насыщенный
	// класс: только такие участвуют в решении «квота исчерпана по ВСЕМ
	// присутствующим типам» ниже. Насыщенный класс из этого решения исключён:
	// у него отдельная причина отказа (overloaded, не quota), и он не должен
	// провоцировать 429 «квота исчерпана» там, где на самом деле переполнен
	// буфер, а второй присутствующий класс (или профили, которые в 429 никогда
	// не участвовали) принят нормально.
	eventQuotaRelevant := hasEvents && !eventOverloaded
	txQuotaRelevant := hasTx && !txOverloaded
	// profQuotaRelevant — присутствующий и НЕ насыщенный класс профилей,
	// зеркало eventQuotaRelevant/txQuotaRelevant: посчитан здесь же, ДО
	// развилок ниже, чтобы участвовать и в переходной 503 (следующий блок), и
	// в решении «квота исчерпана по ВСЕМ присутствующим типам» на равных с
	// событиями и транзакциями. h.Profiles == nil (приём профилей не
	// сконфигурирован) исключён тем же способом, что и из самой обработки
	// профилей ниже: класс, который приёмник не умеет принимать, не может
	// стать причиной отказа.
	profQuotaRelevant := hasProfiles && h.Profiles != nil && !profOverloaded
	// Квота списывается ЗА ЭЛЕМЕНТ. Списание частичное: если до квоты осталось
	// меньше, чем в конверте, принимаем сколько влезло, остаток идёт в дропы —
	// организация получает ровно свою квоту, а не «последний конверт целиком
	// мимо», и org_usage остаётся точным.
	var eventsGranted int
	if eventQuotaRelevant {
		eventsGranted = h.grant(r.Context(), h.quota, key.OrgID, "event", len(env.Events))
	}
	var txGranted int
	if txQuotaRelevant {
		txGranted = h.grant(r.Context(), h.TxQuota, key.OrgID, "transaction", len(txSelected))
	}
	// profGranted считается ЗДЕСЬ же, наравне с eventsGranted/txGranted — до
	// развилки переходной 503 ниже, а не в блоке обработки профилей дальше по
	// функции. Профиль — такой же ретраибельный класс со своей квотой и своим
	// буфером, что и события с транзакциями: его исчерпание квоты обязано
	// участвовать в выборе между 503 и 429 на общих основаниях, а не только в
	// самом 429. Фактическая постановка профиля в h.Profiles (парсинг,
	// скрабинг, ограничение кардинальности) по-прежнему в блоке ниже — там же
	// остаётся весь дроп-лог; здесь только списание квоты.
	var profGranted int
	if profQuotaRelevant {
		profGranted = h.grant(r.Context(), h.ProfileQuota, key.OrgID, "profile", len(env.Profiles))
	}
	eventsAllowed := eventsGranted > 0
	txAllowed := txGranted > 0
	profAllowed := profGranted > 0
	// Стык двух причин отказа: все присутствующие классы отбиты, но по РАЗНЫМ
	// причинам — хотя бы один насыщен (eventOverloaded/txOverloaded/
	// profOverloaded), другой (другие) честно выбили месячную квоту.
	// Выигрывает ПЕРЕХОДНАЯ причина — 503, а не 429: у 429 Retry-After
	// считается до 1-го числа следующего месяца (writeQuotaExceeded), и такой
	// ответ хоронил бы насыщенный класс, который приёмник принял бы уже через
	// 5 секунд, вместе с честно исчерпанным. Решение ДО учёта дропов ниже —
	// под 503 не принято НИЧЕГО, включая класс, исчерпавший квоту (он получит
	// свой отказ заново при ретрае и ничего не потратит повторно, grant
	// вернёт 0, что уже посчитано выше и ничего не спишет дополнительно), и
	// countDrop не должен считать это дропом: то, что раньше тихо уходило
	// туда под 429, теперь просто не принято. Ветка «все отбиты, но НИ ОДИН
	// не насыщен» сюда не попадает (условие ниже) и остаётся прежним 429
	// бит-в-бит, включая профильный detail.
	//
	// signal выбирается по насыщенному классу в фиксированном порядке:
	// событие, иначе транзакция, иначе профиль — тот же порядок, что уже
	// применяет overload preflight выше (all-saturated ветка).
	if (eventQuotaRelevant || txQuotaRelevant || profQuotaRelevant) &&
		!eventsAllowed && !txAllowed && !profAllowed &&
		(eventOverloaded || txOverloaded || profOverloaded) {
		signal, saturation := SignalProfile, profSat
		switch {
		case eventOverloaded:
			signal, saturation = SignalEvent, eventSat
		case txOverloaded:
			signal, saturation = SignalTransaction, txSat
		}
		h.overloaded(w, key.OrgID, projectID, signal, saturation)
		return
	}
	// Учёт дропов до развилки ответа: отклонённое считаем и когда 429 по ВСЕМ
	// типам (ранний return ниже), и когда 200 по смешанному конверту, и когда
	// принята лишь часть. Насыщенный класс здесь тоже попадает в счёт —
	// eventsGranted/txGranted у него принудительно 0 (грант не звался), и
	// разница len(...)-0 корректно списывает ВСЕ его элементы в дропы. До этой
	// строки код не доходит, если сработала переходная 503 (см. выше).
	if dropped := len(env.Events) - eventsGranted; hasEvents && dropped > 0 {
		h.countDrop(r.Context(), dropEvent, key.OrgID, dropped)
	}
	// Уменьшаемое — число ОТОБРАННЫХ, а не пришедших. С len(env.Transactions) в
	// потери по квоте попадало бы отсеянное семплированием, то есть исправление
	// одной лжи породило бы другую: несемплированное отброшено по настройке
	// проекта намеренно и потерей не является.
	if dropped := len(txSelected) - txGranted; dropped > 0 {
		h.countDrop(r.Context(), dropTransaction, key.OrgID, dropped)
	}
	// Профили (этап 7) обрабатываются ЗДЕСЬ — до развилки ответа по квоте
	// событий/транзакций ниже, а не после постановки событий/транзакций в
	// очередь, как было раньше. Раньше блок стоял в самом конце, и конверт, у
	// которого исчерпаны квоты И событий, И транзакций, отвечал 429 ранним
	// возвратом (см. ниже), даже не дойдя до профилей: они пропадали молча,
	// ни в дроп, ни в лог, никуда — при том, что у профилей своя отдельная
	// квота и свой отдельный буфер, и они не должны гибнуть из-за чужого
	// лимита. Перенос делает профили полноправным классом конверта: приняты
	// они или нет, решается независимо от судьбы событий и транзакций, а не
	// как приложение к ним. Транзитную 503 выше (переходная причина отказа)
	// это тоже уже касается — профиль там полноправный участник (см. блок
	// выше), и до этой строки код не доходит, если 503 сработал.
	//
	// Насыщенный буфер профилей (profOverloaded) — та же дисциплина, что у
	// событий/транзакций выше: квоту не трогаем (h.grant уже не звался и
	// здесь не зовётся — profGranted посчитан выше), все элементы уходят в
	// дроп, класс виден в логе отдельной причиной. Битый профиль по-прежнему
	// пропускается без влияния на статус — это дефект данных клиента, а не
	// отказ приёмника. Эту часть T4 не меняет.
	if hasProfiles && h.Profiles != nil {
		if profOverloaded {
			slog.Warn("ingest: dropping items from envelope",
				"reason", "buffer overloaded", "class", "profile",
				"dropped", len(env.Profiles), "accepted", 0,
				"project_id", projectID, "org_id", key.OrgID)
			h.countDrop(r.Context(), dropProfile, key.OrgID, len(env.Profiles))
		} else {
			if dropped := len(env.Profiles) - profGranted; dropped > 0 {
				slog.Warn("ingest: profile quota exceeded, dropping profiles",
					"dropped", dropped, "accepted", profGranted,
					"project_id", projectID, "org_id", key.OrgID)
				h.countDrop(r.Context(), dropProfile, key.OrgID, dropped)
			}
			for _, raw := range env.Profiles[:profGranted] {
				prof, err := profile.ParseSentry(raw, time.Now().UTC())
				if err != nil {
					slog.Warn("ingest: bad sentry profile, skipped", "project_id", projectID, "error", err)
					continue
				}
				h.scrubProfile(&prof)
				h.limitProfileCardinality(projectID, &prof)
				h.Profiles.Add(key.ProjectID, prof)
			}
		}
	}
	// eventQuotaRelevant/txQuotaRelevant/profQuotaRelevant и eventsAllowed/
	// txAllowed/profAllowed уже посчитаны выше (наравне с переходной 503).
	// Следствие, принятое сознательно: конверт из ОДНИХ профилей с исчерпанной
	// квотой профилей теперь получает 429 вместо прежнего 200 — раньше такой
	// клиент получал успех и не узнавал, что его профили выброшены. Это та же
	// честность, что уже действует для событий и транзакций.
	if (eventQuotaRelevant || txQuotaRelevant || profQuotaRelevant) && !eventsAllowed && !txAllowed && !profAllowed {
		detail := "event quota exceeded"
		signal := SignalEvent
		switch {
		case eventQuotaRelevant:
		case txQuotaRelevant:
			detail = "transaction quota exceeded"
			signal = SignalTransaction
		default:
			detail = "profile quota exceeded"
			signal = SignalProfile
		}
		h.writeQuotaExceeded(w, signal, detail)
		return
	}
	// Смешанный envelope, где по ОДНОМУ классу квота исчерпана ИЛИ буфер
	// насыщен: отвечаем 200 (по второму классу приняли), но выброшенный класс
	// обязан быть виден в логах — иначе оператор не отличит «ошибок не было»
	// от «ошибки молча выброшены», а по overloaded-классу вдобавок не отличит
	// его от обычного quota-дропа.
	if dropped := len(env.Events) - eventsGranted; hasEvents && dropped > 0 {
		reason := "quota exceeded"
		if eventOverloaded {
			reason = "buffer overloaded"
		}
		slog.Warn("ingest: dropping items from envelope",
			"reason", reason, "class", "event", "dropped", dropped, "accepted", eventsGranted,
			"project_id", projectID, "org_id", key.OrgID)
	}
	if dropped := len(txSelected) - txGranted; dropped > 0 {
		reason := "quota exceeded"
		if txOverloaded {
			reason = "buffer overloaded"
		}
		slog.Warn("ingest: dropping items from envelope",
			"reason", reason, "class", "transaction", "dropped", dropped, "accepted", txGranted,
			"project_id", projectID, "org_id", key.OrgID)
	}

	id := env.EventID
	// Принимаем ровно столько, сколько списала квота: остальное уже посчитано
	// в дропы выше. eventsEnqueued/eventCapacityDropped — честный учёт РЕЗУЛЬТАТА
	// постановки (T5): между preflight-проверкой заполненности выше и этим
	// вызовом есть окно, в котором соседний запрос успевает добрать очередь, и
	// Enqueue вернёт false уже после того, как квота списана и решение
	// «принимаем» вроде бы принято. eventCapacityDropped — именно ЁМКОСТНАЯ
	// причина (Enqueue вернул false), а не битый item: последний просто
	// пропускается continue'ом и в capacityDropped не попадает — иначе клиент,
	// приславший мусор, получал бы 503 и ретраил бы его вечно.
	var eventsEnqueued int
	var eventCapacityDropped bool
	for _, raw := range env.Events[:eventsGranted] {
		pe, err := ParseEvent(raw)
		if err != nil {
			continue // битый item не валит весь envelope
		}
		if id == "" {
			id = pe.EventID
		}
		pe.Environment = h.Cardinality.Value(projectID, FieldEnvironment, pe.Environment)
		if h.pipeline.Enqueue(projectID, key.OrgID, pe) {
			eventsEnqueued++
		} else {
			eventCapacityDropped = true
		}
	}
	var txEnqueued int
	var txCapacityDropped bool
	if txGranted > 0 {
		txEnqueued, txCapacityDropped = h.enqueueTransactions(projectID, key.OrgID, txSelected[:txGranted])
	}
	// Ничего реально не встало в очередь, и хотя бы одна из причин — именно
	// ёмкость (не квота, не скоуп, не битые item'ы, не семплирование): повтор
	// клиента в этом случае безопасен, дублировать нечего, а молчаливая потеря
	// под видом 200 — ровно то, что T5 закрывает. Профили сюда не входят: их
	// приёмник не отказывает, а вытесняет старое (см. Pipeline.Add у T6), и
	// понятия «не удалось поставить» у них нет.
	if eventsEnqueued == 0 && txEnqueued == 0 && (eventCapacityDropped || txCapacityDropped) {
		signal := SignalTransaction
		if eventCapacityDropped {
			signal = SignalEvent
		}
		h.overloaded(w, key.OrgID, projectID, signal, 1.0)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": id})
}

// scrubProfile чистит метаданные профиля. Путь профилей шёл МИМО скрубера
// целиком, хотя имя транзакции здесь полностью клиентское (?transaction= у
// pprof, поле конверта у Sentry) и регулярно несёт URL с идентификаторами:
// /users/ivan@example.com/settings. Значение оседает в profile_samples и
// показывается в UI.
//
// Имена кадров (функция/файл) НЕ трогаем: это идентификаторы кода, а не ПДн, и
// маскирование сломало бы схлопывание стеков.
func (h *Handler) scrubProfile(p *profile.Profile) {
	if h.Scrub == nil || p == nil {
		return
	}
	// ScrubMessage, а не ScrubJSON: это свободный текст. URL-часть чистится в нём
	// всегда, независимо от ScrubFreeText.
	p.Transaction = h.Scrub.ScrubMessage(p.Transaction)
	p.Service = h.Scrub.ScrubMessage(p.Service)
	p.Environment = h.Scrub.ScrubMessage(p.Environment)
}

// limitProfileCardinality — то же, что limitCardinality, для профилей: service
// и profile_type стоят в ключе сортировки profile_samples, а имя транзакции
// приходит от клиента полностью.
func (h *Handler) limitProfileCardinality(projectID int64, p *profile.Profile) {
	if h.Cardinality == nil || p == nil {
		return
	}
	p.Transaction = h.Cardinality.Value(projectID, FieldTransaction, p.Transaction)
	p.Service = h.Cardinality.Value(projectID, FieldService, p.Service)
	p.Environment = h.Cardinality.Value(projectID, FieldEnvironment, p.Environment)
}

// parseTransactions разбирает transaction-item'ы конверта. Битый item не валит
// весь конверт и не расходует квоту: списание идёт за отобранное, а до отбора
// он не доживает.
func (h *Handler) parseTransactions(projectID int64, items [][]byte) []trace.Transaction {
	out := make([]trace.Transaction, 0, len(items))
	for _, raw := range items {
		tx, err := ParseTransaction(raw)
		if err != nil {
			slog.Debug("ingest: malformed transaction item, skipped",
				"project_id", projectID, "error", err)
			continue
		}
		out = append(out, tx)
	}
	return out
}

// sampleTransactions оставляет те транзакции, которые проект действительно
// сохранит. Общая для ВСЕХ входов (Sentry-envelope и OTLP) точка отбора:
// семплирование ДЕТЕРМИНИРОВАННОЕ по trace_id, так что все спаны одного трейса
// (в т.ч. приехавшие на другую реплику и из другого SDK) принимают одно и то же
// решение.
//
// Отбор стоит ВЫШЕ списания квоты намеренно, и на консистентность трасс это не
// влияет: решение по trace_id от момента вызова не зависит. Раньше квота
// списывалась за все разобранные транзакции, и при transaction_sample_rate =
// 0.1 организация платила вдесятеро против сохранённого, а org_usage —
// источник правды по потреблению — врал на тот же порядок.
func (h *Handler) sampleTransactions(ctx context.Context, projectID int64, txs []trace.Transaction) []trace.Transaction {
	if len(txs) == 0 {
		return nil
	}
	rate := h.sampleRate(ctx, projectID)
	kept := make([]trace.Transaction, 0, len(txs))
	for _, tx := range txs {
		if trace.Keep(tx.TraceID, rate) {
			kept = append(kept, tx)
		}
	}
	return kept
}

// enqueueTransactions отдаёт отобранное и оплаченное в пайплайн. orgID нужен
// пайплайну только для per-org учёта дропов (см. Pipeline.DropCounter).
//
// Возвращает enqueued — сколько транзакций реально встало в очередь, и
// capacityDropped — был ли среди них хоть один дроп именно по ёмкости
// (EnqueueTransaction вернул false). Вызывающие (envelope, otlpTraces) решают
// по этой паре, честен ли ответ 200: если из присутствующих транзакций не
// встало ни одной, а причина — ёмкость, повтор клиента безопасен и должен
// получить 503, а не молчаливую потерю (см. T5).
func (h *Handler) enqueueTransactions(projectID, orgID int64, txs []trace.Transaction) (enqueued int, capacityDropped bool) {
	for i := range txs {
		tx := txs[i]
		h.limitCardinality(projectID, &tx)
		if h.pipeline.EnqueueTransaction(projectID, orgID, tx) {
			enqueued++
		} else {
			capacityDropped = true
		}
	}
	return enqueued, capacityDropped
}

// limitCardinality схлопывает значения, которыми проект уже исчерпал потолок
// различных значений.
//
// Эти поля стоят в ключах сортировки ClickHouse и в GROUP BY материализованных
// представлений: каждое новое значение создаёт новую строку агрегата с
// состояниями квантилей, которая не схлопнётся ни с чем и переживёт всю
// ретенцию. Один идентификатор, случайно попавший в имя транзакции
// (/users/8812/profile вместо /users/:id/profile), превращает десяток
// эндпойнтов в сотни тысяч — и, поскольку ClickHouse общий на всех тенантов,
// платят за это все.
//
// Схлопываем, а не отбрасываем: суммарные throughput и латентность проекта
// остаются верными, пропадает лишь разбивка по хвосту. Что именно схлопнуто и
// примеры значений видны в отчёте (CardinalityGuard.Report) — без примеров
// человек не догадается, что в имя попал идентификатор.
func (h *Handler) limitCardinality(projectID int64, tx *trace.Transaction) {
	if h.Cardinality == nil {
		return
	}
	tx.Name = h.Cardinality.Value(projectID, FieldTransaction, tx.Name)
	tx.Environment = h.Cardinality.Value(projectID, FieldEnvironment, tx.Environment)
	tx.Op = h.Cardinality.Value(projectID, FieldOp, tx.Op)
	for i := range tx.Spans {
		tx.Spans[i].Op = h.Cardinality.Value(projectID, FieldOp, tx.Spans[i].Op)
	}
}

// sampleRate — transaction_sample_rate проекта. Сбой чтения настроек →
// fail-open (принимаем всё), как и сбой квоты: молча выбросить трейсы из-за
// недоступного PG хуже, чем принять их сверх заданной доли.
func (h *Handler) sampleRate(ctx context.Context, projectID int64) float64 {
	if h.Projects == nil {
		return 1
	}
	p, err := h.Projects.Resolve(ctx, projectID)
	if err != nil {
		slog.Warn("ingest: project settings lookup failed, sampling everything",
			"project_id", projectID, "error", err)
		return 1
	}
	return p.TransactionSampleRate
}

// store — легаси-эндпойнт: одно событие ошибки, транзакций тут не бывает,
// поэтому квота ровно одна (ошибок).
func (h *Handler) store(w http.ResponseWriter, r *http.Request) {
	key, ok := h.authenticate(w, r, SignalEvent)
	if !ok {
		return
	}
	if h.rateLimited(w, key.OrgID, key.ProjectID, SignalEvent) {
		return
	}
	if h.overloaded(w, key.OrgID, key.ProjectID, SignalEvent, h.pipeline.EventSaturation()) {
		return
	}
	if h.grant(r.Context(), h.quota, key.OrgID, "event", 1) == 0 {
		h.countDrop(r.Context(), dropEvent, key.OrgID, 1)
		h.writeQuotaExceeded(w, SignalEvent, "event quota exceeded")
		return
	}
	projectID := key.ProjectID
	body, closeBody, err := h.body(w, r)
	if err != nil {
		h.countRejected(RejectMalformed, SignalEvent)
		writeJSONError(w, http.StatusBadRequest, "bad body encoding")
		return
	}
	defer closeBody()
	raw, err := io.ReadAll(body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.Is(err, ErrTooLarge) || errors.As(err, &maxErr) {
			h.countRejected(RejectTooLarge, SignalEvent)
			writeJSONError(w, http.StatusRequestEntityTooLarge, "event too large")
			return
		}
		h.countRejected(RejectMalformed, SignalEvent)
		writeJSONError(w, http.StatusBadRequest, "bad body")
		return
	}
	pe, err := ParseEvent(raw)
	if err != nil {
		h.countRejected(RejectMalformed, SignalEvent)
		writeJSONError(w, http.StatusBadRequest, "malformed event")
		return
	}
	// store несёт ровно одно событие: если Enqueue вернул false, это событие —
	// единственное содержимое запроса, и ничего не встало в очередь целиком
	// (см. T5, докблок Enqueue). Ответ честно становится 503 вместо прежнего
	// 200: повтор безопасен, ставить было нечего, кроме этого события.
	// saturation=1.0 — не измерение, а констатация уже случившегося факта
	// отказа постановки, которую preflight выше не увидел (окно между
	// проверкой заполненности и этим вызовом).
	if !h.pipeline.Enqueue(projectID, key.OrgID, pe) {
		h.overloaded(w, key.OrgID, projectID, SignalEvent, 1.0)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": pe.EventID})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSONError(w http.ResponseWriter, status int, detail string) {
	writeJSON(w, status, map[string]string{"detail": detail})
}
