package ingest

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
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

// Причина отделена от факта отказа: «нет ключа» и «ключ чужого проекта» —
// разные проблемы клиента, общий счётчик их не различал.
type KeyRejectReason string

const (
	KeyRejectMissingKey      KeyRejectReason = "missing_key"
	KeyRejectInvalidKey      KeyRejectReason = "invalid_key"
	KeyRejectProjectMismatch KeyRejectReason = "project_mismatch"
	KeyRejectMissingBearer   KeyRejectReason = "missing_bearer"
	KeyRejectInvalidDSNKey   KeyRejectReason = "invalid_dsn_key"
	// В отличие от прочих причин, это не проблема доставки ключа: источник
	// настроен ключом не того класса.
	KeyRejectScope KeyRejectReason = "scope"
)

// Существует, чтобы счётчики создавались один раз при инициализации: тогда
// countKeyReject на горячем пути — атомарный инкремент без блокировки и записи в map.
var keyRejectReasons = []KeyRejectReason{
	KeyRejectMissingKey, KeyRejectInvalidKey, KeyRejectProjectMismatch,
	KeyRejectMissingBearer, KeyRejectInvalidDSNKey, KeyRejectScope,
}

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

type Handler struct {
	keys     *KeyCache
	quota    QuotaChecker
	pipeline *Pipeline
	maxBytes int64

	// Процесс-локальный, не per-org: организация запроса неизвестна, пока
	// ключ не резолвился.
	keyRejected map[KeyRejectReason]*atomic.Int64

	// Отдельно от keyRejected: та детальна по ключу, эта — по всем причинам
	// отказа сразу, с меткой вида телеметрии.
	rejected map[IngestRejectionKey]*atomic.Int64

	// Считается до аутентификации и лимитера — сюда попадают и отказы 401/429
	// на устаревшем пути, не только успешные запросы.
	deprecated map[DeprecatedPath]*atomic.Int64

	// Один sync.Once на путь: предупреждение пишется раз за жизнь процесса, не
	// на каждый запрос.
	deprecatedLogged map[DeprecatedPath]*sync.Once

	// Отдельно от rejected_total (запрос там принят, не зарегистрирован лишь
	// хост) и от Toucher.RejectedNames (потолок хостов на проект).
	hostScopeSkipped atomic.Int64

	// Профиль принят (200/202), но декодер срезал часть по капу; по парсеру,
	// т.к. pprof и sentry режут независимо друг от друга.
	profileTruncated map[ProfileParser]*atomic.Int64

	// Общий для pprof и sentry-профилей — см. profile_trunc.go.
	profileDecodeBudget *profileDecodeBudget

	// Дешёвый per-DSN токен-бакет до quota-проверки; nil → лимит выключен.
	rate *rateLimiter[int64]

	// По client IP, до authenticate; nil → лимит выключен.
	preAuth *rateLimiter[string]

	// По IP, для touchUnverifiedSignal; много туже preAuth.
	signalTouch *rateLimiter[string]

	// Throttle лога overloaded: в отличие от rateLimited (бьёт один ключ),
	// overloaded срабатывает на каждый запрос, пока просажен общий буфер.
	overloadLogMu   sync.Mutex
	lastOverloadLog map[IngestSignal]time.Time

	// Квота транзакций отдельно от quota (квоты ошибок) — разные лимиты и
	// счётчики; nil → транзакции не квотируются.
	TxQuota QuotaChecker

	// nil → семплируем все транзакции (rate = 1).
	Projects ProjectSettings

	// nil → метрики выключены, эндпоинт отвечает успехом без записи.
	Metrics MetricSink
	// Отдельный счётчик квоты метрик; nil → не квотируются.
	MetricQuota QuotaChecker

	// nil → профили выключены, не пишутся.
	Profiles ProfileSink
	// nil → профили не квотируются.
	ProfileQuota QuotaChecker

	// Учёт отклонённых единиц по орге/месяцу, best-effort; nil → не считаем.
	DropCounter DropCounter

	// Зачистка ПДн атрибутов OTLP-метрик (152-ФЗ) — путь метрик идёт мимо
	// Pipeline.Scrubber. nil → выключен, методы nil-safe.
	Scrub *Scrubber

	// nil — ограничение выключено (методы nil-safe).
	Cardinality *CardinalityGuard

	// nil — приём работает без регистрации хостов (режимы без PG).
	Hosts HostRegistry

	// nil → логи выключены, эндпоинты отвечают успехом без записи.
	Logs LogSink
	// nil → логи не квотируются.
	LogQuota QuotaChecker

	// nil → приём деплоев выключен, эндпоинт отвечает 503.
	Deploy *deploy.Store

	// Per-project учёт сигналов приёма, отдельно от процесс-локальных
	// self-метрик. nil → сигналы не пишутся (методы nil-safe).
	Signals SignalRecorder
}

