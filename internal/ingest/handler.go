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
	"time"

	"github.com/klauspost/compress/zstd"

	"gitflic.ru/otezvikentiy/gotcha/internal/host"
	"gitflic.ru/otezvikentiy/gotcha/internal/log"
	"gitflic.ru/otezvikentiy/gotcha/internal/metric"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/profile"
	"gitflic.ru/otezvikentiy/gotcha/internal/trace"
)

// Handler — HTTP-слой Sentry-протокола.
type Handler struct {
	keys     *KeyCache
	quota    QuotaChecker
	pipeline *Pipeline
	maxBytes int64

	// rate — дешёвый per-DSN (по project id) токен-бакет ПЕРЕД quota-проверкой:
	// срезает флуд с одного ключа до похода в PG (см. ratelimit.go). Задаётся в
	// NewHandler дефолтом; заменяем на nil/свой через SetRateLimit для тестов и
	// тонкой настройки. nil → лимит выключен.
	rate *rateLimiter

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
	// pprof из /profiles/pprof кладут распарсенные Profile сюда (*profile.Writer).
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

	// Logs — приёмник логов (C1): /v1/logs (OTLP) и /logs (NDJSON) кладут
	// распарсенные записи сюда (*log.Writer ему удовлетворяет). nil → логи
	// выключены, эндпоинты отвечают успехом без записи (как Metrics nil).
	Logs LogSink
	// LogQuota — квота ЛОГОВ (log_quota против org_usage.logs_count),
	// отдельный счётчик. nil → логи не квотируются.
	LogQuota QuotaChecker
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
		keys:     keys,
		quota:    quota,
		pipeline: pipeline,
		maxBytes: maxEventBytes,
		rate:     newRateLimiter(time.Now, defaultIngestRatePerSec, defaultIngestBurst),
	}
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
func (h *Handler) rateLimited(w http.ResponseWriter, orgID, projectID int64) bool {
	if h.rate == nil || h.rate.Allow(projectID) {
		return false
	}
	slog.Warn("ingest: per-DSN rate limit exceeded",
		"project_id", projectID, "org_id", orgID)
	w.Header().Set("Retry-After", "1")
	writeJSONError(w, http.StatusTooManyRequests, "rate limit exceeded")
	return true
}

