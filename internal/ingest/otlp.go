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
	"regexp"
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

	"gitflic.ru/otezvikentiy/gotcha/internal/host"
	"gitflic.ru/otezvikentiy/gotcha/internal/hostmetric"
	"gitflic.ru/otezvikentiy/gotcha/internal/metric"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/trace"
	"log/slog"
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

// maxSpanAttrs — сколько атрибутов спана вообще читается.
//
// Потолок стоит НА ВХОДЕ, до построения карт и строк, и в этом весь смысл.
// Раньше карта строилась из ВСЕХ атрибутов, ключи сортировались, и только потом
// оставались первые maxDataKeys: тело в 10 МиБ (около 30 КБ в gzip) несёт
// примерно 1.2 млн атрибутов, и на них уходило около 100 МБ сверх самого тела —
// то есть десятикратное усиление, управляемое отправителем.
//
// 256 — заведомо больше, чем бывает у настоящего спана (спецификация OTel
// рекомендует 128), и заведомо меньше, чем нужно для усиления. Отбор
// maxDataKeys идёт среди прочитанных, порядок — как прислал отправитель.
const maxSpanAttrs = 256

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
	// будет ретраить вечно) и не тратим квоту. Envelope-путь приходит к тому же
	// результату иначе: там при выключенном трейсинге не выполняется отбор
	// транзакций, а квота списывается за отобранное.
	if !h.pipeline.TracingEnabled() {
		writeOTLPResponse(w, enc)
		return
	}
	if h.rateLimited(w, key.OrgID, key.ProjectID) {
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
		// errJSONTooDeep — не обычная ошибка разбора: тело НЕ уходит в protojson
		// (см. её докблок), поэтому отделяем от "malformed" и своя строка в логе.
		if errors.Is(err, errJSONTooDeep) {
			slog.Warn("otlp: body rejected", "reason", "json_too_deep",
				"project_id", key.ProjectID)
			writeJSONError(w, http.StatusBadRequest, "json nesting too deep")
			return
		}
		writeJSONError(w, http.StatusBadRequest, "malformed otlp payload")
		return
	}

	projectID := key.ProjectID
	// Квота списывается ПОСЛЕ разбора и ЗА ЭЛЕМЕНТ. Раньше она проверялась до
	// разбора — тогда числа транзакций в экспорте ещё нет, и списывалась одна
	// единица за весь батч: экспорт с десятью тысячами спанов стоил столько же,
	// сколько один. Разбор до списания безопасен: и rate-лимитер, и потолок
	// размера тела отрабатывают выше.
	txs := MapOTLP(req.GetResourceSpans(), time.Now().UTC())
	// Отбор ДО списания — тот же порядок, что на envelope-пути: платим за то,
	// что будет записано. Раньше квота списывалась за все разобранные
	// транзакции, и лишь потом семплирование отбрасывало несохраняемые.
	kept := h.sampleTransactions(r.Context(), projectID, txs)
	granted := h.grant(r.Context(), h.TxQuota, key.OrgID, "transaction", len(kept))
	// Уменьшаемое — число отобранных: отсеянное семплированием отброшено по
	// настройке проекта намеренно и потерей по квоте не является.
	if dropped := len(kept) - granted; dropped > 0 {
		h.countDrop(r.Context(), dropTransaction, key.OrgID, dropped)
		slog.Warn("ingest: transaction quota exceeded, dropping items from OTLP export",
			"dropped", dropped, "accepted", granted, "project_id", projectID, "org_id", key.OrgID)
	}
	// Условие опирается на отобранное: экспорт, целиком отсеянный
	// семплированием, — это успешный приём по настройке проекта, а не
	// исчерпанная квота, и отвечать на него 429 значило бы просить коллектор
	// прислать то же самое ещё раз.
	if granted == 0 && len(kept) > 0 {
		writeQuotaExceeded(w, "transaction quota exceeded")
		return
	}

	h.enqueueTransactions(projectID, key.OrgID, kept[:granted])
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
	// Квота ПОСЛЕ разбора и за датапойнт, а не за батч: экспорт коллектора
	// несёт тысячи точек, и одна единица за весь батч делала квоту метрик
	// декоративной.
	points := metric.MapOTLP(req.GetResourceMetrics(), time.Now().UTC())
	// Регистрация хостов — ДО среза по квоте и ДО ветки 429: квота ограничивает
	// запись точек в CH, а не приём экспорта, и живость хоста («он сейчас шлёт
	// метрики») — это факт о самом экспорте, а не о том, сколько из него влезло
	// в бюджет организации. Гард кардинальности применяется к ИМЕНИ здесь же.
	//
	// points[i].Host ЗАПИСЫВАЕТСЯ ПО ИНДЕКСУ (не в копию цикла `for _, p :=
	// range`) — иначе цикл записи ниже вызвал бы Value ВТОРОЙ РАЗ с тем же
	// СЫРЫМ именем и исказил бы CardinalityGuard.Report(): Value идемпотентен
	// только для уже ПРИНЯТЫХ значений (ветка `known` — просто возвращает их) и
	// для "" / CardinalityOverflow (ранний return, cardinality.go:127); для
	// имени, которое ТОЛЬКО ЧТО схлопнулось, повторный вызов с тем же сырым
	// значением не находит его в f.seen и повторно инкрементирует collapsed и
	// дублирует sample (cardinality.go:170-173) — счётчик и примеры в отчёте
	// оператора врали бы вдвое. Записав результат обратно в points[i].Host,
	// добиваемся, чтобы цикл записи (Task 3, ниже по функции) вызывал Value
	// уже на ПРИНЯТОМ имени или на CardinalityOverflow — то есть строго по
	// идемпотентным веткам.
	if h.Hosts != nil {
		// Версии агента — по СЫРЫМ (ещё не схлопнутым гардом) host.name, отдельным
		// проходом по ресурсам батча: MapOTLP уплощает ResourceMetrics в плоский
		// список точек и теряет границу resource (спека §3.2), а версия агента —
		// resource-атрибут, а не атрибут датапойнта.
		versions := agentVersionsByRawHost(req.GetResourceMetrics())
		roles := hostRolesByRawHost(req.GetResourceMetrics())
		seen := map[string]struct{}{}
		var hosts []host.TouchEntry
		for i := range points {
			// raw запоминается ДО Cardinality.Value: Value читает points[i].Host
			// ровно один раз на точку (см. комментарий выше, про collapsed-
			// статистику), а raw нужен ещё и как ключ в versions — карте, чьи
			// ключи посчитаны ДО схлопывания гардом.
			raw := points[i].Host
			name := h.Cardinality.Value(key.ProjectID, FieldHost, raw)
			points[i].Host = name
			if name == "" || name == CardinalityOverflow {
				continue
			}
			if _, ok := seen[name]; ok {
				continue
			}
			seen[name] = struct{}{}
			hosts = append(hosts, host.TouchEntry{
				Name:         name,
				AgentVersion: versions[raw],
				// points[i].Environment — промоутированное значение (metric.MapOTLP
				// разрешает оба env-ключа и применяет capRunes) ДО
				// Cardinality.Value(FieldEnvironment) — та применяется ниже, в цикле
				// записи (301-316). Для метки хоста это корректно: кардинальный гард
				// к строке hosts не нужен, потолок хостов держит MaxHostsPerProject.
				Environment: points[i].Environment,
				// Role — non-empty-wins по ресурсам батча (hostRolesByRawHost), а
				// Environment здесь берётся с первого ресурса хоста, встреченного
				// в points (seen-дедуп выше). Если один хост в одном батче разбит
				// на несколько ResourceMetrics с расходящимися метками (мисконфиг
				// источника, не штатный случай) — источники могут разойтись;
				// самоизлечивается на следующем тике non-empty-wins. Риск ~0.
				Role: roles[raw],
			})
		}
		if len(hosts) > 0 {
			h.Hosts.Touch(r.Context(), key.ProjectID, hosts)
		}
	}
	granted := h.grant(r.Context(), h.MetricQuota, key.OrgID, "metric", len(points))
	if dropped := len(points) - granted; dropped > 0 {
		h.countDrop(r.Context(), dropMetric, key.OrgID, dropped)
		slog.Warn("ingest: metric quota exceeded, dropping points from OTLP export",
			"dropped", dropped, "accepted", granted,
			"project_id", key.ProjectID, "org_id", key.OrgID)
	}
	if granted == 0 && len(points) > 0 {
		writeQuotaExceeded(w, "metric quota exceeded")
		return
	}

	for _, p := range points[:granted] {
		// Зачистка ПДн: атрибуты датапойнта — единственный носитель denylist-
		// значений на этом пути, и он идёт мимо Pipeline/Scrubber. h.Scrub == nil
		// — no-op (ScrubTags nil-safe). p.Attributes — map[string]string.
		h.Scrub.ScrubTags(p.Attributes)
		// Имя метрики и сервис стоят в ключе сортировки metric_points, поэтому
		// их кардинальность ограничена так же, как у имён транзакций.
		p.Name = h.Cardinality.Value(key.ProjectID, FieldMetricName, p.Name)
		p.Service = h.Cardinality.Value(key.ProjectID, FieldService, p.Service)
		p.Environment = h.Cardinality.Value(key.ProjectID, FieldEnvironment, p.Environment)
		// host.name — единственное промоутированное поле точки вне ключа
		// сортировки (см. FieldHost), но клиентское и такое же открытое, как имя
		// сервиса, поэтому под тем же гардом.
		p.Host = h.Cardinality.Value(key.ProjectID, FieldHost, p.Host)
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

// attrHostNameResource — resource-атрибут host.name. Дублирует одноимённую
// константу metric-пакета (там она не экспортирована): нужна здесь только
// затем, чтобы agentVersionsByRawHost построила ключ карты, совпадающий с
// points[i].Host после промоушена в metric.MapOTLP.
const attrHostNameResource = "host.name"

// agentVersionsByRawHost — версии агента по СЫРОМУ host.name resource-атрибута
// (с тем же капом в 200 рун, что применяет metric.MapOTLP при промоушене
// host.name, — иначе ключи этой карты не совпали бы с points[i].Host).
//
// Отдельный проход по ResourceMetrics нужен потому, что metric.MapOTLP
// уплощает батч в плоский список MetricPoint и теряет границу resource (спека
// §3.2): версия агента лежит в атрибутах РЕСУРСА, а не датапойнта, и после
// уплощения её было бы уже не привязать к конкретному хосту.
//
// В карту попадает ТОЛЬКО валидная непустая версия: смешанный экспорт (агент
// и коллектор hostmetrics шлют метрики с одним и тем же host.name в одном
// батче) не должен затирать уже найденную версию пустой — непустая версия
// побеждает вне зависимости от порядка ресурсов в батче.
func agentVersionsByRawHost(rms []*metricspb.ResourceMetrics) map[string]string {
	out := make(map[string]string, 4)
	for _, rm := range rms {
		var rawHost, rawVersion string
		for _, kv := range rm.GetResource().GetAttributes() {
			switch kv.GetKey() {
			case attrHostNameResource:
				rawHost = otlpResourceAttrString(kv.GetValue())
			case hostmetric.AgentVersionAttr:
				rawVersion = otlpResourceAttrString(kv.GetValue())
			}
		}
		if rawHost == "" {
			continue
		}
		version := validAgentVersion(rawVersion)
		if version == "" {
			continue // пустую/невалидную версию в карту не кладём — см. докблок
		}
		out[capRunes(rawHost, 200)] = version
	}
	return out
}

// attrHostRoleResource — resource-атрибут host.role: не промоутируется
// MapOTLP и в атрибутах датапойнта его нет, поэтому читается тем же отдельным
// проходом по ResourceMetrics, что и версия агента.
const attrHostRoleResource = "host.role"

// hostRolesByRawHost — роль хоста по СЫРОМУ host.name resource-атрибута, тем
// же проходом и с тем же капом 200 рун, что agentVersionsByRawHost.
func hostRolesByRawHost(rms []*metricspb.ResourceMetrics) map[string]string {
	out := make(map[string]string, 4)
	for _, rm := range rms {
		var rawHost, rawRole string
		for _, kv := range rm.GetResource().GetAttributes() {
			switch kv.GetKey() {
			case attrHostNameResource:
				rawHost = otlpResourceAttrString(kv.GetValue())
			case attrHostRoleResource:
				rawRole = otlpResourceAttrString(kv.GetValue())
			}
		}
		if rawHost == "" || rawRole == "" {
			continue
		}
		out[capRunes(rawHost, 200)] = capRunes(rawRole, 200)
	}
	return out
}

// otlpResourceAttrString — скалярное строковое представление resource-
// атрибута. Зеркалит attrString из internal/metric/parse.go (не otlpAttrString
// этого файла — та рекурсивно разворачивает массивы/kvlist и иначе капает
// строки): host.name и gotcha.agent.version обязаны читаться ИДЕНТИЧНО тому,
// как их читает metric.MapOTLP, иначе ключ agentVersionsByRawHost разойдётся с
// points[i].Host.
func otlpResourceAttrString(v *commonpb.AnyValue) string {
	switch x := v.GetValue().(type) {
	case *commonpb.AnyValue_StringValue:
		return x.StringValue
	case *commonpb.AnyValue_BoolValue:
		return strconv.FormatBool(x.BoolValue)
	case *commonpb.AnyValue_IntValue:
		return strconv.FormatInt(x.IntValue, 10)
	case *commonpb.AnyValue_DoubleValue:
		return strconv.FormatFloat(x.DoubleValue, 'f', -1, 64)
	}
	return ""
}

// maxAgentVersionLen — потолок длины resource-атрибута gotcha.agent.version.
// Настоящая версия укладывается в десяток символов; предел защищает колонку
// hosts.agent_version от недоверенного значения (тот же приём, что у прочих
// промотированных resource-атрибутов, см. capRunes(...,200) у host.name).
const maxAgentVersionLen = 32

// agentVersionPattern — semver-подобие версии агента: major.minor.patch плюс
// произвольный хвост (pre-release/build/git-описание вроде "0.6.0-5-gabc123").
// Полного semver не парсим — версия едет в служебное поле для отображения
// оператору ("Хосты"), не в сравнения версий.
var agentVersionPattern = regexp.MustCompile(`^v?\d+\.\d+\.\d+[0-9A-Za-z.+-]*$`)

// validAgentVersion проверяет resource-атрибут gotcha.agent.version на
// semver-подобие и длину. Мусор (SQL-инъекция, случайный текст, чрезмерная
// длина) → "": пустая версия — валидное состояние, Touch её не пишет поверх
// уже сохранённой (см. host.TouchEntry).
func validAgentVersion(s string) string {
	if len(s) > maxAgentVersionLen {
		return ""
	}
	if !agentVersionPattern.MatchString(s) {
		return ""
	}
	return s
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

// maxJSONWalkDepth — предел глубины ОБХОДА тела в otlpJSONHexIDs. Отличать от
// maxJSONIDDepth, который ограничивает только подмену идентификаторов: тот
// отключает замену и продолжает копировать, этот прекращает разбор целиком.
//
// Обход рекурсивен (value → valueFrom → object|array → value), а стек
// горутины в Go растёт до 1 ГБ и упирается в fatal error, которую recover не
// ловит: переполнение убивает процесс целиком, а не только запрос. Раньше
// защиту давала сама encoding/json — тело разбиралось через Unmarshal в
// map[string]any, и предел вложенности стоял в библиотеке. Потоковый разбор
// (коммит fe37c99) снял амплификацию кучи и вместе с ней эту защиту.
//
// Практическая глубина OTLP/JSON — около десяти: resourceSpans → scopeSpans →
// spans → attributes → value → kvlist. Сотня заведомо выше любого настоящего
// тела и заведомо ниже опасного.
const maxJSONWalkDepth = 100

// errJSONTooDeep — тело глубже предела обхода. В отличие от прочих ошибок
// разбора НЕ приводит к терпимому откату «вернуть тело как есть»: такое тело
// ушло бы в protojson без подмены идентификаторов и, если предел вложенности
// там выше нашего, было бы принято с hex-строками вместо байтов — то есть
// тихо испорчено. Поэтому глубина даёт отдельную ошибку, которую приём
// отличает от прочих и отвечает 400 со строкой в логе (см. otlpTraces).
var errJSONTooDeep = errors.New("ingest: json nesting too deep")

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
		rewritten, err := otlpJSONHexIDs(raw)
		if err != nil {
			return err // errJSONTooDeep — единственная ошибка, otlpJSONHexIDs больше не отдаёт
		}
		return protojson.UnmarshalOptions{DiscardUnknown: true}.Unmarshal(rewritten, req)
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
// Разбор ПОТОКОВЫЙ: токен за токеном, сразу в выходной буфер. Раньше тело
// целиком материализовалось в map[string]any/[]any, и это была удалённая
// амплификация памяти — измерено: 10 МиБ тела (предел Handler.body для
// распакованного) превращались в 507 МиБ кучи, ×51. На проводе с gzip такое
// тело весит около 15 КБ, а ключ приёма публичен по построению — он лежит в
// браузерных бандлах. Одного запроса хватало, чтобы положить процесс на
// профиле с mem_limit 256 МиБ. Тот же класс уже чинили для zstd; это была
// соседняя дверь.
//
// Числа переносятся как json.Number — иначе большие наносекундные таймстемпы
// уехали бы через float64 и вернулись в экспоненциальной записи, которую
// protojson не примет. Глубина ПОДМЕНЫ идентификаторов ограничена
// maxJSONIDDepth, а глубина самого ОБХОДА — отдельным, более широким пределом
// maxJSONWalkDepth (см. его докблок): без него рекурсия по недоверенному телу
// валит процесс переполнением стека, которое recover не ловит.
//
// Любая ОБЫЧНАЯ ошибка разбора — не наша забота: возвращаем тело нетронутым и
// даём protojson отчитаться об ошибке самому. errJSONTooDeep — исключение из
// этого правила (см. её докблок): она отдаётся наружу, а не проглатывается.
func otlpJSONHexIDs(raw []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()

	// Выход не больше входа плюс небольшой запас: hex→base64 короче исходного
	// (32 символа против 24), а всё остальное переписывается один в один.
	var out bytes.Buffer
	out.Grow(len(raw))

	w := &otlpIDRewriter{dec: dec, out: &out}
	if err := w.value(0); err != nil {
		if errors.Is(err, errJSONTooDeep) {
			return nil, errJSONTooDeep
		}
		return raw, nil
	}
	// В теле обязан быть ровно один документ; хвост означает мусор, и разбирать
	// его — забота protojson, а не наша.
	if _, err := dec.Token(); err != io.EOF {
		return raw, nil
	}
	if !w.changed {
		return raw, nil // hex-идентификаторов не было: тело уже в base64
	}
	return out.Bytes(), nil
}

// otlpIDRewriter переписывает поток JSON-токенов в выходной буфер, подменяя
// значения идентификаторов из otlpJSONIDLen с hex на base64.
//
// Существует вместо обхода разобранного дерева, потому что дерево — это и был
// вектор амплификации: json.Decoder читает по токену, а держать в памяти
// приходится только выход.
type otlpIDRewriter struct {
	dec     *json.Decoder
	out     *bytes.Buffer
	changed bool
}

// value переписывает одно значение (скаляр, объект или массив) с его позиции в
// потоке. Глубже maxJSONIDDepth идентификаторов не бывает, но копировать
// содержимое всё равно надо — иначе тело потеряло бы данные, поэтому ЭТОТ
// предел только отключает подмену, а не обход. Сам обход ограничен отдельно,
// в object/array, — см. maxJSONWalkDepth.
func (w *otlpIDRewriter) value(depth int) error {
	tok, err := w.dec.Token()
	if err != nil {
		return err
	}
	return w.valueFrom(tok, depth, "")
}

// valueFrom переписывает значение, первый токен которого уже прочитан. key —
// имя поля, в котором это значение лежит (пусто для элементов массива и
// корня): по нему решается, подменять ли идентификатор.
func (w *otlpIDRewriter) valueFrom(tok json.Token, depth int, key string) error {
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			return w.object(depth)
		case '[':
			return w.array(depth)
		default:
			// '}' или ']' на месте значения — сломанный JSON.
			return errUnexpectedJSONDelim
		}
	case string:
		if n, ok := otlpJSONIDLen[key]; ok && depth <= maxJSONIDDepth {
			if b64, ok := hexIDToBase64(t, n); ok {
				w.changed = true
				return w.writeString(b64)
			}
		}
		return w.writeString(t)
	case json.Number:
		_, err := w.out.WriteString(t.String())
		return err
	case bool:
		if t {
			_, err := w.out.WriteString("true")
			return err
		}
		_, err := w.out.WriteString("false")
		return err
	case nil:
		_, err := w.out.WriteString("null")
		return err
	default:
		return errUnexpectedJSONToken
	}
}