// Duck-typing интерфейс: пакет не должен знать способ агрегации сигнала,
// только факт «случился».
type SignalRecorder interface {
	Touch(projectID int64, kind ingestsignal.Kind)
}

// Вызывается и при отказе по квоте: семантика «приём принял экспорт», не
// «данные записаны».
type HostRegistry interface {
	Touch(ctx context.Context, projectID int64, entries []host.TouchEntry)
}

type DropCounter interface {
	IncDroppedEvents(ctx context.Context, orgID int64, month time.Time, n int64) error
	IncDroppedTransactions(ctx context.Context, orgID int64, month time.Time, n int64) error
	IncDroppedMetrics(ctx context.Context, orgID int64, month time.Time, n int64) error
	IncDroppedProfiles(ctx context.Context, orgID int64, month time.Time, n int64) error
	IncDroppedLogs(ctx context.Context, orgID int64, month time.Time, n int64) error
}

type MetricSink interface {
	Add(projectID int64, p metric.MetricPoint)
}

type ProfileSink interface {
	Add(projectID int64, p profile.Profile)
}

type LogSink interface {
	Add(projectID int64, r log.LogRecord)
}

func NewHandler(keys *KeyCache, quota QuotaChecker, pipeline *Pipeline, maxEventBytes int64) *Handler {
	return &Handler{
		keys:        keys,
		quota:       quota,
		pipeline:    pipeline,
		maxBytes:    maxEventBytes,
		rate:        newRateLimiter[int64](time.Now, defaultIngestRatePerSec, defaultIngestBurst),
		preAuth:     newRateLimiter[string](time.Now, defaultPreAuthRatePerSec, defaultPreAuthBurst),
		signalTouch: newRateLimiter[string](time.Now, defaultSignalTouchRatePerSec, defaultSignalTouchBurst),
		keyRejected: newKeyRejectCounters(),
		rejected:    newIngestRejectCounters(),

		lastOverloadLog: make(map[IngestSignal]time.Time),

		deprecated:       newDeprecatedCounters(),
		deprecatedLogged: newDeprecatedLogOnce(),

		profileTruncated:    newProfileTruncatedCounters(),
		profileDecodeBudget: newProfileDecodeBudget(),
	}
}

// path — r.URL.Path вызывающего эндпоинта: self-метрика не различает
// эндпоинты, лог — да.
func (h *Handler) countKeyReject(reason KeyRejectReason, path string) {
	if c, ok := h.keyRejected[reason]; ok {
		c.Add(1)
	}
	slog.Warn("ingest: key rejected", "reason", string(reason), "path", path)
}

func (h *Handler) touchSignal(projectID int64, kind ingestsignal.Kind) {
	if h.Signals != nil {
		h.Signals.Touch(projectID, kind)
	}
}

// projectID из URL не проверен — гасит перебор чужих project_id по IP.
func (h *Handler) touchUnverifiedSignal(r *http.Request, projectID int64, kind ingestsignal.Kind) {
	if h.signalTouch != nil && !h.signalTouch.Allow(preAuthClientIP(r)) {
		return
	}
	h.touchSignal(projectID, kind)
}

func (h *Handler) KeyRejectedBy(reason KeyRejectReason) int64 {
	if c, ok := h.keyRejected[reason]; ok {
		return c.Load()
	}
	return 0
}

