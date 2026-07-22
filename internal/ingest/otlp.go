package ingest

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"gitflic.ru/otezvikentiy/gotcha/internal/metric"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/trace"
)

// Атрибуты OTel-семантики, которые мы промотируем в поля нашей модели. Всё
// остальное едет в Span.Data как есть — своих полей не выдумываем.
const (
	attrServiceName    = "service.name"
	attrServiceVersion = "service.version"
	attrDeployEnv      = "deployment.environment"      // старая семконвенция
	attrDeployEnvName  = "deployment.environment.name" // текущая
	attrDBSystem       = "db.system"                   // старая семконвенция
	attrDBSystemName   = "db.system.name"              // текущая
	attrDBStatement    = "db.statement"                // старая семконвенция
	attrDBQueryText    = "db.query.text"               // текущая
	attrHTTPMethod     = "http.request.method"
	attrHTTPMethodOld  = "http.method"
	attrURLFull        = "url.full"
	attrURLFullOld     = "http.url"
	attrServerAddress  = "server.address"
	attrURLPath        = "url.path"
)

// attrMeasurementPrefix — ЕДИНСТВЕННЫЙ известный носитель web vitals в OTLP.
// Семантика measurements в OpenTelemetry не стандартизирована; Sentry-SDK кладёт
// их в атрибуты корневого спана с этим префиксом (sentry.measurements.lcp =
// 2480.0), имя vital'а — то, что после префикса. Других соглашений нет, поэтому и
// читаем только этот префикс.
const attrMeasurementPrefix = "sentry.measurements."

// maxDataValue — кап значения атрибута в Span.Data: та же дисциплина, что у
// description (см. capRunes), Data уезжает в JSON-колонку.
const maxDataValue = maxSpanDescription

// maxDataKeys — сколько атрибутов спана кладём в Span.Data. Тот же кап, что у
// тегов (см. capTags): Data целиком сериализуется в колонку `data`, и спан со
// 100k атрибутов не должен утаскивать их туда все. Выбор ключей
// детерминирован — первые maxDataKeys в отсортированном порядке.
const maxDataKeys = 64

// maxOTLPSpans — потолок числа спанов, разбираемых из одного OTLP-запроса
// /v1/traces. Без него недоверенный экспорт с сотнями тысяч спанов раздул бы
// плоский список flat и карту parents (амплификация CPU/памяти) в обход
// дисциплины maxEnvelopeItems Sentry-пути. Щедрый (10× maxSpans), но конечный.
// Датапойнты /v1/metrics каппит metric.MapOTLP своим maxOTLPMetricPoints.
const maxOTLPSpans = 10000

// maxParentHops — предел подъёма по цепочке parent_span_id при поиске корня
// спана (см. MapOTLP). Реальные трейсы столько не вкладывают; кап нужен, чтобы
// битый батч с циклом в parent_span_id не крутил нас вечно.
const maxParentHops = 64

// --- эндпойнт POST /v1/traces (OTLP/HTTP) ---

// otlpEncoding — кодировка OTLP-сообщения. Ответ ОБЯЗАН уехать в той же
// кодировке, в какой приехал запрос (protobuf → protobuf, json → json): OTLP/HTTP
// требует в ответ ExportTraceServiceResponse, и коллектор, не сумевший его
// разобрать, считает экспорт неуспешным и ретраит пачку бесконечно.
type otlpEncoding int

const (
	otlpProtobuf otlpEncoding = iota
	otlpJSON
)

// otlpTraces — приём трейсов от OpenTelemetry. Это ВТОРАЯ ДВЕРЬ в тот же
// пайплайн, а не вторая система: после MapOTLP это обычная trace.Transaction, и
// дальше она идёт по ровно тому же пути, что Sentry-транзакция — та же квота
// транзакций (h.TxQuota), то же детерминированное семплирование
// (enqueueSampled → trace.Keep) и тот же SpanWriter.
func (h *Handler) otlpTraces(w http.ResponseWriter, r *http.Request) {
	key, ok := h.otlpAuthenticate(w, r)
	if !ok {
		return
	}
	enc, ok := otlpEncodingOf(r.Header.Get("Content-Type"))
	if !ok {
		writeJSONError(w, http.StatusUnsupportedMediaType, "unsupported content-type")
		return
	}
	// Трейсинг выключен — писать некуда. Отвечаем успехом (иначе коллектор
	// будет ретраить вечно) и не тратим квоту: как и envelope-путь, который до
	// квоты транзакций смотрит на TracingEnabled.
	if !h.pipeline.TracingEnabled() {
		writeOTLPResponse(w, enc)
		return
	}
	if h.rateLimited(w, key.OrgID, key.ProjectID) {
		return
	}
	if !h.allow(r.Context(), h.TxQuota, key.OrgID, "transaction") {
		// Тело ещё не разобрано — точного числа транзакций в экспорте нет;
		// считаем один отклонённый батч (best-effort сигнал о потерях).
		h.countDrop(r.Context(), dropTransaction, key.OrgID, 1)
		writeQuotaExceeded(w, "transaction quota exceeded")
		return
	}

	// Лимит тела и распаковка — общий Handler.body: коллектор по умолчанию жмёт
	// gzip'ом, и защита от «бомбы» здесь ровно та же, что у Sentry-входа.
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
			writeJSONError(w, http.StatusRequestEntityTooLarge, "export too large")
			return
		}
		writeJSONError(w, http.StatusBadRequest, "bad body")
		return
	}
	var req tracepb.TracesData
	if err := otlpUnmarshal(enc, raw, &req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "malformed otlp payload")
		return
	}

	projectID := key.ProjectID
	rate := h.sampleRate(r.Context(), projectID)
	for _, tx := range MapOTLP(req.GetResourceSpans(), time.Now().UTC()) {
		h.enqueueSampled(projectID, rate, tx)
	}
	writeOTLPResponse(w, enc)
}