func (w *otlpIDRewriter) object(depth int) error {
	if depth > maxJSONWalkDepth {
		return errJSONTooDeep
	}
	w.out.WriteByte('{')
	first := true
	for w.dec.More() {
		keyTok, err := w.dec.Token()
		if err != nil {
			return err
		}
		key, ok := keyTok.(string)
		if !ok {
			return errUnexpectedJSONToken
		}
		if !first {
			w.out.WriteByte(',')
		}
		first = false
		if err := w.writeString(key); err != nil {
			return err
		}
		w.out.WriteByte(':')

		valTok, err := w.dec.Token()
		if err != nil {
			return err
		}
		if err := w.valueFrom(valTok, depth+1, key); err != nil {
			return err
		}
	}
	if _, err := w.dec.Token(); err != nil { // закрывающая '}'
		return err
	}
	w.out.WriteByte('}')
	return nil
}

func (w *otlpIDRewriter) array(depth int) error {
	if depth > maxJSONWalkDepth {
		return errJSONTooDeep
	}
	w.out.WriteByte('[')
	first := true
	for w.dec.More() {
		if !first {
			w.out.WriteByte(',')
		}
		first = false
		if err := w.value(depth + 1); err != nil {
			return err
		}
	}
	if _, err := w.dec.Token(); err != nil { // закрывающая ']'
		return err
	}
	w.out.WriteByte(']')
	return nil
}

