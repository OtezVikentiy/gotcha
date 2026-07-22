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
}

// DropCounter учитывает отклонённые единицы приёма по орге за текущий месяц.
// Реализация — *org.Service (методы IncDropped*). Сигнатуры совпадают с ним, так
// что сервис подставляется в поле напрямую.
type DropCounter interface {
	IncDroppedEvents(ctx context.Context, orgID int64, month time.Time, n int64) error
	IncDroppedTransactions(ctx context.Context, orgID int64, month time.Time, n int64) error
	IncDroppedMetrics(ctx context.Context, orgID int64, month time.Time, n int64) error
	IncDroppedProfiles(ctx context.Context, orgID int64, month time.Time, n int64) error
}

// MetricSink принимает распарсенную metric-точку. Реализация — *metric.Writer.
type MetricSink interface {
	Add(projectID int64, p metric.MetricPoint)
}

// ProfileSink принимает распарсенный профиль. Реализация — *profile.Writer.
type ProfileSink interface {
	Add(projectID int64, p profile.Profile)
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
	mux.HandleFunc("POST /api/{project}/envelope/{$}", h.envelope)
	mux.HandleFunc("POST /api/{project}/store/{$}", h.store)
	// OTLP — ВТОРОЙ ВХОД в тот же пайплайн (см. otlp.go): своей квоты, своей
	// модели и своих таблиц у него нет.
	mux.HandleFunc("POST /v1/traces", h.otlpTraces)
	// OTLP-метрики (этап 6) — третий вход в ingest: своя квота и своя таблица
	// metric_points (см. otlp.go otlpMetrics).
	mux.HandleFunc("POST /v1/metrics", h.otlpMetrics)
	// Профили pprof (этап 7): свой минимальный эндпоинт (стандарта пуша pprof
	// нет), Bearer-DSN auth + метаданные из query.
	mux.HandleFunc("POST /profiles/pprof", h.pprofIngest)
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

// allow списывает единицу из квоты q и говорит, принимать ли содержимое.
// nil-квота (не сконфигурирована) и сбой счётчика → fail-open: терять данные
// из-за сбоя квот хуже, чем иногда пропустить организацию сверх квоты.
func (h *Handler) allow(ctx context.Context, q QuotaChecker, orgID int64, kind string) bool {
	if q == nil {
		return true
	}
	allowed, err := q.CheckAndCount(ctx, orgID)
	if err != nil {
		slog.Warn("ingest: quota check failed, allowing item",
			"org_id", orgID, "kind", kind, "error", err)
		return true
	}
	return allowed
}

// dropKind — класс отклонённой единицы для countDrop.
type dropKind int

const (
	dropEvent dropKind = iota
	dropTransaction
	dropMetric
	dropProfile
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
		zr, err := zstd.NewReader(raw)
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
	hasTx := len(env.Transactions) > 0 && h.pipeline.TracingEnabled()
	eventsAllowed := hasEvents && h.allow(r.Context(), h.quota, key.OrgID, "event")
	txAllowed := hasTx && h.allow(r.Context(), h.TxQuota, key.OrgID, "transaction")
	// Учёт дропов до развилки ответа: отклонённый класс считаем и когда 429 по
	// ВСЕМ типам (ранний return ниже), и когда 200 по смешанному envelope'у.
	if hasEvents && !eventsAllowed {
		h.countDrop(r.Context(), dropEvent, key.OrgID, len(env.Events))
	}
	if hasTx && !txAllowed {
		h.countDrop(r.Context(), dropTransaction, key.OrgID, len(env.Transactions))
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
	if hasEvents && !eventsAllowed {
		slog.Warn("ingest: quota exceeded, dropping items from mixed envelope",
			"class", "event", "items", len(env.Events),
			"project_id", projectID, "org_id", key.OrgID)
	}
	if hasTx && !txAllowed {
		slog.Warn("ingest: quota exceeded, dropping items from mixed envelope",
			"class", "transaction", "items", len(env.Transactions),
			"project_id", projectID, "org_id", key.OrgID)
	}

	id := env.EventID
	if eventsAllowed {
		for _, raw := range env.Events {
			pe, err := ParseEvent(raw)
			if err != nil {
				continue // битый item не валит весь envelope
			}
			if id == "" {
				id = pe.EventID
			}
			h.pipeline.Enqueue(projectID, pe)
		}
	}
	if txAllowed {
		h.ingestTransactions(r.Context(), projectID, env.Transactions)
	}
	// Профили (этап 7) — best-effort: своя квота, отдельная от событий/транзакций;
	// её исчерпание или битый профиль не меняют статус ответа по остальным типам.
	if len(env.Profiles) > 0 && h.Profiles != nil {
		if h.allow(r.Context(), h.ProfileQuota, key.OrgID, "profile") {
			for _, raw := range env.Profiles {
				prof, err := profile.ParseSentry(raw, time.Now().UTC())
				if err != nil {
					slog.Warn("ingest: bad sentry profile, skipped", "project_id", projectID, "error", err)
					continue
				}
				h.Profiles.Add(key.ProjectID, prof)
			}
		} else {
			slog.Warn("ingest: profile quota exceeded, dropping profiles",
				"items", len(env.Profiles), "project_id", projectID, "org_id", key.OrgID)
			h.countDrop(r.Context(), dropProfile, key.OrgID, len(env.Profiles))
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": id})
}

// ingestTransactions разбирает transaction-item'ы, отбрасывает несемплированные
// трейсы и отдаёт остальное в пайплайн. Битый item не валит весь envelope.
func (h *Handler) ingestTransactions(ctx context.Context, projectID int64, items [][]byte) {
	rate := h.sampleRate(ctx, projectID)
	for _, raw := range items {
		tx, err := ParseTransaction(raw)
		if err != nil {
			slog.Debug("ingest: malformed transaction item, skipped",
				"project_id", projectID, "error", err)
			continue
		}
		h.enqueueSampled(projectID, rate, tx)
	}
}

// enqueueSampled — общая для ВСЕХ входов (Sentry-envelope и OTLP) точка отдачи
// транзакции в пайплайн: семплирование ДЕТЕРМИНИРОВАННОЕ по trace_id, так что
// все спаны одного трейса (в т.ч. приехавшие на другую реплику и из другого
// SDK) принимают одно и то же решение.
func (h *Handler) enqueueSampled(projectID int64, rate float64, tx trace.Transaction) {
	if !trace.Keep(tx.TraceID, rate) {
		return
	}
	h.pipeline.EnqueueTransaction(projectID, tx)
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
	if !h.allow(r.Context(), h.quota, key.OrgID, "event") {
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
	h.pipeline.Enqueue(projectID, pe)
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
