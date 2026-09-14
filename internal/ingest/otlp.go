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

const attrMeasurementPrefix = "sentry.measurements."

const maxDataValue = maxSpanDescription

const maxDataKeys = 64

const maxSpanAttrs = 256

const maxOTLPSpans = 10000

// защита от цикла в parent_span_id: без предела битый батч крутил бы подъём к корню вечно.
const maxParentHops = 64

// ответ обязан уйти в кодировке запроса, иначе коллектор считает экспорт неуспешным и ретраит вечно.
type otlpEncoding int

const (
	otlpProtobuf otlpEncoding = iota
	otlpJSON
)

func (h *Handler) otlpTraces(w http.ResponseWriter, r *http.Request) {
	key, ok := h.otlpAuthenticate(w, r, SignalTransaction)
	if !ok {
		return
	}
	enc, ok := otlpEncodingOf(r.Header.Get("Content-Type"))
	if !ok {
		writeJSONError(w, http.StatusUnsupportedMediaType, "unsupported content-type")
		return
	}
	// трейсинг выключен — отвечаем успехом без записи и не тратим квоту.
	if !h.pipeline.TracingEnabled() {
		writeOTLPResponse(w, enc)
		return
	}
	if h.rateLimited(w, key.OrgID, key.ProjectID, SignalTransaction) {
		return
	}
	if h.overloaded(w, key.OrgID, key.ProjectID, SignalTransaction, h.pipeline.TransactionSaturation()) {
		return
	}
	body, closeBody, err := h.body(w, r)
	if err != nil {
		h.countRejected(RejectMalformed, SignalTransaction)
		writeJSONError(w, http.StatusBadRequest, "bad body encoding")
		return
	}
	defer closeBody()
	raw, err := io.ReadAll(body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.Is(err, ErrTooLarge) || errors.As(err, &maxErr) {
			h.countRejected(RejectTooLarge, SignalTransaction)
			writeJSONError(w, http.StatusRequestEntityTooLarge, "export too large")
			return
		}
		h.countRejected(RejectMalformed, SignalTransaction)
		writeJSONError(w, http.StatusBadRequest, "bad body")
		return
	}
	var req tracepb.TracesData
	if err := otlpUnmarshal(enc, raw, &req); err != nil {
		// тело не уходит в protojson — логируем и отвечаем отдельно от прочих ошибок разбора.
		if errors.Is(err, errJSONTooDeep) {
			slog.Warn("otlp: body rejected", "reason", "json_too_deep",
				"project_id", key.ProjectID)
			h.countRejected(RejectMalformed, SignalTransaction)
			writeJSONError(w, http.StatusBadRequest, "json nesting too deep")
			return
		}
		h.countRejected(RejectMalformed, SignalTransaction)
		writeJSONError(w, http.StatusBadRequest, "malformed otlp payload")
		return
	}

	projectID := key.ProjectID
	txs := MapOTLP(req.GetResourceSpans(), time.Now().UTC())
	kept := h.sampleTransactions(r.Context(), projectID, txs)
	granted, chargedAt := h.grant(r.Context(), h.TxQuota, key.OrgID, "transaction", len(kept))
	if dropped := len(kept) - granted; dropped > 0 {
		h.countDrop(r.Context(), dropTransaction, key.OrgID, dropped)
		slog.Warn("ingest: transaction quota exceeded, dropping items from OTLP export",
			"dropped", dropped, "accepted", granted, "project_id", projectID, "org_id", key.OrgID)
	}
	if granted == 0 && len(kept) > 0 {
		h.writeQuotaExceeded(w, SignalTransaction, "transaction quota exceeded")
		return
	}

	enqueued, capacityDropped := h.enqueueTransactions(projectID, key.OrgID, kept[:granted])
	h.refund(r.Context(), h.TxQuota, key.OrgID, "transaction", capacityDropped, chargedAt)
	if enqueued == 0 && capacityDropped > 0 {
		h.overloaded(w, key.OrgID, projectID, SignalTransaction, 1.0)
		return
	}
	writeOTLPResponse(w, enc)
}