// ratePerSec<=0 выключает лимит; now==nil — используется time.Now.
func (h *Handler) SetRateLimit(now func() time.Time, ratePerSec, burst float64) {
	if now == nil {
		now = time.Now
	}
	h.rate = newRateLimiter[int64](now, ratePerSec, burst)
}

// ratePerSec<=0 выключает лимит; now==nil — используется time.Now.
func (h *Handler) SetPreAuthRateLimit(now func() time.Time, ratePerSec, burst float64) {
	if now == nil {
		now = time.Now
	}
	h.preAuth = newRateLimiter[string](now, ratePerSec, burst)
}

// ratePerSec<=0 выключает лимит; now==nil — используется time.Now.
func (h *Handler) SetSignalTouchRateLimit(now func() time.Time, ratePerSec, burst float64) {
	if now == nil {
		now = time.Now
	}
	h.signalTouch = newRateLimiter[string](now, ratePerSec, burst)
}

// Окно — доли секунды, не месяц, как у квоты. Вызывается после аутентификации
// и до quota-проверки (дешевле её).
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

// Не настройка эксплуатации, а точка технического поведения буфера
// (drop-oldest/drop), с которой приём отвечает отказом вместо приёма-с-потерей.
const overloadThreshold = 0.95

// Отдельный интерфейс, не метод на самих синках: добавление в общий контракт
// сломало бы существующие тестовые двойники. Спрашивается через type-assert.
type saturationSource interface{ Saturation() float64 }

// 0 — для nil и для двойника без Saturation. Typed-nil тут не бывает:
// единственный источник — Pipeline.batcher, а его строит только NewPipeline.
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

// Вызывающий обязан звать до h.grant: иначе отказ спишет квоту за элемент,
// который всё равно выбросит буфер, и спишет её снова при ретрае клиента.
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

const overloadLogInterval = 5 * time.Second

// Троттлится только лог; gotcha_ingest_rejected_total растёт на каждый отказ независимо.
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

// Сужение ради сторожа маршрутов: http.ServeMux не даёт перечислить
// зарегистрированные паттерны, а подменный регистратор — даёт.
type muxRegistrar interface {
	HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request))
}

func (h *Handler) Register(mux muxRegistrar) {
	// OPTIONS не заворачиваем в preAuthGate: preflight в БД не ходит.

	// DSN (public key) не секрет — как у Sentry, разрешаем любой origin.
	mux.HandleFunc("POST /api/{project}/envelope/{$}", cors(h.preAuthGate(h.envelope)))
	mux.HandleFunc("OPTIONS /api/{project}/envelope/{$}", corsPreflight)
	mux.HandleFunc("POST /api/{project}/store/{$}", cors(h.preAuthGate(h.store)))
	mux.HandleFunc("OPTIONS /api/{project}/store/{$}", corsPreflight)
	// Второй вход в тот же пайплайн: своей квоты, модели и таблиц у него нет.
	mux.HandleFunc("POST /v1/traces", h.preAuthGate(h.otlpTraces))
	mux.HandleFunc("POST /v1/metrics", h.preAuthGate(h.otlpMetrics))
	// Свой минимальный эндпоинт (стандарта пуша pprof нет); канон — /api/v1/*,
	// /profiles/pprof — алиас до 2.0.
	mux.HandleFunc("POST /api/v1/profiles/pprof", h.preAuthGate(h.pprofIngest))
	mux.HandleFunc("POST /profiles/pprof", h.deprecatedAlias(DeprecatedProfilePprof, h.preAuthGate(h.pprofIngest)))
	// /v1/logs — OTLP-стандарт, не переезжает; /api/v1/logs — наш NDJSON-канон,
	// /logs — алиас до 2.0.
	mux.HandleFunc("POST /v1/logs", h.preAuthGate(h.otlpLogs))
	mux.HandleFunc("POST /api/v1/logs", h.preAuthGate(h.logsNDJSON))
	mux.HandleFunc("POST /logs", h.deprecatedAlias(DeprecatedLogs, h.preAuthGate(h.logsNDJSON)))
	// Server-to-server вход из CI, без CORS/preflight. Обе формы канона
	// регистрируются явно: CI-клиенты редиректы на POST не следуют.
	mux.HandleFunc("POST /api/v1/{project}/deployments", h.preAuthGate(h.deploymentsIngest))
	mux.HandleFunc("POST /api/v1/{project}/deployments/{$}", h.preAuthGate(h.deploymentsIngest))
	mux.HandleFunc("POST /api/{project}/deployments/{$}",
		h.deprecatedAlias(DeprecatedDeployments, h.preAuthGate(h.deploymentsIngest)))
}