// writeString пишет строку как JSON-литерал БЕЗ HTML-эскейпа: тела спанов несут
// URL и SQL с символами &, < и >, и превращать их в \u0026 значило бы менять
// данные ради ничего — этот JSON уходит не в браузер, а в protojson.
func (w *otlpIDRewriter) writeString(s string) error {
	enc := json.NewEncoder(w.out)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		return err
	}
	// Encode дописывает перевод строки — он здесь лишний.
	w.out.Truncate(w.out.Len() - 1)
	return nil
}

var (
	errUnexpectedJSONToken = errors.New("ingest: unexpected json token")
	errUnexpectedJSONDelim = errors.New("ingest: unexpected json delimiter")
)

// maxJSONIDDepth — предел глубины ПОДМЕНЫ идентификаторов в otlpJSONHexIDs.
// Идентификаторы живут на глубине ~6 (resourceSpans → scopeSpans → spans →
// links); всё, что глубже, — вложенные атрибуты, и переписывать там нечего.
//
// ВАЖНО: это НЕ предел обхода. Глубже maxJSONIDDepth подмена просто
// отключается, но копирование продолжается как обычно — иначе тело потеряло
// бы данные. Сам обход ограничен отдельно и намного шире — maxJSONWalkDepth;
// раньше здесь было написано, что «обход глубже просто прекращается», и это
// было неправдой — она и усыпила прошлое ревью, пропустившее рекурсию без
// предела (см. errJSONTooDeep).
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