// метрики не семплируются и не зависят от флага трейсинга.
func (h *Handler) otlpMetrics(w http.ResponseWriter, r *http.Request) {
	key, ok := h.otlpAuthenticate(w, r, SignalMetric)
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
	if h.rateLimited(w, key.OrgID, key.ProjectID, SignalMetric) {
		return
	}
	if h.overloaded(w, key.OrgID, key.ProjectID, SignalMetric, saturationOf(h.Metrics)) {
		return
	}
	body, closeBody, err := h.body(w, r)
	if err != nil {
		h.countRejected(RejectMalformed, SignalMetric)
		writeJSONError(w, http.StatusBadRequest, "bad body encoding")
		return
	}
	defer closeBody()
	raw, err := io.ReadAll(body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.Is(err, ErrTooLarge) || errors.As(err, &maxErr) {
			h.countRejected(RejectTooLarge, SignalMetric)
			writeJSONError(w, http.StatusRequestEntityTooLarge, "export too large")
			return
		}
		h.countRejected(RejectMalformed, SignalMetric)
		writeJSONError(w, http.StatusBadRequest, "bad body")
		return
	}
	var req metricspb.MetricsData
	if err := otlpUnmarshalMetrics(enc, raw, &req); err != nil {
		h.countRejected(RejectMalformed, SignalMetric)
		writeJSONError(w, http.StatusBadRequest, "malformed otlp payload")
		return
	}
	points := metric.MapOTLP(req.GetResourceMetrics(), time.Now().UTC())
	// Регистрация хостов идёт до среза по квоте (та режет запись в CH, не приём);
	// points[i].Host правится по индексу — повторный Cardinality.Value задвоил бы счётчик.
	switch {
	case h.Hosts == nil:
	case !scopeAllowsHosts(key.Kind):
		for i := range points {
			if points[i].Host != "" {
				h.hostScopeSkipped.Add(1)
				break
			}
		}
	default:
		versions := agentVersionsByRawHost(req.GetResourceMetrics())
		roles := hostRolesByRawHost(req.GetResourceMetrics())
		seen := map[string]struct{}{}
		var hosts []host.TouchEntry
		for i := range points {
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
				Environment:  points[i].Environment,
				Role:         roles[raw],
			})
		}
		if len(hosts) > 0 {
			h.Hosts.Touch(r.Context(), key.ProjectID, hosts)
		}
	}
	granted, _ := h.grant(r.Context(), h.MetricQuota, key.OrgID, "metric", len(points))
	if dropped := len(points) - granted; dropped > 0 {
		h.countDrop(r.Context(), dropMetric, key.OrgID, dropped)
		slog.Warn("ingest: metric quota exceeded, dropping points from OTLP export",
			"dropped", dropped, "accepted", granted,
			"project_id", key.ProjectID, "org_id", key.OrgID)
	}
	if granted == 0 && len(points) > 0 {
		h.writeQuotaExceeded(w, SignalMetric, "metric quota exceeded")
		return
	}

	for _, p := range points[:granted] {
		h.Scrub.ScrubTags(p.Attributes)
		p.Name = h.Cardinality.Value(key.ProjectID, FieldMetricName, p.Name)
		p.Service = h.Cardinality.Value(key.ProjectID, FieldService, p.Service)
		p.Environment = h.Cardinality.Value(key.ProjectID, FieldEnvironment, p.Environment)
		p.Host = h.Cardinality.Value(key.ProjectID, FieldHost, p.Host)
		h.Metrics.Add(key.ProjectID, p)
	}
	writeOTLPResponse(w, enc)
}

func otlpUnmarshalMetrics(enc otlpEncoding, raw []byte, req *metricspb.MetricsData) error {
	if enc == otlpJSON {
		return protojson.UnmarshalOptions{DiscardUnknown: true}.Unmarshal(raw, req)
	}
	return proto.Unmarshal(raw, req)
}

const attrHostNameResource = "host.name"