// DSN публичен по замыслу — credentials не используются, поэтому
// wildcard-origin безопасен.
func corsHeaders(w http.ResponseWriter) {
	head := w.Header()
	head.Set("Access-Control-Allow-Origin", "*")
	head.Set("Access-Control-Allow-Methods", "POST, OPTIONS")
	head.Set("Access-Control-Allow-Headers", "content-type, x-sentry-auth, x-requested-with, baggage, sentry-trace")
	head.Set("Access-Control-Max-Age", "86400")
}

func cors(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		corsHeaders(w)
		next(w, r)
	}
}

func corsPreflight(w http.ResponseWriter, _ *http.Request) {
	corsHeaders(w)
	w.WriteHeader(http.StatusNoContent)
}

// За реверс-прокси это адрес прокси — GOTCHA_TRUSTED_PROXIES здесь не читается.
func preAuthClientIP(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// До authenticate/otlpAuthenticate на каждом маршруте приёма.
func (h *Handler) preAuthGate(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.preAuth != nil && !h.preAuth.Allow(preAuthClientIP(r)) {
			w.Header().Set("Retry-After", "1")
			writeJSONError(w, http.StatusTooManyRequests, "rate limit exceeded")
			return
		}
		next(w, r)
	}
}

// Считает отказ в обе self-метрики. 403, а не 401 (даже на OTLP): ключ
// резолвится успешно, просто не для этого эндпоинта.
func (h *Handler) scopeReject(w http.ResponseWriter, r *http.Request, signal IngestSignal, projectID int64) {
	h.countKeyReject(KeyRejectScope, r.URL.Path)
	h.countRejected(RejectKeyScope, signal)
	h.touchSignal(projectID, ingestsignal.KindKeyScope)
	writeJSONError(w, http.StatusForbidden, "key type not allowed for this endpoint")
}