// otlpMetrics — приём OTLP-метрик (этап 6). Каркас копирует otlpTraces
// (Bearer-DSN auth, квота, лимит тела, proto+JSON), но без sampling/TracingEnabled:
// метрики не семплируются и не зависят от флага трейсинга. Метрики выключены
// (h.Metrics == nil) → отвечаем успехом без записи (коллектор не ретраит вечно).
func (h *Handler) otlpMetrics(w http.ResponseWriter, r *http.Request) {
	key, ok := h.otlpAuthenticate(w, r)
	if !ok {
		return
	}
	enc, ok := otlpEncodingOf(r.Header.Get("Content-Type"))
	if !ok {
		writeJSONError(w, http.StatusUnsupportedMediaType, "unsupported content-type")
		return
	}
	if h.Metrics == nil {
		writeOTLPResponse(w, enc)
		return
	}
	if h.rateLimited(w, key.OrgID, key.ProjectID) {
		return
	}
	if !h.allow(r.Context(), h.MetricQuota, key.OrgID, "metric") {
		// Тело ещё не разобрано — считаем один отклонённый батч метрик.
		h.countDrop(r.Context(), dropMetric, key.OrgID, 1)
		writeQuotaExceeded(w, "metric quota exceeded")
		return
	}
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
			writeJSONError(w, http.StatusRequestEntityTooLarge, "export too large")
			return
		}
		writeJSONError(w, http.StatusBadRequest, "bad body")
		return
	}
	var req metricspb.MetricsData
	if err := otlpUnmarshalMetrics(enc, raw, &req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "malformed otlp payload")
		return
	}
	for _, p := range metric.MapOTLP(req.GetResourceMetrics(), time.Now().UTC()) {
		// Зачистка ПДн: атрибуты датапойнта — единственный носитель denylist-
		// значений на этом пути, и он идёт мимо Pipeline/Scrubber. h.Scrub == nil
		// — no-op (ScrubTags nil-safe). p.Attributes — map[string]string.
		h.Scrub.ScrubTags(p.Attributes)
		h.Metrics.Add(key.ProjectID, p)
	}
	writeOTLPResponse(w, enc)
}

// otlpUnmarshalMetrics — разбор тела метрик. В отличие от otlpUnmarshal (трейсы),
// hex-id переписывание не нужно: у метрик нет верхнеуровневых байтовых
// идентификаторов, которые мы читаем (exemplars с trace/span id мы игнорируем).
func otlpUnmarshalMetrics(enc otlpEncoding, raw []byte, req *metricspb.MetricsData) error {
	if enc == otlpJSON {
		return protojson.UnmarshalOptions{DiscardUnknown: true}.Unmarshal(raw, req)
	}
	return proto.Unmarshal(raw, req)
}

// otlpAuthenticate резолвит публичный ключ DSN из `Authorization: Bearer <key>`
// через тот же KeyCache, что и Sentry-вход (у OTel-экспортёров есть штатная
// опция headers:, своего формата мы не изобретаем). Проект берётся ИЗ КЛЮЧА: в
// OTLP-протоколе нет места для него в URL. Нет заголовка / неизвестный ключ →
// 401 (у envelope там 403 — там ключ уже сопоставляется с проектом из пути).
func (h *Handler) otlpAuthenticate(w http.ResponseWriter, r *http.Request) (org.Key, bool) {
	pub := otlpBearer(r)
	if pub == "" {
		writeJSONError(w, http.StatusUnauthorized, "missing bearer token")
		return org.Key{}, false
	}
	key, err := h.keys.Resolve(r.Context(), pub)
	switch {
	case errors.Is(err, org.ErrNotFound):
		writeJSONError(w, http.StatusUnauthorized, "invalid dsn key")
		return org.Key{}, false
	case err != nil:
		writeJSONError(w, http.StatusServiceUnavailable, "key lookup failed")
		return org.Key{}, false
	}
	return key, true
}

// otlpBearer достаёт токен из Authorization. Схема сравнивается без учёта
// регистра (RFC 7235), значение — как есть.
func otlpBearer(r *http.Request) string {
	scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return strings.TrimSpace(token)
}