func (h *Handler) Register(mux *http.ServeMux) {
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
	// нет), Bearer-DSN auth + метаданные из query.
	mux.HandleFunc("POST /profiles/pprof", h.pprofIngest)
	// Логи (C1) — OTLP-вход, четвёртая дверь в тот же ingest-mux (своя квота и
	// своя таблица logs, см. logs.go otlpLogs), и NDJSON-вход для источников без
	// OTLP-экспортёра (см. logsNDJSON).
	mux.HandleFunc("POST /v1/logs", h.otlpLogs)
	mux.HandleFunc("POST /logs", h.logsNDJSON)
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

// authenticate проверяет ключ проекта; при успехе возвращает ключ и true. При
// отказе сама пишет ошибку в w и возвращает false. Квоты здесь НЕ проверяются:
// их две (ошибки и транзакции), и какую списывать — видно только после
// разбора envelope'а.
func (h *Handler) authenticate(w http.ResponseWriter, r *http.Request) (org.Key, bool) {
	projectID, err := strconv.ParseInt(r.PathValue("project"), 10, 64)
	if err != nil {
		writeJSONError(w, http.StatusNotFound, "unknown project")
		return org.Key{}, false
	}
	pub := PublicKeyFromRequest(r)
	if pub == "" {
		writeJSONError(w, http.StatusUnauthorized, "missing sentry_key")
		return org.Key{}, false
	}
	key, err := h.keys.Resolve(r.Context(), pub)
	switch {
	case errors.Is(err, org.ErrNotFound):
		writeJSONError(w, http.StatusForbidden, "invalid sentry_key")
		return org.Key{}, false
	case err != nil:
		writeJSONError(w, http.StatusServiceUnavailable, "key lookup failed")
		return org.Key{}, false
	case key.ProjectID != projectID:
		writeJSONError(w, http.StatusForbidden, "sentry_key does not match project")
		return org.Key{}, false
	}

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
func (h *Handler) grant(ctx context.Context, q QuotaChecker, orgID int64, kind string, want int) int {
	if want <= 0 {
		return 0
	}
	if q == nil {
		return want
	}
	granted, err := q.CheckAndCount(ctx, orgID, int64(want))
	if err != nil {
		slog.Warn("ingest: quota check failed, allowing items",
			"org_id", orgID, "kind", kind, "want", want, "error", err)
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
// следующего месяца UTC, когда счётчик организации обнулится.
func writeQuotaExceeded(w http.ResponseWriter, detail string) {
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
	key, ok := h.authenticate(w, r)
	if !ok {
		return
	}
	projectID := key.ProjectID
	if h.rateLimited(w, key.OrgID, projectID) {
		return
	}
	body, closeBody, err := h.body(w, r)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "bad body encoding")
		return
	}
	defer closeBody()
	env, err := ParseEnvelope(body, h.maxBytes)
	if err != nil {
		status := http.StatusBadRequest
		var maxErr *http.MaxBytesError
		if errors.Is(err, ErrTooLarge) || errors.As(err, &maxErr) {
			status = http.StatusRequestEntityTooLarge
		}
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
	// Квота списывается ЗА ЭЛЕМЕНТ. Списание частичное: если до квоты осталось
	// меньше, чем в конверте, принимаем сколько влезло, остаток идёт в дропы —
	// организация получает ровно свою квоту, а не «последний конверт целиком
	// мимо», и org_usage остаётся точным.
	eventsGranted := h.grant(r.Context(), h.quota, key.OrgID, "event", len(env.Events))
	txGranted := h.grant(r.Context(), h.TxQuota, key.OrgID, "transaction", len(txSelected))
	eventsAllowed := eventsGranted > 0
	txAllowed := txGranted > 0
	// Учёт дропов до развилки ответа: отклонённое считаем и когда 429 по ВСЕМ
	// типам (ранний return ниже), и когда 200 по смешанному конверту, и когда
	// принята лишь часть.
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
	if (hasEvents || hasTx) && !eventsAllowed && !txAllowed {
		detail := "event quota exceeded"
		if !hasEvents {
			detail = "transaction quota exceeded"
		}
		writeQuotaExceeded(w, detail)
		return
	}
	// Смешанный envelope, где по ОДНОМУ классу квота исчерпана: отвечаем 200 (по
	// второму классу приняли), но выброшенный класс обязан быть виден в логах —
	// иначе оператор не отличит «ошибок не было» от «ошибки молча выброшены».
	if dropped := len(env.Events) - eventsGranted; hasEvents && dropped > 0 {
		slog.Warn("ingest: quota exceeded, dropping items from envelope",
			"class", "event", "dropped", dropped, "accepted", eventsGranted,
			"project_id", projectID, "org_id", key.OrgID)
	}
	if dropped := len(txSelected) - txGranted; dropped > 0 {
		slog.Warn("ingest: quota exceeded, dropping items from envelope",
			"class", "transaction", "dropped", dropped, "accepted", txGranted,
			"project_id", projectID, "org_id", key.OrgID)
	}

	id := env.EventID
	// Принимаем ровно столько, сколько списала квота: остальное уже посчитано
	// в дропы выше.
	for _, raw := range env.Events[:eventsGranted] {
		pe, err := ParseEvent(raw)
		if err != nil {
			continue // битый item не валит весь envelope
		}
		if id == "" {
			id = pe.EventID
		}
		pe.Environment = h.Cardinality.Value(projectID, FieldEnvironment, pe.Environment)
		h.pipeline.Enqueue(projectID, key.OrgID, pe)
	}
	if txGranted > 0 {
		h.enqueueTransactions(projectID, key.OrgID, txSelected[:txGranted])
	}
	// Профили (этап 7) — best-effort: своя квота, отдельная от событий/транзакций;
	// её исчерпание или битый профиль не меняют статус ответа по остальным типам.
	if len(env.Profiles) > 0 && h.Profiles != nil {
		profGranted := h.grant(r.Context(), h.ProfileQuota, key.OrgID, "profile", len(env.Profiles))
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
func (h *Handler) enqueueTransactions(projectID, orgID int64, txs []trace.Transaction) {
	for i := range txs {
		tx := txs[i]
		h.limitCardinality(projectID, &tx)
		h.pipeline.EnqueueTransaction(projectID, orgID, tx)
	}
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
	key, ok := h.authenticate(w, r)
	if !ok {
		return
	}
	if h.rateLimited(w, key.OrgID, key.ProjectID) {
		return
	}
	if h.grant(r.Context(), h.quota, key.OrgID, "event", 1) == 0 {
		h.countDrop(r.Context(), dropEvent, key.OrgID, 1)
		writeQuotaExceeded(w, "event quota exceeded")
		return
	}
	projectID := key.ProjectID
	body, closeBody, err := h.body(w, r)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "bad body encoding")
		return
	}
	defer closeBody()
	raw, err := io.ReadAll(body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.Is(err, ErrTooLarge) || errors.As(err, &maxErr) {
			writeJSONError(w, http.StatusRequestEntityTooLarge, "event too large")
			return
		}
		writeJSONError(w, http.StatusBadRequest, "bad body")
		return
	}
	pe, err := ParseEvent(raw)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "malformed event")
		return
	}
	h.pipeline.Enqueue(projectID, key.OrgID, pe)
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