// капается в 200 рун, как в metric.MapOTLP — иначе ключи разойдутся с points[i].Host.
// Непустая версия побеждает вне зависимости от порядка ресурсов в батче.
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
			continue
		}
		out[capRunes(rawHost, 200)] = version
	}
	return out
}

const attrHostRoleResource = "host.role"

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

// должен читать host.name/gotcha.agent.version идентично metric.MapOTLP — иначе
// ключ agentVersionsByRawHost разойдётся с points[i].Host.
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

const maxAgentVersionLen = 32

var agentVersionPattern = regexp.MustCompile(`^v?\d+\.\d+\.\d+[0-9A-Za-z.+-]*$`)

// пустая строка — валидное состояние: Touch не пишет её поверх уже сохранённой версии.
func validAgentVersion(s string) string {
	if len(s) > maxAgentVersionLen {
		return ""
	}
	if !agentVersionPattern.MatchString(s) {
		return ""
	}
	return s
}

// 401 — нет заголовка/ключ не найден; 403 — ключ валиден, но не допущен к сигналу (скоуп).
func (h *Handler) otlpAuthenticate(w http.ResponseWriter, r *http.Request, signal IngestSignal) (org.Key, bool) {
	pub := otlpBearer(r)
	if pub == "" {
		h.countKeyReject(KeyRejectMissingBearer, r.URL.Path)
		h.countRejected(RejectKeyUnknown, signal)
		writeJSONError(w, http.StatusUnauthorized, "missing bearer token")
		return org.Key{}, false
	}
	key, err := h.keys.Resolve(r.Context(), pub)
	switch {
	case errors.Is(err, org.ErrNotFound):
		h.countKeyReject(KeyRejectInvalidDSNKey, r.URL.Path)
		h.countRejected(RejectKeyUnknown, signal)
		writeJSONError(w, http.StatusUnauthorized, "invalid dsn key")
		return org.Key{}, false
	case err != nil:
		writeJSONError(w, http.StatusServiceUnavailable, "key lookup failed")
		return org.Key{}, false
	}
	if !scopeAllows(key.Kind, signal) {
		h.scopeReject(w, r, signal, key.ProjectID)
		return org.Key{}, false
	}
	h.touchDeprecatedSignal(r.Context(), key.ProjectID)
	return key, true
}

func otlpBearer(r *http.Request) string {
	scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return strings.TrimSpace(token)
}

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

// обход рекурсивен: без предела глубины недоверенное тело валит горутину
// переполнением стека — fatal error, которую recover не ловит.
const maxJSONWalkDepth = 100

// не откатываем как обычную ошибку разбора: тело ушло бы в protojson с hex-строками
// вместо байтов и было бы тихо испорчено.
var errJSONTooDeep = errors.New("ingest: json nesting too deep")

// OTLP/JSON — protojson поверх тех же сообщений, не «просто JSON»: encoding/json тут
// молча декодировал бы hex-идентификаторы как мусор.
func otlpUnmarshal(enc otlpEncoding, raw []byte, req *tracepb.TracesData) error {
	if enc == otlpJSON {
		rewritten, err := otlpJSONHexIDs(raw)
		if err != nil {
			return err
		}
		return protojson.UnmarshalOptions{DiscardUnknown: true}.Unmarshal(rewritten, req)
	}
	return proto.Unmarshal(raw, req)
}

// protojson трактует trace/span id как base64 и не падает на hex (кратно 4) —
// молча даёт мусорные байты, поэтому переписываем hex→base64 сами.
func otlpJSONHexIDs(raw []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()

	var out bytes.Buffer
	out.Grow(len(raw))

	w := &otlpIDRewriter{dec: dec, out: &out}
	if err := w.value(0); err != nil {
		if errors.Is(err, errJSONTooDeep) {
			return nil, errJSONTooDeep
		}
		return raw, nil
	}
	if _, err := dec.Token(); err != io.EOF {
		return raw, nil
	}
	if !w.changed {
		return raw, nil
	}
	return out.Bytes(), nil
}

type otlpIDRewriter struct {
	dec     *json.Decoder
	out     *bytes.Buffer
	changed bool
}