// otlpEncodingOf разбирает Content-Type запроса. application/x-protobuf — то,
// что шлёт OTel-экспортёр по умолчанию; application/protobuf разрешён спекой как
// синоним; application/json — OTLP/JSON.
func otlpEncodingOf(contentType string) (otlpEncoding, bool) {
	mediaType, _, _ := strings.Cut(contentType, ";")
	switch strings.ToLower(strings.TrimSpace(mediaType)) {
	case "application/x-protobuf", "application/protobuf":
		return otlpProtobuf, true
	case "application/json":
		return otlpJSON, true
	}
	return 0, false
}

// otlpUnmarshal разбирает тело в соответствии с кодировкой. OTLP/JSON — это
// protojson поверх ТЕХ ЖЕ сообщений, а не «просто JSON»: encoding/json тут
// молча выдал бы мусор (другие имена полей, base64-кодирование байтовых id).
// DiscardUnknown: новое поле в свежей версии OTLP-спеки не должна отбивать
// весь батч.
//
// Разбираем в TracesData, а НЕ в ExportTraceServiceRequest коллекторного пакета:
// на проводе это одно и то же сообщение — `repeated ResourceSpans resource_spans
// = 1` и там, и там (в JSON — то же имя `resourceSpans`), так что байты
// настоящего коллектора читаются один в один. Разница только в том, что
// сгенерированный код коллекторного пакета тащит за собой gRPC и grpc-gateway,
// а gRPC-сервера у нас нет и не будет.
//
// ВАЖНО: перед protojson тело OTLP/JSON проходит через otlpJSONHexIDs. Спека
// OTLP отступает от стандартного protobuf-JSON ровно в одном месте — в
// байтовых идентификаторах: trace_id/span_id/parent_span_id в OTLP/JSON это
// HEX-СТРОКИ, а не base64. protojson реализует стандартный маппинг и молча
// декодирует их как base64 — и НЕ падает: любой hex-символ входит в
// base64-алфавит, а 32/16 символов кратны 4. Получаются 24/12 байт мусора,
// которые мы потом честно кодируем в hex и каппим до нужной ДЛИНЫ — id
// правильной формы и полностью неверного значения. Именно поэтому у самого
// коллектора в Go свой анмаршалер, а не protojson.
func otlpUnmarshal(enc otlpEncoding, raw []byte, req *tracepb.TracesData) error {
	if enc == otlpJSON {
		return protojson.UnmarshalOptions{DiscardUnknown: true}.Unmarshal(otlpJSONHexIDs(raw), req)
	}
	return proto.Unmarshal(raw, req)
}

// otlpJSONHexIDs переписывает в сыром OTLP/JSON идентификаторы из hex в base64,
// чтобы стандартный protobuf-JSON-декодер (protojson) получил то, что ожидает.
//
// Правило намеренно ТЕРПИМОЕ: конвертируется только значение правильной длины
// (32 hex-символа у trace_id, 16 у span_id/parent_span_id) и состоящее только
// из hex-цифр. Всё остальное отдаётся protojson как есть — значит, клиент,
// шлющий id в base64 (как это делает protojson.Marshal), продолжает работать:
// неоднозначности здесь нет, base64 16 байт — это 24 символа, 8 байт — 12, ни
// то, ни другое не совпадает с 32/16.
//
// Тело уже ограничено по размеру (см. Handler.body), поэтому обход разобранного
// JSON допустим; глубина рекурсии ограничена maxJSONIDDepth, а числа читаются
// как json.Number — иначе большие наносекундные таймстемпы уехали бы через
// float64 и вернулись в JSON в экспоненциальной записи, которую protojson не
// примет. Любая ошибка разбора — не наша забота: возвращаем тело нетронутым и
// даём protojson отчитаться об ошибке самому.
func otlpJSONHexIDs(raw []byte) []byte {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return raw
	}
	if !rewriteOTLPIDs(doc, 0) {
		return raw // hex-идентификаторов не было: тело уже в base64
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return raw
	}
	return out
}

// maxJSONIDDepth — предел глубины обхода в otlpJSONHexIDs. Идентификаторы живут
// на глубине ~6 (resourceSpans → scopeSpans → spans → links); всё, что глубже, —
// вложенные атрибуты, и переписывать там нечего. Обход глубже просто
// прекращается, данные при этом не теряются.
const maxJSONIDDepth = 64

// otlpJSONIDLen — длины hex-идентификаторов OTLP/JSON по именам полей (оба
// написания: protojson принимает и lowerCamelCase, и оригинальное snake_case).
var otlpJSONIDLen = map[string]int{
	"traceId":        maxTraceID,
	"trace_id":       maxTraceID,
	"spanId":         maxSpanID,
	"span_id":        maxSpanID,
	"parentSpanId":   maxSpanID,
	"parent_span_id": maxSpanID,
}

// rewriteOTLPIDs — обход разобранного JSON; возвращает true, если что-то
// переписал (тогда тело придётся собрать заново).
func rewriteOTLPIDs(v any, depth int) bool {
	if depth > maxJSONIDDepth {
		return false
	}
	changed := false
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			if n, ok := otlpJSONIDLen[k]; ok {
				if s, ok := val.(string); ok {
					if b64, ok := hexIDToBase64(s, n); ok {
						x[k] = b64
						changed = true
						continue
					}
				}
			}
			if rewriteOTLPIDs(val, depth+1) {
				changed = true
			}
		}
	case []any:
		for _, e := range x {
			if rewriteOTLPIDs(e, depth+1) {
				changed = true
			}
		}
	}
	return changed
}