// Квоты не проверяются здесь: их две, какую считать — видно только после
// разбора envelope'а. Гейт скоупа стоит после сверки проекта.
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
		h.touchUnverifiedSignal(r, projectID, ingestsignal.KindKeyInvalid)
		writeJSONError(w, http.StatusUnauthorized, "missing sentry_key")
		return org.Key{}, false
	}
	key, err := h.keys.Resolve(r.Context(), pub)
	switch {
	case errors.Is(err, org.ErrNotFound):
		h.countKeyReject(KeyRejectInvalidKey, r.URL.Path)
		h.countRejected(RejectKeyUnknown, signal)
		h.touchUnverifiedSignal(r, projectID, ingestsignal.KindKeyInvalid)
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

// Списывается за элемент, не за запрос; nil-квота/сбой → fail-open. chargedAt
// нужно пронести в парный refund без изменений, иначе списание и возврат разъедутся.
func (h *Handler) grant(ctx context.Context, q QuotaChecker, orgID int64, quotaKind string, want int) (int, time.Time) {
	if want <= 0 {
		return 0, time.Time{}
	}
	if q == nil {
		return want, time.Time{}
	}
	granted, chargedAt, err := q.CheckAndCount(ctx, orgID, int64(want))
	if err != nil {
		slog.Warn("ingest: quota check failed, allowing items",
			"org_id", orgID, "kind", quotaKind, "want", want, "error", err)
		return want, time.Time{}
	}
	return int(granted), chargedAt
}

type dropKind int

const (
	dropEvent dropKind = iota
	dropTransaction
	dropMetric
	dropProfile
	dropLog
)

// Best-effort: nil-счётчик или n<=0 — no-op, ошибка не влияет на ответ.
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

// chargedAt — то же значение, что вернул парный h.grant; пересчитывать нельзя,
// иначе списание и возврат разъедутся по месяцам на границе. Best-effort, как countDrop.
func (h *Handler) refund(ctx context.Context, q QuotaChecker, orgID int64, quotaKind string, n int, chargedAt time.Time) {
	if q == nil || n <= 0 {
		return
	}
	if err := q.Refund(ctx, orgID, int64(n), chargedAt); err != nil {
		slog.Warn("ingest: quota refund failed",
			"org_id", orgID, "kind", quotaKind, "n", n, "error", err)
	}
}

// Retry-After — до 1-го числа следующего месяца UTC, когда квота обнулится.
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

func noopClose() {}

// Функцию закрытия декомпрессора нужно звать defer'ом: zstd.Decoder держит
// фоновую горутину.
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
		// Лимиты декодера обязательны: окно объявляет клиент в заголовке фрейма, и
		// буфер под него аллоцируется до чтения тела — без ограничения удалённый OOM.
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

// В отличие от io.LimitReader не обрезает молча — при превышении отдаёт ErrTooLarge.
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

// Реальный pprof сжат одним слоем; несколько слоёв — «матрёшка»-бомба.
const maxGzipLayers = 3

// Разматывает ВСЕ слои: pp.ParseData сам разжимает один внутренний gzip без
// предела — без цикла двойной gzip обошёл бы лимит и дал OOM.
func gunzipLimited(raw []byte, limit int64) ([]byte, error) {
	for layer := 0; ; layer++ {
		if len(raw) < 2 || raw[0] != 0x1f || raw[1] != 0x8b {
			return raw, nil
		}
		if layer >= maxGzipLayers {
			return nil, ErrTooLarge
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
	// Класс отброшенных по лимиту не известен точно — списываем best-effort в
	// события, доминирующий класс приёма.
	if env.Dropped > 0 {
		slog.Warn("ingest: envelope item limit exceeded, extra items dropped",
			"limit", maxEnvelopeItems, "dropped", env.Dropped,
			"project_id", projectID, "org_id", key.OrgID)
		h.countDrop(r.Context(), dropEvent, key.OrgID, env.Dropped)
	}
	// Считается раз на запрос на каждый сигнал с отброшенным item'ом, не по
	// item — иначе key_scope стал бы несравним с соседними причинами на дашборде.
	for signal := range env.ScopeRejected {
		h.countRejected(RejectKeyScope, signal)
	}

	// 429 отдаём, только если по ВСЕМ присутствующим типам организация вышла
	// за квоту — иначе приняли бы 200 и молча выбросили половину envelope'а.
	hasEvents := len(env.Events) > 0
	// Отбор (разбор + семплирование) — до списания квоты: платить нужно за то,
	// что реально запишется, не за все разобранные транзакции.
	var txSelected []trace.Transaction
	if len(env.Transactions) > 0 && h.pipeline.TracingEnabled() {
		txSelected = h.sampleTransactions(r.Context(),
			projectID, h.parseTransactions(projectID, env.Transactions))
	}
	hasTx := len(txSelected) > 0
	hasProfiles := len(env.Profiles) > 0

	// «Присутствующий» — класс с элементами в этом envelope'е; hasTx уже после
	// отбора. Насыщенность считается один раз, до развилок ниже.
	eventSat := h.pipeline.EventSaturation()
	txSat := h.pipeline.TransactionSaturation()
	profSat := saturationOf(h.Profiles)
	eventOverloaded := hasEvents && eventSat >= overloadThreshold
	txOverloaded := hasTx && txSat >= overloadThreshold
	profOverloaded := hasProfiles && profSat >= overloadThreshold
	// Насыщены буферы всех присутствующих классов — целиком отказываем 503 до
	// списания квоты.
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
	// Насыщенный класс квоту не тратит и не участвует в решении «квота
	// исчерпана по всем типам» — у него отдельная причина отказа (overloaded).
	eventQuotaRelevant := hasEvents && !eventOverloaded
	txQuotaRelevant := hasTx && !txOverloaded
	// Профили: то же правило, плюс h.Profiles == nil исключает класс целиком.
	profQuotaRelevant := hasProfiles && h.Profiles != nil && !profOverloaded
	// Частичное списание: если до квоты осталось меньше, чем в конверте,
	// остаток — в дропы.
	var eventsGranted int
	var eventsChargedAt time.Time
	if eventQuotaRelevant {
		eventsGranted, eventsChargedAt = h.grant(r.Context(), h.quota, key.OrgID, "event", len(env.Events))
	}
	var txGranted int
	var txChargedAt time.Time
	if txQuotaRelevant {
		txGranted, txChargedAt = h.grant(r.Context(), h.TxQuota, key.OrgID, "transaction", len(txSelected))
	}
	// Считается наравне с eventsGranted/txGranted, до развилки 503: профиль —
	// такой же ретраибельный класс со своей квотой и буфером.
	var profGranted int
	if profQuotaRelevant {
		// chargedAt профилям не нужен: они вытесняются, а не отклоняются — refund не парен.
		profGranted, _ = h.grant(r.Context(), h.ProfileQuota, key.OrgID, "profile", len(env.Profiles))
	}
	eventsAllowed := eventsGranted > 0
	txAllowed := txGranted > 0
	profAllowed := profGranted > 0
	// Если часть классов насыщена, а другие честно исчерпали квоту —
	// выигрывает 503: у 429 Retry-After до следующего месяца.
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
	// Насыщенный класс тоже попадает в счёт: eventsGranted/txGranted у него 0,
	// разница корректно списывает все элементы в дропы.
	if dropped := len(env.Events) - eventsGranted; hasEvents && dropped > 0 {
		h.countDrop(r.Context(), dropEvent, key.OrgID, dropped)
	}
	// Уменьшаемое — отобранные, не пришедшие: иначе в потери попало бы
	// отсеянное семплированием.
	if dropped := len(txSelected) - txGranted; dropped > 0 {
		h.countDrop(r.Context(), dropTransaction, key.OrgID, dropped)
	}
	// Профили обрабатываются здесь, до развилки по квоте событий/транзакций:
	// у них своя квота и буфер, они не зависят от чужого лимита.
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
				release, ok := h.acquireProfileDecode(r.Context(), len(raw))
				if !ok {
					// Как profOverloaded рядом: элемент теряется, остальной envelope — как обычно.
					slog.Warn("ingest: dropping sentry profile, decode budget exhausted",
						"project_id", projectID, "org_id", key.OrgID)
					h.countDrop(r.Context(), dropProfile, key.OrgID, 1)
					continue
				}
				prof, err := profile.ParseSentry(raw, time.Now().UTC())
				release()
				if err != nil {
					slog.Warn("ingest: bad sentry profile, skipped", "project_id", projectID, "error", err)
					continue
				}
				if prof.Truncated {
					h.countProfileTruncated(ParserSentry)
				}
				h.scrubProfile(&prof)
				h.limitProfileCardinality(projectID, &prof)
				h.Profiles.Add(key.ProjectID, prof)
			}
		}
	} else if hasProfiles {
		// h.Profiles == nil — приём профилей выключен на этом узле: элементы
		// молча выброшены, но обязаны попасть в дропы, а не пройти незамеченными.
		h.countDrop(r.Context(), dropProfile, key.OrgID, len(env.Profiles))
	}
	// Конверт из одних профилей с исчерпанной квотой получает 429, не 200 —
	// та же честность, что для событий и транзакций.
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
	// Отвечаем 200, но выброшенный класс обязан быть виден в логах — иначе не
	// отличить «ошибок не было» от «ошибки молча выброшены».
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
	// eventsCapacityDropped считает только ёмкостные отказы (Enqueue вернул
	// false), не битые item'ы — иначе брак получал бы бесплатный возврат квоты.
	var eventsEnqueued int
	var eventsCapacityDropped int
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
			eventsCapacityDropped++
		}
	}
	var txEnqueued, txCapacityDropped int
	if txGranted > 0 {
		txEnqueued, txCapacityDropped = h.enqueueTransactions(projectID, key.OrgID, txSelected[:txGranted])
	}
	// Возвращается списанное, но не поставленное именно по ёмкости — наша
	// вина, не клиента.
	h.refund(r.Context(), h.quota, key.OrgID, "event", eventsCapacityDropped, eventsChargedAt)
	h.refund(r.Context(), h.TxQuota, key.OrgID, "transaction", txCapacityDropped, txChargedAt)
	// Причина именно ёмкость (не квота/скоуп/брак/семплирование) — повтор
	// клиента безопасен, профили сюда не входят: их приёмник вытесняет, а не отказывает.
	if eventsEnqueued == 0 && txEnqueued == 0 && (eventsCapacityDropped > 0 || txCapacityDropped > 0) {
		signal := SignalTransaction
		if eventsCapacityDropped > 0 {
			signal = SignalEvent
		}
		h.overloaded(w, key.OrgID, projectID, signal, 1.0)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": id})
}