func (w *otlpIDRewriter) value(depth int) error {
	tok, err := w.dec.Token()
	if err != nil {
		return err
	}
	return w.valueFrom(tok, depth, "")
}

func (w *otlpIDRewriter) valueFrom(tok json.Token, depth int, key string) error {
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			return w.object(depth)
		case '[':
			return w.array(depth)
		default:
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
	if _, err := w.dec.Token(); err != nil {
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
	if _, err := w.dec.Token(); err != nil {
		return err
	}
	w.out.WriteByte(']')
	return nil
}

// без HTML-эскейпа: тела спанов несут URL/SQL с & < >, а этот JSON уходит не в
// браузер, а в protojson.
func (w *otlpIDRewriter) writeString(s string) error {
	enc := json.NewEncoder(w.out)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		return err
	}
	w.out.Truncate(w.out.Len() - 1)
	return nil
}

var (
	errUnexpectedJSONToken = errors.New("ingest: unexpected json token")
	errUnexpectedJSONDelim = errors.New("ingest: unexpected json delimiter")
)

// предел подмены id, не обхода: глубже подмена отключается, копирование продолжается.
const maxJSONIDDepth = 64

var otlpJSONIDLen = map[string]int{
	"traceId":        maxTraceID,
	"trace_id":       maxTraceID,
	"spanId":         maxSpanID,
	"span_id":        maxSpanID,
	"parentSpanId":   maxSpanID,
	"parent_span_id": maxSpanID,
}

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

const otlpEmptyJSON = "{}"

// partial_success не заполняем — оно для частично отвергнутых батчей, не для семплированных.
func writeOTLPResponse(w http.ResponseWriter, enc otlpEncoding) {
	body, mediaTy := []byte(nil), "application/x-protobuf"
	if enc == otlpJSON {
		body, mediaTy = []byte(otlpEmptyJSON), "application/json"
	}
	w.Header().Set("Content-Type", mediaTy)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// корень транзакции — spans без parent_span_id либо kind=SERVER/CONSUMER; спаны без
// корня в этом же батче — сироты, отбрасываются (сборка трейса между запросами не делается).
func MapOTLP(rs []*tracepb.ResourceSpans, now time.Time) []trace.Transaction {
	type flatSpan struct {
		res      otlpResource
		span     *tracepb.Span
		traceID  string
		spanID   string
		parentID string
	}
	var flat []flatSpan
	// parents: (trace_id,span_id) → parent_span_id — поднять ребёнка до ближайшего корня.
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
	// rootOf: (trace_id,span_id) корня → индекс транзакции; firstRoot: запасной
	// вариант, если цепочка родителей ушла за пределы батча.
	rootOf := make(map[string]int, 4)
	firstRoot := make(map[string]int, 4)

	for _, e := range flat {
		if !otlpIsRoot(e.span) {
			continue
		}
		start, end, ok := otlpSpanTimes(e.span, now)
		if !ok {
			continue // вне окна хранения — транзакции нет
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
			continue // спан-«отравитель» по партициям
		}
		op := otlpOp(e.span)
		txs[i].Spans = append(txs[i].Spans, trace.Span{
			SpanID:       e.spanID,
			ParentSpanID: e.parentID,
			Op:           op,
			// байтовый кап, не рунный: Description идёт в NormalizeSQL, чья
			// цена зависит от длины в байтах (см. capBytes).
			Description: capBytes(otlpDescription(e.span, op), maxSpanDescription),
			Start:       start,
			End:         end,
			Status:      otlpStatus(e.span.GetStatus()),
			Data:        otlpData(e.span),
		})
	}
	return txs
}

// span_id уникален только внутри трейса, поэтому ключ включает и trace_id.
func otlpSpanKey(traceID, spanID string) string { return traceID + "\x00" + spanID }

// поднимается по parent_span_id до ближайшего корня батча; maxParentHops заодно
// обезвреживает цикл в parent_span_id. Не дошли — запасной вариант: первый корень трейса.
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

// SERVER/CONSUMER считаются корнем даже с parent_span_id: родитель живёт в другом
// сервисе, входящий запрос — начало транзакции в этой модели.
func otlpIsRoot(s *tracepb.Span) bool {
	if otlpID(s.GetParentSpanId(), maxSpanID) == "" {
		return true
	}
	kind := s.GetKind()
	return kind == tracepb.Span_SPAN_KIND_SERVER || kind == tracepb.Span_SPAN_KIND_CONSUMER
}

// пустой или нулевой (все байты 0) id невалиден по спеке OTLP — возвращаем "".
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
		end = start
	}
	return start, end, true
}