// hexIDToBase64 переводит hex-идентификатор ровно длины n в base64. ok=false —
// это не hex нужной длины (пустая строка, base64, мусор): трогать не надо.
func hexIDToBase64(s string, n int) (string, bool) {
	if len(s) != n {
		return "", false
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return "", false
	}
	return base64.StdEncoding.EncodeToString(b), true
}

// otlpEmptyJSON — пустой ExportTraceServiceResponse в OTLP/JSON.
const otlpEmptyJSON = "{}"

// writeOTLPResponse отдаёт пустой (полностью успешный) ExportTraceServiceResponse
// в кодировке запроса. partial_success не заполняем: спека OTLP требует поле
// только для ЧАСТИЧНО отвергнутых батчей, а спаны, потерянные семплированием,
// отвергнутыми не являются — это осознанная политика приёма, и коллектору
// перепосылать их незачем.
//
// А раз partial_success не заполняется, сообщение пустое — и пишем мы его
// напрямую: пустое protobuf-сообщение это ПУСТОЕ тело, в JSON — `{}`. Городить
// ради двух этих констант зависимость от коллекторного пакета (а с ним от gRPC)
// незачем. Content-Type при этом обязан зеркалить запрос: коллектор, не сумевший
// разобрать ответ, считает экспорт неуспешным и ретраит пачку бесконечно.
func writeOTLPResponse(w http.ResponseWriter, enc otlpEncoding) {
	body, mediaTy := []byte(nil), "application/x-protobuf"
	if enc == otlpJSON {
		body, mediaTy = []byte(otlpEmptyJSON), "application/json"
	}
	w.Header().Set("Content-Type", mediaTy)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// MapOTLP превращает ResourceSpans OTLP-запроса в транзакции нашей модели.
//
// Транзакция = КОРНЕВОЙ спан: без parent_span_id ЛИБО kind=SERVER/CONSUMER
// (входящий запрос/сообщение — начало работы в этом сервисе, даже если у него
// есть удалённый родитель). Остальные спаны привязываются к СВОЕМУ корню —
// подъёмом по цепочке parent_span_id до ближайшего корня в этом же батче, а не
// к первому корню трейса: у одного trace_id корней может быть НЕСКОЛЬКО.
// Именно так и работает штатная поставка — batch-процессор коллектора СКЛЕИВАЕТ
// ResourceSpans разных сервисов в один экспорт, и трейс, прошедший через два
// сервиса, приезжает с двумя SERVER-корнями. Привязка к первому означала бы, что
// DB-спан биллинга уедет в транзакцию фронтенда (SpanWriter копирует в строку
// спана transaction и environment ВЛАДЕЮЩЕЙ транзакции) — и фильтр по
// окружению начал бы врать.
//
// Спаны, чей корень НЕ приехал в этом же запросе, отбрасываются: OTel-коллектор
// шлёт трейс батчами, и часть спанов законно приезжает без корня. Собирать
// трейс между запросами мы не будем — это ровно то межзапросное состояние,
// которого мы избегаем; потеря здесь best-effort и осознанная.
//
// Идентификаторы в OTLP — СЫРЫЕ БАЙТЫ; кодируем их в канонический lowercase hex
// (hex.EncodeToString), как в Sentry-парсере (см. normalizeID): и семплирование
// (trace.Keep), и join spans↔transactions завязаны на один вид id, иначе один и
// тот же трейс, увиденный двумя SDK, развалится надвое.
//
// Таймстемпы прогоняются через то же окно [now-90d, now+1d], что и Sentry-путь
// (см. timestamp.go): корень вне окна → транзакции нет вовсе, отдельный спан вне
// окна → выкидывается из tx.Spans. Пачка timestamp'ов из разных месяцев кладёт
// запись CH для всего инстанса (Too many partitions), и второй вход не должен
// открывать эту дверь заново.
func MapOTLP(rs []*tracepb.ResourceSpans, now time.Time) []trace.Transaction {
	// Плоский список спанов запроса: детей нельзя разложить, пока не известны
	// все корни (коллектор не гарантирует порядок).
	type flatSpan struct {
		res      otlpResource
		span     *tracepb.Span
		traceID  string
		spanID   string
		parentID string
	}
	var flat []flatSpan
	// parents: (trace_id, span_id) → parent_span_id по всем спанам батча. Нужна,
	// чтобы поднять ребёнка до ближайшего корня через промежуточные спаны.
	parents := make(map[string]string, 8)
otlpSpans:
	for _, r := range rs {
		if r == nil {
			continue
		}
		res := mapOTLPResource(r.GetResource())
		for _, ss := range r.GetScopeSpans() {
			for _, s := range ss.GetSpans() {
				if len(flat) >= maxOTLPSpans {
					break otlpSpans // потолок спанов на запрос — лишнее отбрасываем
				}
				if s == nil {
					continue
				}
				traceID := otlpID(s.GetTraceId(), maxTraceID)
				if traceID == "" {
					continue // пустой/нулевой trace_id: связать не с чем
				}
				spanID := otlpID(s.GetSpanId(), maxSpanID)
				if spanID == "" {
					continue // пустой/нулевой span_id невалиден так же, как trace_id
				}
				parentID := otlpID(s.GetParentSpanId(), maxSpanID)
				flat = append(flat, flatSpan{
					res: res, span: s, traceID: traceID, spanID: spanID, parentID: parentID,
				})
				parents[otlpSpanKey(traceID, spanID)] = parentID
			}
		}
	}

	txs := make([]trace.Transaction, 0, 4)
	// rootOf: (trace_id, span_id) КОРНЯ → индекс его транзакции.
	// firstRoot: trace_id → индекс первого корня трейса; запасной вариант для
	// спанов, чья цепочка родителей уходит за пределы батча.
	rootOf := make(map[string]int, 4)
	firstRoot := make(map[string]int, 4)

	for _, e := range flat {
		if !otlpIsRoot(e.span) {
			continue
		}
		start, end, ok := otlpSpanTimes(e.span, now)
		if !ok {
			continue // вне окна хранения → транзакции нет (см. ErrTimestampOutOfWindow)
		}
		tx := trace.Transaction{
			TraceID:      e.traceID,
			SpanID:       e.spanID,
			Name:         capRunes(e.span.GetName(), maxTransactionName),
			Op:           otlpOp(e.span),
			Status:       otlpStatus(e.span.GetStatus()),
			Start:        start,
			End:          end,
			Environment:  capRunes(e.res.environment, 200),
			Release:      capRunes(e.res.release, 200),
			ServerName:   capRunes(e.res.service, 200),
			Tags:         capTags(otlpTags(e.span.GetAttributes())),
			Spans:        make([]trace.Span, 0, 4),
			Source:       "otlp",
			Measurements: otlpMeasurements(e.span.GetAttributes()),
		}
		rootOf[otlpSpanKey(e.traceID, e.spanID)] = len(txs)
		if _, seen := firstRoot[e.traceID]; !seen {
			firstRoot[e.traceID] = len(txs)
		}
		txs = append(txs, tx)
	}

	for _, e := range flat {
		if otlpIsRoot(e.span) {
			continue
		}
		i, ok := otlpOwningRoot(e.traceID, e.parentID, parents, rootOf, firstRoot)
		if !ok {
			continue // сирота: корень не приехал в этом запросе
		}
		if len(txs[i].Spans) >= maxSpans {
			continue // раздутый трейс: лишние спаны отбрасываем, транзакцию оставляем
		}
		start, end, ok := otlpSpanTimes(e.span, now)
		if !ok {
			continue // спан-«отравитель» по партициям: см. ParseTransaction
		}
		op := otlpOp(e.span)
		txs[i].Spans = append(txs[i].Spans, trace.Span{
			SpanID:       e.spanID,
			ParentSpanID: e.parentID,
			Op:           op,
			Description:  capRunes(otlpDescription(e.span, op), maxSpanDescription),
			Start:        start,
			End:          end,
			Status:       otlpStatus(e.span.GetStatus()),
			Data:         otlpData(e.span),
		})
	}
	return txs
}

// otlpSpanKey — ключ спана в пределах батча. span_id уникален только внутри
// трейса, поэтому в ключ входит и trace_id.
func otlpSpanKey(traceID, spanID string) string { return traceID + "\x00" + spanID }

// otlpOwningRoot ищет транзакцию, которой принадлежит спан: поднимается по
// parent_span_id до ближайшего КОРНЯ этого же батча. Подъём ограничен
// maxParentHops — это же обезвреживает цикл в parent_span_id (битый батч).
//
// Цепочка ушла за пределы батча (родитель не приехал, оборвалась на
// отброшенном спане, упёрлась в кап) → запасной вариант: первый корень трейса.
// Корня у трейса нет вовсе → ok=false, спан-сирота отбрасывается.
func otlpOwningRoot(traceID, parentID string, parents map[string]string, rootOf, firstRoot map[string]int) (int, bool) {
	for id, hop := parentID, 0; id != "" && hop < maxParentHops; hop++ {
		if i, ok := rootOf[otlpSpanKey(traceID, id)]; ok {
			return i, true
		}
		id = parents[otlpSpanKey(traceID, id)]
	}
	i, ok := firstRoot[traceID]
	return i, ok
}

// otlpResource — resource-атрибуты, которые мы кладём в поля транзакции.
type otlpResource struct {
	service     string
	environment string
	release     string
}

func mapOTLPResource(r *resourcepb.Resource) otlpResource {
	var out otlpResource
	for _, kv := range r.GetAttributes() {
		v := otlpAttrString(kv.GetValue())
		switch kv.GetKey() {
		case attrServiceName:
			out.service = v
		case attrDeployEnvName:
			out.environment = v
		case attrDeployEnv:
			if out.environment == "" {
				out.environment = v
			}
		case attrServiceVersion:
			out.release = v
		}
	}
	return out
}

// otlpIsRoot — корневой ли спан. SERVER/CONSUMER считаем корнем даже с
// parent_span_id: их родитель живёт в ДРУГОМ сервисе, и в нашей модели входящий
// запрос — это начало транзакции.
func otlpIsRoot(s *tracepb.Span) bool {
	if otlpID(s.GetParentSpanId(), maxSpanID) == "" {
		return true
	}
	kind := s.GetKind()
	return kind == tracepb.Span_SPAN_KIND_SERVER || kind == tracepb.Span_SPAN_KIND_CONSUMER
}

// otlpID кодирует сырые байты id в канонический lowercase hex. Пустой или
// нулевой (все байты 0) id — невалиден по спеке OTLP, возвращаем "".
func otlpID(b []byte, n int) string {
	if len(b) == 0 {
		return ""
	}
	zero := true
	for _, c := range b {
		if c != 0 {
			zero = false
			break
		}
	}
	if zero {
		return ""
	}
	return capRunes(hex.EncodeToString(b), n)
}

// otlpSpanTimes переводит наносекунды OTLP в time.Time и проверяет окно
// хранения. ok=false → спан (или транзакция целиком) отбрасывается.
func otlpSpanTimes(s *tracepb.Span, now time.Time) (start, end time.Time, ok bool) {
	start, ok = otlpTime(s.GetStartTimeUnixNano())
	if !ok {
		return time.Time{}, time.Time{}, false
	}
	if !inRetentionWindow(start, now) {
		return time.Time{}, time.Time{}, false
	}
	end, ok = otlpTime(s.GetEndTimeUnixNano())
	if !ok {
		// Конца нет — длительность будет нулевой, а не отрицательной
		// (см. trace.Transaction.DurationUS).
		end = start
	}
	return start, end, true
}

// otlpTime — uint64 наносекунд от эпохи. 0 (поле не заполнено) и значения, не
// влезающие в int64, — не время, а мусор.
func otlpTime(ns uint64) (time.Time, bool) {
	if ns == 0 || ns > math.MaxInt64 {
		return time.Time{}, false
	}
	return time.Unix(0, int64(ns)).UTC(), true
}

// otlpStatus — статус OTLP в наш словарь: всё, кроме ERROR (в т.ч. UNSET, его
// шлёт подавляющее большинство спанов), — это 'ok'. Пустой статус нельзя
// оставлять как есть: MV transactions_5m считает провалом всё != 'ok', и failure
// rate стал бы 100% (та же нормализация, что в transactionStatus).
func otlpStatus(st *tracepb.Status) string {
	if st.GetCode() == tracepb.Status_STATUS_CODE_ERROR {
		return "internal_error"
	}
	return "ok"
}

// otlpOp — op спана по атрибутам и kind (спека §2): db → http.client →
// http.server → сам kind в нижнем регистре.
func otlpOp(s *tracepb.Span) string {
	attrs := s.GetAttributes()
	kind := s.GetKind()
	switch {
	case otlpDBSystem(attrs) != "":
		return otlpDBOp(otlpDBSystem(attrs))
	case kind == tracepb.Span_SPAN_KIND_CLIENT && otlpHTTPMethod(attrs) != "":
		return "http.client"
	case kind == tracepb.Span_SPAN_KIND_SERVER:
		return "http.server"
	}
	return capRunes(otlpKindName(kind), maxOp)
}

// otlpDBSystem — СУБД спана: db.system (старая семконвенция) или db.system.name
// (текущая — её и шлют SDK на свежем OTel). Читать только старый ключ нельзя:
// спан современного SDK не получил бы db-шный op (упал бы в `client` по kind), а
// значит стал бы НЕВИДИМ для всех детекторов сразу — hasOpPrefix(op, "db") ложен
// и для N+1, и для медленных запросов. Тот же разъезд старого и нового ключа уже
// учтён у текста запроса (db.statement / db.query.text, см. otlpDescription).
func otlpDBSystem(attrs []*commonpb.KeyValue) string {
	if v := otlpAttr(attrs, attrDBSystem); v != "" {
		return v
	}
	return otlpAttr(attrs, attrDBSystemName)
}

// otlpDBOp — op спана с db.system. Ключ-значение хранилища получают СВОЙ op, а
// не общий `db`: по op'у trace.NormalizeDescription выбирает нормализатор, и
// `db` означает «описание — это SQL». Прогнать через SQL-нормализатор команду
// Redis нельзя: `GET user:42` там читается как именованный плейсхолдер `:42` и
// сохраняется как есть, поэтому цикл из восьми промахов кеша (восемь РАЗНЫХ
// ключей) не схлопывался бы в одну форму — канонический Redis-N+1 через OTLP был
// бы невидим, хотя тот же цикл через Sentry SDK (op=db.redis) детектится.
//
// Значения — из семконвенции OTel (db.system.name): redis, valkey (тот же
// протокол), memcached. Всё прочее (postgresql, mysql, mssql, oracle, ...) —
// SQL, и остаётся `db`.
func otlpDBOp(system string) string {
	switch strings.ToLower(strings.TrimSpace(system)) {
	case "redis", "valkey":
		return "db.redis"
	case "memcached":
		return "db.memcached"
	}
	return "db"
}

// otlpKindName — kind в нижнем регистре. UNSPECIFIED по спеке OTLP разрешено
// трактовать как INTERNAL, «unspecified» в LowCardinality-колонке не нужен.
func otlpKindName(kind tracepb.Span_SpanKind) string {
	switch kind {
	case tracepb.Span_SPAN_KIND_CLIENT:
		return "client"
	case tracepb.Span_SPAN_KIND_SERVER:
		return "server"
	case tracepb.Span_SPAN_KIND_PRODUCER:
		return "producer"
	case tracepb.Span_SPAN_KIND_CONSUMER:
		return "consumer"
	default:
		return "internal"
	}
}

// otlpDescription — описание спана: запрос/команда для любого db-спана (включая
// db.redis и db.memcached — там в db.statement лежит команда с ключом),
// «METHOD URL» для исходящего HTTP, имя спана во всех прочих случаях.
func otlpDescription(s *tracepb.Span, op string) string {
	attrs := s.GetAttributes()
	switch {
	case op == "db" || strings.HasPrefix(op, "db."):
		if q := otlpAttr(attrs, attrDBStatement); q != "" {
			return q
		}
		if q := otlpAttr(attrs, attrDBQueryText); q != "" {
			return q
		}
	case op == "http.client":
		if url := otlpHTTPURL(attrs); url != "" {
			return strings.TrimSpace(otlpHTTPMethod(attrs) + " " + url)
		}
	}
	return s.GetName()
}

func otlpHTTPMethod(attrs []*commonpb.KeyValue) string {
	if m := otlpAttr(attrs, attrHTTPMethod); m != "" {
		return m
	}
	return otlpAttr(attrs, attrHTTPMethodOld)
}

// otlpHTTPURL — URL исходящего запроса. Без url.full собираем его из
// server.address + url.path, следя за разделителем: url.path по семконвенции
// начинается со слэша, но встречается и без него, и склейка «в лоб» дала бы
// `api.internalv2/users`.
func otlpHTTPURL(attrs []*commonpb.KeyValue) string {
	if u := otlpAttr(attrs, attrURLFull); u != "" {
		return u
	}
	if u := otlpAttr(attrs, attrURLFullOld); u != "" {
		return u
	}
	addr, path := otlpAttr(attrs, attrServerAddress), otlpAttr(attrs, attrURLPath)
	switch {
	case addr == "" || path == "":
		return addr + path
	case strings.HasPrefix(path, "/"):
		return addr + path
	default:
		return addr + "/" + path
	}
}

// otlpAttr — значение атрибута по ключу (строкой; см. otlpAttrString).
func otlpAttr(attrs []*commonpb.KeyValue, key string) string {
	for _, kv := range attrs {
		if kv.GetKey() == key {
			return otlpAttrString(kv.GetValue())
		}
	}
	return ""
}

// otlpMeasurements извлекает web vitals из атрибутов корневого спана с префиксом
// attrMeasurementPrefix (единственный известный носитель measurements в OTLP).
// Значение берётся из числового атрибута (double или int); строковые/булевы
// игнорируются. Дисциплина недоверенных данных та же, что в Sentry-парсере (см.
// parseMeasurements): не-конечные (NaN/Inf) и отрицательные — вон, имена каппятся
// (maxMeasurementKey), число ключей — maxMeasurements. Единицы не конвертируем:
// OTLP-значение уже числовое, соглашения об unit у этих атрибутов нет. Нет таких
// атрибутов → nil (в CH уедет пустой Map).
func otlpMeasurements(attrs []*commonpb.KeyValue) map[string]float64 {
	out := make(map[string]float64, 4)
	for _, kv := range attrs {
		name, ok := strings.CutPrefix(kv.GetKey(), attrMeasurementPrefix)
		if !ok || name == "" {
			continue
		}
		v, ok := otlpNumber(kv.GetValue())
		if !ok || math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			continue
		}
		out[capRunes(name, maxMeasurementKey)] = v
	}
	if len(out) == 0 {
		return nil
	}
	return capMeasurements(out)
}