// Имя транзакции полностью клиентское и часто несёт URL с ПДн — чистим. Имена
// кадров не трогаем: не ПДн, маскирование сломало бы схлопывание стеков.
func (h *Handler) scrubProfile(p *profile.Profile) {
	if h.Scrub == nil || p == nil {
		return
	}
	// ScrubMessage, не ScrubJSON: свободный текст; URL чистится всегда, вне ScrubFreeText.
	p.Transaction = h.Scrub.ScrubMessage(p.Transaction)
	p.Service = h.Scrub.ScrubMessage(p.Service)
	p.Environment = h.Scrub.ScrubMessage(p.Environment)
}

// service и profile_type — в ключе сортировки profile_samples, имя
// транзакции — от клиента целиком.
func (h *Handler) limitProfileCardinality(projectID int64, p *profile.Profile) {
	if h.Cardinality == nil || p == nil {
		return
	}
	p.Transaction = h.Cardinality.Value(projectID, FieldTransaction, p.Transaction)
	p.Service = h.Cardinality.Value(projectID, FieldService, p.Service)
	p.Environment = h.Cardinality.Value(projectID, FieldEnvironment, p.Environment)
}

// Битый item не валит весь конверт и не расходует квоту — до отбора он не доживает.
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

// Общая точка отбора для Sentry-envelope и OTLP; детерминированное по
// trace_id — все спаны трейса получают одно решение. Стоит выше списания квоты намеренно.
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