// 0 (поле не заполнено) и значения вне int64 — это не время, а мусор.
func otlpTime(ns uint64) (time.Time, bool) {
	if ns == 0 || ns > math.MaxInt64 {
		return time.Time{}, false
	}
	return time.Unix(0, int64(ns)).UTC(), true
}

// всё, кроме ERROR (в т.ч. UNSET), — 'ok': MV transactions_5m считает провалом
// всё != 'ok', иначе failure rate был бы 100%.
func otlpStatus(st *tracepb.Status) string {
	if st.GetCode() == tracepb.Status_STATUS_CODE_ERROR {
		return "internal_error"
	}
	return "ok"
}

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

// оба ключа обязательны: без старого (db.system) спаны свежих SDK, шлющих только
// db.system.name, не получили бы db-op и стали бы невидимы для детекторов N+1/slow-query.
func otlpDBSystem(attrs []*commonpb.KeyValue) string {
	if v := otlpAttr(attrs, attrDBSystem); v != "" {
		return v
	}
	return otlpAttr(attrs, attrDBSystemName)
}

// redis/memcached получают свой op, не общий db: SQL-нормализатор читает
// `GET user:42` как именованный плейсхолдер и не различает разные ключи — N+1 не увидеть.
func otlpDBOp(system string) string {
	switch strings.ToLower(strings.TrimSpace(system)) {
	case "redis", "valkey":
		return "db.redis"
	case "memcached":
		return "db.memcached"
	}
	return "db"
}

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

// без url.full собираем из server.address+url.path, следя за слэшем: путь по
// семконвенции начинается с /, но не всегда — склейка «в лоб» дала бы `api.internalv2/users`.
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

func otlpAttr(attrs []*commonpb.KeyValue, key string) string {
	for _, kv := range attrs {
		if kv.GetKey() == key {
			return otlpAttrString(kv.GetValue())
		}
	}
	return ""
}

// NaN/Inf/отрицательные — вон, имена и число ключей каппятся (как в Sentry-парсере).
// Единицы не конвертируем: OTLP-значение уже числовое, соглашения об unit тут нет.
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

func otlpNumber(v *commonpb.AnyValue) (float64, bool) {
	switch x := v.GetValue().(type) {
	case *commonpb.AnyValue_DoubleValue:
		return x.DoubleValue, true
	case *commonpb.AnyValue_IntValue:
		return float64(x.IntValue), true
	}
	return 0, false
}

func capAttrs(attrs []*commonpb.KeyValue) []*commonpb.KeyValue {
	if len(attrs) <= maxSpanAttrs {
		return attrs
	}
	return attrs[:maxSpanAttrs]
}

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

// значения кладём строками: смешение типов в одном ключе ломает JSON-колонку.
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

// Sentry-путь кладёт data спанов как есть — без капа спан со 100k атрибутов
// утащит их все в колонку.
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

// глубину и длину капаем во время обхода — иначе клиент раздувает CPU/память
// вложенным AnyValue произвольной глубины.
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
		kvs := capAttrs(x.KvlistValue.GetValues())
		parts := make([]string, 0, len(kvs))
		total := 0
		for _, kv := range kvs {
			p := kv.GetKey() + "=" + otlpAttrStringDepth(kv.GetValue(), depth+1)
			if total += len(p) + 1; total > maxAttrLen {
				break
			}
			parts = append(parts, p)
		}
		return strings.Join(parts, ",")
	}
	return ""
}

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