// otlpNumber возвращает числовое значение атрибута (double как есть, int — с
// приведением к float64). ok=false — атрибут не числовой.
func otlpNumber(v *commonpb.AnyValue) (float64, bool) {
	switch x := v.GetValue().(type) {
	case *commonpb.AnyValue_DoubleValue:
		return x.DoubleValue, true
	case *commonpb.AnyValue_IntValue:
		return float64(x.IntValue), true
	}
	return 0, false
}

// otlpTags — теги транзакции: атрибуты корневого спана. Числа/булевы приводим к
// строке — колонка тегов строковая.
func otlpTags(attrs []*commonpb.KeyValue) map[string]string {
	tags := make(map[string]string, len(attrs))
	for _, kv := range attrs {
		if kv.GetKey() == "" {
			continue
		}
		tags[kv.GetKey()] = otlpAttrString(kv.GetValue())
	}
	return tags
}

// otlpData — всё, что несёт спан сверх наших полей: его атрибуты и события.
// Значения кладём СТРОКАМИ (даже числа): data — JSON-колонка, и один и тот же
// ключ, приезжающий то числом, то строкой, ломает её типизацию; терять при этом
// нечего — в UI это всё равно текст. Промотированные в Op/Description атрибуты
// не вычитаем: description капается до 2000 рун, а в data остаётся полностью.
//
// Число ключей ограничено maxDataKeys (тот же кап, что у тегов): всё содержимое
// Data сериализуется в колонку `data`, и спан со 100k атрибутов не должен
// утащить их туда все. events — служебный ключ поверх этого капа, а не атрибут.
func otlpData(s *tracepb.Span) map[string]any {
	attrs := s.GetAttributes()
	events := s.GetEvents()
	if len(attrs) == 0 && len(events) == 0 {
		return nil
	}
	data := otlpAttrMap(attrs)
	if len(events) > 0 {
		list := make([]any, 0, len(events))
		for _, ev := range events {
			e := map[string]any{"name": capRunes(ev.GetName(), 200)}
			if ts, ok := otlpTime(ev.GetTimeUnixNano()); ok {
				e["timestamp"] = ts.Format(time.RFC3339Nano)
			}
			if len(ev.GetAttributes()) > 0 {
				e["attributes"] = otlpAttrMap(ev.GetAttributes())
			}
			list = append(list, e)
		}
		if data == nil {
			data = make(map[string]any, 1)
		}
		data["events"] = list
	}
	return data
}

