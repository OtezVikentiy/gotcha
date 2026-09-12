package web

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/log"
	"gitflic.ru/otezvikentiy/gotcha/internal/logfilter"
	"gitflic.ru/otezvikentiy/gotcha/internal/web/templates"
)

const logsListLimit = 100

// положительные параметры лимита не имеют — иначе он сломал бы уже разосланные ссылки.
const maxNegativeConditions = 20

const logsHistogramBuckets = 48

const (
	logsAttrKeysLimit   = 20
	logsAttrValuesLimit = 10
)

const logsAttrKeysAutocompleteLimit = 20

const attrKeysCacheTTL = 60 * time.Second

// при переполнении карта очищается целиком: записи короткоживущие и дёшевы
// для пересчёта, ярусное вытеснение не оправдано.
const maxAttrKeysCacheEntries = 10000

// period/start/end — сырые значения query, не резолвленные метки: resolveTimeRange
// пересчитывает их от time.Now() на каждый вызов, кеш по абсолютным меткам не сработал бы.
type attrKeysCacheKey struct {
	projectID int64
	prefix    string
	period    string
	start     string
	end       string
}

func attrKeysWindowKey(q url.Values) (period, start, end string) {
	return q.Get("period"), q.Get("start"), q.Get("end")
}

type attrKeysCacheEntry struct {
	values  []log.FacetValue
	expires time.Time
}

type attrKeysCache struct {
	now func() time.Time

	mu      sync.Mutex
	entries map[attrKeysCacheKey]attrKeysCacheEntry
}

func newAttrKeysCache() *attrKeysCache {
	return &attrKeysCache{
		now:     time.Now,
		entries: map[attrKeysCacheKey]attrKeysCacheEntry{},
	}
}

func (c *attrKeysCache) get(projectID int64, prefix, period, start, end string) ([]log.FacetValue, bool) {
	key := attrKeysCacheKey{projectID: projectID, prefix: prefix, period: period, start: start, end: end}
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok || !e.expires.After(now) {
		return nil, false
	}
	return e.values, true
}

func (c *attrKeysCache) put(projectID int64, prefix, period, start, end string, values []log.FacetValue) {
	key := attrKeysCacheKey{projectID: projectID, prefix: prefix, period: period, start: start, end: end}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) >= maxAttrKeysCacheEntries {
		c.entries = map[attrKeysCacheKey]attrKeysCacheEntry{}
	}
	c.entries[key] = attrKeysCacheEntry{values: values, expires: c.now().Add(attrKeysCacheTTL)}
}