// capAttrs отсекает хвост списка атрибутов до maxSpanAttrs.
//
// Возвращает исходный слайс, когда он и так короче: копии не делается, это
// горячий путь приёма.
func capAttrs(attrs []*commonpb.KeyValue) []*commonpb.KeyValue {
	if len(attrs) <= maxSpanAttrs {
		return attrs
	}
	return attrs[:maxSpanAttrs]
}

// otlpTags — теги транзакции: атрибуты корневого спана. Числа/булевы приводим к
// строке — колонка тегов строковая.
func otlpTags(attrs []*commonpb.KeyValue) map[string]string {
	attrs = capAttrs(attrs)
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
	attrs = capAttrs(attrs)
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
// maxAttrDepth/maxAttrLen — потолки обхода вложенного AnyValue. Глубину выбирает
// КЛИЕНТ (protobuf допускает тысячи уровней), а каждый уровень безусловно склеивал
// всех детей, поэтому стоимость была O(глубина × длина) и 518 КБ тела давали ~1 ГБ
// и полсекунды CPU на HTTP-горутине. capRunes применялся ПОСЛЕ полной рекурсии,
// то есть уже после раздувания — ограничивать надо ВО ВРЕМЯ обхода.
const (
	maxAttrDepth = 8
	maxAttrLen   = 8192
)

func otlpAttrString(v *commonpb.AnyValue) string { return otlpAttrStringDepth(v, 0) }

func otlpAttrStringDepth(v *commonpb.AnyValue, depth int) string {
	switch x := v.GetValue().(type) {
	case *commonpb.AnyValue_StringValue:
		return capRunes(x.StringValue, maxAttrLen)
	case *commonpb.AnyValue_BoolValue:
		return strconv.FormatBool(x.BoolValue)
	case *commonpb.AnyValue_IntValue:
		return strconv.FormatInt(x.IntValue, 10)
	case *commonpb.AnyValue_DoubleValue:
		return strconv.FormatFloat(x.DoubleValue, 'f', -1, 64)
	case *commonpb.AnyValue_BytesValue:
		return capRunes(hex.EncodeToString(x.BytesValue), maxAttrLen)
	case *commonpb.AnyValue_ArrayValue:
		if depth >= maxAttrDepth {
			return ""
		}
		return joinAttrParts(x.ArrayValue.GetValues(), func(e *commonpb.AnyValue) string {
			return otlpAttrStringDepth(e, depth+1)
		})
	case *commonpb.AnyValue_KvlistValue:
		if depth >= maxAttrDepth {
			return ""
		}
		// Ёмкость по числу прочитанных, а не по длине списка: склейка и так
		// обрывается по maxAttrLen, но слайс на миллион элементов выделялся до
		// первой проверки.
		kvs := capAttrs(x.KvlistValue.GetValues())
		parts := make([]string, 0, len(kvs))
		total := 0
		for _, kv := range kvs {
			p := kv.GetKey() + "=" + otlpAttrStringDepth(kv.GetValue(), depth+1)
			if total += len(p) + 1; total > maxAttrLen {
				break // накопленная длина исчерпана — дальше не склеиваем
			}
			parts = append(parts, p)
		}
		return strings.Join(parts, ",")
	}
	return ""
}

// joinAttrParts склеивает элементы, обрывая обход по достижении maxAttrLen, —
// длина результата ограничивается ВО ВРЕМЯ склейки, а не после.
func joinAttrParts[T any](items []T, render func(T) string) string {
	parts := make([]string, 0, len(items))
	total := 0
	for _, it := range items {
		p := render(it)
		if total += len(p) + 1; total > maxAttrLen {
			break
		}
		parts = append(parts, p)
	}
	return strings.Join(parts, ",")
}