// otlpAttrMap — атрибуты в map для Span.Data, не более maxDataKeys штук. Выбор
// ключей при переполнении детерминирован — первые maxDataKeys в отсортированном
// порядке (как в capTags).
func otlpAttrMap(attrs []*commonpb.KeyValue) map[string]any {
	if len(attrs) == 0 {
		return nil
	}
	vals := make(map[string]string, len(attrs))
	for _, kv := range attrs {
		if kv.GetKey() == "" {
			continue
		}
		vals[capRunes(kv.GetKey(), 64)] = capRunes(otlpAttrString(kv.GetValue()), maxDataValue)
	}
	keys := make([]string, 0, len(vals))
	for k := range vals {
		keys = append(keys, k)
	}
	if len(keys) > maxDataKeys {
		sort.Strings(keys)
		keys = keys[:maxDataKeys]
	}
	out := make(map[string]any, len(keys))
	for _, k := range keys {
		out[k] = vals[k]
	}
	return out
}

// capDataMap ограничивает УЖЕ готовую span.Data (map[string]any): не более
// maxDataKeys ключей (детерминированно — первые в отсортированном порядке, как
// otlpAttrMap), длина ключа и строкового значения каппится (64 руны / maxDataValue).
// Sentry-путь (transaction.go) кладёт сюда data спанов КАК ЕСТЬ из payload'а SDK;
// без капа спан со 100k атрибутов или гигантским значением утаскивает их все в
// CH-колонку data. Не-строковые значения (числа/булевы/вложенные) длину колонки
// заметно не раздувают — оставляем как есть, чтобы не терять типы.
func capDataMap(m map[string]any) map[string]any {
	if len(m) == 0 {
		return m
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	if len(keys) > maxDataKeys {
		sort.Strings(keys)
		keys = keys[:maxDataKeys]
	}
	out := make(map[string]any, len(keys))
	for _, k := range keys {
		v := m[k]
		if s, ok := v.(string); ok {
			v = capRunes(s, maxDataValue)
		}
		out[capRunes(k, 64)] = v
	}
	return out
}

// otlpAttrString приводит AnyValue к строке. Составные значения (массив,
// kvlist) схлопываем в плоскую строку: наши колонки — строки и JSON-текст,
// вложенных типов мы не обещали.
func otlpAttrString(v *commonpb.AnyValue) string {
	switch x := v.GetValue().(type) {
	case *commonpb.AnyValue_StringValue:
		return x.StringValue
	case *commonpb.AnyValue_BoolValue:
		return strconv.FormatBool(x.BoolValue)
	case *commonpb.AnyValue_IntValue:
		return strconv.FormatInt(x.IntValue, 10)
	case *commonpb.AnyValue_DoubleValue:
		return strconv.FormatFloat(x.DoubleValue, 'f', -1, 64)
	case *commonpb.AnyValue_BytesValue:
		return hex.EncodeToString(x.BytesValue)
	case *commonpb.AnyValue_ArrayValue:
		parts := make([]string, 0, len(x.ArrayValue.GetValues()))
		for _, e := range x.ArrayValue.GetValues() {
			parts = append(parts, otlpAttrString(e))
		}
		return strings.Join(parts, ",")
	case *commonpb.AnyValue_KvlistValue:
		parts := make([]string, 0, len(x.KvlistValue.GetValues()))
		for _, kv := range x.KvlistValue.GetValues() {
			parts = append(parts, kv.GetKey()+"="+otlpAttrString(kv.GetValue()))
		}
		return strings.Join(parts, ",")
	}
	return ""
}