func (h *Handler) logsList(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	projectID, ok := h.parsePathProjectID(w, r)
	if !ok {
		return
	}
	canAccess, err := h.Org.CanAccessProject(r.Context(), uid, projectID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	if !canAccess {
		h.notFound(w, r)
		return
	}
	h.renderLogsPage(w, r, http.StatusOK, projectID, uid, "", r.URL.Query())
}

// params: GET передаёт r.URL.Query(), POST-перерисовка при ошибке — logFilterFormParams(r)
// (у POST query пуст, r.URL.Query() тут потерял бы введённые условия).
func (h *Handler) renderLogsPage(w http.ResponseWriter, r *http.Request, status int, projectID, uid int64, errMsg string, params url.Values) {
	if h.LogQuery == nil {
		h.notFound(w, r)
		return
	}

	// Окно клампится дважды: общим контролом (90 дней), затем retention'ом
	// (h.LogRetentionDays) — иначе запрос уйдёт в партиции ClickHouse, которых уже нет.
	rng := h.resolveTimeRange(w, r, "24h")
	q := params
	f, rangeClamped := parseLogFilter(q, rng, h.LogRetentionDays)

	// nodefault — отдельный признак подавления умолчания (не пустой URL, который
	// включил бы его снова); эхом идёт в форму скрытым полем DefaultSuppressed.
	defaultSuppressed := q.Get("nodefault") != ""
	var defaultFilter *logfilter.Filter
	var showAllHref string
	if h.LogFilters != nil && !defaultSuppressed && !hasLogFilterParams(q) {
		if def, ok, err := h.LogFilters.Default(r.Context(), projectID, uid); err != nil {
			slog.Warn("logs: default filter unavailable", "project_id", projectID, "err", err)
		} else if ok && def.Applicable {
			// Applicable=false не применяется явным условием, а не как случайное
			// следствие пустого Predicates (applyPredicates(nil) тоже был бы no-op).
			applyPredicates(&f, def.Predicates)
			defaultFilter = &def
			showAllQuery := url.Values{}
			for k, v := range q {
				showAllQuery[k] = v
			}
			showAllQuery.Set("nodefault", "1")
			showAllHref = templates.LogsURLFromValues(projectID, showAllQuery)
		}
	}

	rows, listErr := h.LogQuery.List(r.Context(), projectID, f)
	// Ошибка ClickHouse — не 500: список остаётся видимым (фильтры не пропадают), просто пуст.
	loadFailed := listErr != nil
	if loadFailed {
		slog.Warn("logs: list failed", "project_id", projectID, "err", listErr)
		rows = nil
	}

	vmRows := make([]templates.LogRow, len(rows))
	for i, row := range rows {
		vmRows[i] = templates.NewLogRow(row)
	}

	filter := templates.LogsFilter{
		Severity:    f.Severity,
		Service:     f.Service,
		Environment: f.Environment,
		Query:       f.Query,
		Attrs:       f.Attrs,
		TraceID:     f.TraceID,
		Not:         f.Not,
		Range:       timeRangeVM(rng),
		Active: len(f.Severity) > 0 || f.Service != "" || f.Environment != "" || f.Query != "" || len(f.Attrs) > 0 ||
			f.TraceID != "" || len(f.Not) > 0 || rng.Key != "24h",
		Facet:              q.Get("facet"),
		RangeClamped:       rangeClamped,
		RetentionDays:      h.LogRetentionDays,
		DefaultApplied:     defaultFilter,
		DefaultShowAllHref: showAllHref,
		DefaultSuppressed:  defaultSuppressed,
	}

	var olderHref string
	if before, tieSkip, hasMore := nextLogCursor(f, rows); hasMore {
		olderHref = templates.LogsPageURL(projectID, filter, before, tieSkip)
	}

	var histogram templates.LogsHistogram
	var facets templates.LogFacets
	if loadFailed {
		histogram = templates.LogsHistogram{Empty: true}
		facets = templates.LogFacets{
			Severity:    templates.LogFacet{TooMuchData: true},
			Service:     templates.LogFacet{TooMuchData: true},
			Environment: templates.LogFacet{TooMuchData: true},
			Attrs:       templates.LogAttrFacets{TooMuchData: true},
		}
	} else {
		histogram = h.logsHistogram(r.Context(), projectID, f)
		facets = h.logsFacets(r.Context(), projectID, f, filter, filter.Facet)
	}

	panel := h.logFiltersPanel(r.Context(), projectID, uid)

	// WriteHeader(status) отправляет заголовки раньше первого Write, поэтому
	// автоопределение Content-Type сниффингом не срабатывает при статусе не 200.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_ = templates.LogsScreen(projectID, vmRows, filter, loadFailed, olderHref, histogram, facets, h.currentEmail(r), panel, errMsg).Render(r.Context(), w)
}

func (h *Handler) logsAttrKeys(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	projectID, ok := h.parsePathProjectID(w, r)
	if !ok {
		return
	}
	if h.LogQuery == nil {
		h.notFound(w, r)
		return
	}
	canAccess, err := h.Org.CanAccessProject(r.Context(), uid, projectID)
	if err != nil {
		http.Error(w, i18n.T(r.Context(), "error.internal"), http.StatusInternalServerError)
		return
	}
	if !canAccess {
		h.notFound(w, r)
		return
	}

	prefix := r.URL.Query().Get("q")
	period, start, end := attrKeysWindowKey(r.URL.Query())

	if h.attrKeysCache != nil {
		if values, hit := h.attrKeysCache.get(projectID, prefix, period, start, end); hit {
			writeAttrKeysJSON(w, values)
			return
		}
	}

	rng := h.resolveTimeRange(w, r, "24h")
	from, _ := clampLogRetention(rng.From, h.LogRetentionDays)
	// trace_id не входит в ключ attrKeysCache: иначе typeahead одного трейса кэшировал
	// бы ключи под тем же ключом, что и ключи всего окна, и отдавал бы их другому трейсу.
	f := log.ListFilter{From: from, To: rng.To}
	values, err := h.LogQuery.AttrKeys(r.Context(), projectID, f, prefix, logsAttrKeysAutocompleteLimit)
	if err != nil {
		slog.Warn("logs: attr keys autocomplete failed", "project_id", projectID, "err", err)
		writeAttrKeysJSON(w, nil)
		return
	}
	if h.attrKeysCache != nil {
		h.attrKeysCache.put(projectID, prefix, period, start, end, values)
	}
	writeAttrKeysJSON(w, values)
}

type attrKeyJSON struct {
	Key   string `json:"key"`
	Count int64  `json:"count"`
}

func writeAttrKeysJSON(w http.ResponseWriter, values []log.FacetValue) {
	out := make([]attrKeyJSON, len(values))
	for i, v := range values {
		out[i] = attrKeyJSON{Key: v.Value, Count: v.Count}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func (h *Handler) logsHistogram(ctx context.Context, projectID int64, f log.ListFilter) templates.LogsHistogram {
	times, series, err := h.LogQuery.Histogram(ctx, projectID, f, logsHistogramBuckets)
	if err != nil {
		slog.Warn("logs: histogram failed", "project_id", projectID, "err", err)
		return templates.LogsHistogram{Empty: true}
	}
	if !logsHistogramHasData(series) {
		return templates.LogsHistogram{Empty: true}
	}

	legend := make([]templates.LegendItem, len(log.Severities))
	for i, sev := range log.Severities {
		legend[i] = templates.LegendItem{Label: i18n.T(ctx, "logs.severity."+sev), Class: "legend-sev-" + sev}
	}
	return templates.LogsHistogram{
		Chart:  logHistogramSVG(ctx, times, series, latencyChartWidth, latencyChartHeight),
		Legend: legend,
	}
}

func (h *Handler) logsFacets(ctx context.Context, projectID int64, f log.ListFilter, filter templates.LogsFilter, expandedAttrKey string) templates.LogFacets {
	sevValues, sevErr := h.LogQuery.Facet(ctx, projectID, f, "severity")
	if sevErr != nil {
		slog.Warn("logs: facet failed", "project_id", projectID, "col", "severity", "err", sevErr)
	}
	svcValues, svcErr := h.LogQuery.Facet(ctx, projectID, f, "service")
	if svcErr != nil {
		slog.Warn("logs: facet failed", "project_id", projectID, "col", "service", "err", svcErr)
	}
	envValues, envErr := h.LogQuery.Facet(ctx, projectID, f, "environment")
	if envErr != nil {
		slog.Warn("logs: facet failed", "project_id", projectID, "col", "environment", "err", envErr)
	}
	return templates.LogFacets{
		Severity:    templates.NewSeverityFacet(ctx, projectID, filter, sevValues, sevErr != nil),
		Service:     templates.NewServiceFacet(ctx, projectID, filter, svcValues, svcErr != nil),
		Environment: templates.NewEnvironmentFacet(ctx, projectID, filter, envValues, envErr != nil),
		Attrs:       h.logsAttrFacets(ctx, projectID, f, filter, expandedAttrKey),
	}
}

// AttrKeys обнаруживает ключи только в log_attributes, не в resource_attrs — раскрытый
// ключ сайдбара всегда оттуда же; resource_attrs доступен только через ручной ?attr=res:key:value.
func (h *Handler) logsAttrFacets(ctx context.Context, projectID int64, f log.ListFilter, filter templates.LogsFilter, expandedKey string) templates.LogAttrFacets {
	keys, err := h.LogQuery.AttrKeys(ctx, projectID, f, "", logsAttrKeysLimit)
	if err != nil {
		slog.Warn("logs: attr keys failed", "project_id", projectID, "err", err)
		return templates.LogAttrFacets{TooMuchData: true}
	}

	var values []log.FacetValue
	if expandedKey != "" {
		var valuesErr error
		values, valuesErr = h.LogQuery.AttrValues(ctx, projectID, f, false, expandedKey, logsAttrValuesLimit)
		if valuesErr != nil {
			slog.Warn("logs: attr values failed", "project_id", projectID, "key", expandedKey, "err", valuesErr)
			values = nil
		}
	}

	return templates.NewAttrFacets(ctx, projectID, filter, keys, expandedKey, values)
}

func logsHistogramHasData(series map[string][]int64) bool {
	for _, vals := range series {
		for _, v := range vals {
			if v > 0 {
				return true
			}
		}
	}
	return false
}

// retentionDays<=0 — хранение бессрочно, кламп невозможен.
func clampLogRetention(from time.Time, retentionDays int) (clampedFrom time.Time, clamped bool) {
	if retentionDays <= 0 {
		return from, false
	}
	if cutoff := time.Now().Add(-time.Duration(retentionDays) * 24 * time.Hour); from.Before(cutoff) {
		return cutoff, true
	}
	return from, false
}

func parseLogFilter(q url.Values, rng TimeRange, retentionDays int) (f log.ListFilter, clamped bool) {
	from, clamped := clampLogRetention(rng.From, retentionDays)

	f = log.ListFilter{
		From:        from,
		To:          rng.To,
		Service:     q.Get("service"),
		Environment: q.Get("environment"),
		Query:       q.Get("q"),
		TraceID:     q.Get("trace_id"),
		Limit:       logsListLimit,
	}

	for _, sv := range q["severity"] {
		if slices.Contains(log.Severities, sv) {
			f.Severity = append(f.Severity, sv)
		}
	}

	for _, raw := range q["attr"] {
		if af, ok := parseLogAttrFilter(raw); ok {
			f.Attrs = append(f.Attrs, af)
		}
	}

	if beforeMS := q.Get("before"); beforeMS != "" {
		if ms, err := strconv.ParseInt(beforeMS, 10, 64); err == nil && ms > 0 {
			f.Before = time.UnixMilli(ms).UTC()
			f.TieSkip, _ = strconv.Atoi(q.Get("tskip")) // невалидное/пустое tskip — 0, что и так дефолт Atoi-ошибки
			if f.TieSkip < 0 {
				f.TieSkip = 0 // защита от отрицательного из URL; верхний потолок ставит сам log.List (maxListLimit)
			}
		}
	}

	var not []log.Predicate
	for _, v := range q["q_not"] {
		not = append(not, log.Predicate{Field: log.FieldBody, Op: log.OpNotContains, Value: v})
	}
	for _, v := range q["severity_not"] {
		not = append(not, log.Predicate{Field: log.FieldSeverity, Op: log.OpNeq, Value: v})
	}
	for _, v := range q["service_not"] {
		not = append(not, log.Predicate{Field: log.FieldService, Op: log.OpNeq, Value: v})
	}
	for _, v := range q["environment_not"] {
		not = append(not, log.Predicate{Field: log.FieldEnvironment, Op: log.OpNeq, Value: v})
	}
	for _, raw := range q["attr_not"] {
		if af, ok := parseLogAttrFilter(raw); ok {
			field := log.FieldAttr
			if af.Resource {
				field = log.FieldResourceAttr
			}
			not = append(not, log.Predicate{Field: field, Key: af.Key, Op: log.OpNeq, Value: af.Value})
		}
	}
	// Разбор URL мягкий: мусор в ссылке молча игнорируется — ссылки правят руками
	// и пересылают, ронять страницу на них нельзя.
	not = log.NormalizePredicates(not)
	if len(not) > maxNegativeConditions {
		not = not[:maxNegativeConditions]
	}
	f.Not = not

	return f, clamped
}

// остаток делится по ПЕРВОМУ ":" на ключ/значение — значение само может содержать
// двоеточие (например URL).
func parseLogAttrFilter(raw string) (log.AttrFilter, bool) {
	resource := false
	if rest, ok := strings.CutPrefix(raw, "res:"); ok {
		resource = true
		raw = rest
	}
	key, value, ok := strings.Cut(raw, ":")
	if !ok {
		return log.AttrFilter{}, false
	}
	return log.AttrFilter{Resource: resource, Key: key, Value: value}, true
}

// Пагинация/facet/период не входят в logFilterParams: они не сужают выборку сами
// по себе, а окно задано всегда, чистый заход по нему не отличить.
func hasLogFilterParams(q url.Values) bool {
	for _, name := range logFilterParams {
		if len(q[name]) > 0 {
			return true
		}
	}
	return false
}

func applyPredicates(f *log.ListFilter, preds []log.Predicate) {
	log.ApplyPredicates(f, preds)
}

func filterToPredicates(f log.ListFilter) []log.Predicate {
	var out []log.Predicate
	for _, sv := range f.Severity {
		out = append(out, log.Predicate{Field: log.FieldSeverity, Op: log.OpEq, Value: sv})
	}
	if f.Service != "" {
		out = append(out, log.Predicate{Field: log.FieldService, Op: log.OpEq, Value: f.Service})
	}
	if f.Environment != "" {
		out = append(out, log.Predicate{Field: log.FieldEnvironment, Op: log.OpEq, Value: f.Environment})
	}
	if f.Query != "" {
		out = append(out, log.Predicate{Field: log.FieldBody, Op: log.OpContains, Value: f.Query})
	}
	if f.TraceID != "" {
		out = append(out, log.Predicate{Field: log.FieldTraceID, Op: log.OpEq, Value: f.TraceID})
	}
	for _, a := range f.Attrs {
		field := log.FieldAttr
		if a.Resource {
			field = log.FieldResourceAttr
		}
		out = append(out, log.Predicate{Field: field, Key: a.Key, Op: log.OpEq, Value: a.Value})
	}
	out = append(out, f.Not...)
	return out
}

// TieSkip накапливается, а не пересчитывается заново: если хвостовой Before не
// меняется между страницами (тай растянут на несколько страниц), иначе дубль строк.
func nextLogCursor(f log.ListFilter, rows []log.LogRow) (before time.Time, tieSkip int, hasMore bool) {
	limit := f.Limit
	if limit <= 0 {
		limit = logsListLimit
	}
	if len(rows) == 0 || len(rows) < limit {
		return time.Time{}, 0, false
	}
	last := rows[len(rows)-1]
	matches := 0
	for _, r := range rows {
		if r.Timestamp.Equal(last.Timestamp) {
			matches++
		}
	}
	if !f.Before.IsZero() && last.Timestamp.Equal(f.Before) {
		tieSkip = f.TieSkip + matches
	} else {
		tieSkip = matches
	}
	return last.Timestamp, tieSkip, true
}