// capacityDropped — отклонено именно по ёмкости (EnqueueTransaction вернул
// false); вызывающие возвращают за это квоту и решают, честен ли ответ 200.
func (h *Handler) enqueueTransactions(projectID, orgID int64, txs []trace.Transaction) (enqueued, capacityDropped int) {
	for i := range txs {
		tx := txs[i]
		h.limitCardinality(projectID, &tx)
		if h.pipeline.EnqueueTransaction(projectID, orgID, tx) {
			enqueued++
		} else {
			capacityDropped++
		}
	}
	return enqueued, capacityDropped
}

// Эти поля — в ключах сортировки/GROUP BY ClickHouse: один идентификатор в
// имени транзакции взрывает кардинальность. Схлопываем, не отбрасываем.
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

// Сбой чтения настроек → fail-open (принимаем всё), как и сбой квоты.
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

// Легаси-эндпоинт: одно событие ошибки, транзакций не бывает — квота одна.
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
	projectID := key.ProjectID
	// Разбор — до списания квоты (тот же порядок, что у envelope): битое или
	// слишком большое тело не должно стоить клиенту квоты.
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
	eventGranted, eventChargedAt := h.grant(r.Context(), h.quota, key.OrgID, "event", 1)
	if eventGranted == 0 {
		h.countDrop(r.Context(), dropEvent, key.OrgID, 1)
		h.writeQuotaExceeded(w, SignalEvent, "event quota exceeded")
		return
	}
	// Единственное содержимое запроса не встало в очередь — честный 503, повтор
	// безопасен. saturation=1.0 — констатация уже случившегося отказа, не измерение.
	if !h.pipeline.Enqueue(projectID, key.OrgID, pe) {
		// Списанная единица не встала в очередь по ёмкости — не вина клиента, возвращаем квоту.
		h.refund(r.Context(), h.quota, key.OrgID, "event", 1, eventChargedAt)
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
