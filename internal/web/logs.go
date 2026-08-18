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
	"gitflic.ru/otezvikentiy/gotcha/internal/web/templates"
)

// logsListLimit — сколько строк логов запрашивать за одну страницу (задача 2,
// C2). Курсорная пагинация («показать старее») добирает следующие такого же
// размера — полноценный постраничный счётчик, как у issues, здесь не нужен:
// ClickHouse keyset-курсор (log.ListFilter.Before/TieSkip) не считает total.
const logsListLimit = 100

// logsHistogramBuckets — целевое число корзин гистограммы объёма (задача 3,
// C2). График переиспользует viewBox стека задержек монитора (720×160,
// latencyChartWidth/Height в svg.go) — тот же ориентир числа корзин на
// ширину, что и monitorLatencyBuckets.
const logsHistogramBuckets = 48

// logsAttrKeysLimit/logsAttrValuesLimit — размер топа атрибут-фасетов
// (задача 5, C2, §4 спеки): 20 ключей в сайдбаре, 10 значений раскрытого
// ключа (тот же порядок величины, что facetLimit у встроенных фасетов).
const (
	logsAttrKeysLimit   = 20
	logsAttrValuesLimit = 10
)

// logsAttrKeysAutocompleteLimit — сколько ключей отдаёт JSON-эндпоинт
// автокомплита (задача 6, C2, §6 спеки): чуть шире, чем сайдбар
// (logsAttrKeysLimit), — typeahead ищет по произвольному префиксу среди ВСЕХ
// обнаруженных ключей, не только топ-20 без фильтра, так что список
// совпадений с непустым q может быть длиннее без потери релевантности.
const logsAttrKeysAutocompleteLimit = 20

// logsAttrKeysAutocompleteWindow — окно, за которое ищутся ключи для
// автокомплита (задача 6, §6 спеки: «окно = последние 24ч (или текущее окно
// фильтра, если передано)»). Эндпоинт вызывается отдельным fetch без
// остальных query-параметров экрана (logs.js бьёт только по q=<prefix>), так
// что текущее окно фильтра ему недоступно — фиксированные последние 24ч,
// тот же дефолт, что и у самого экрана логов (resolveTimeRange(...,"24h")).
const logsAttrKeysAutocompleteWindow = 24 * time.Hour

// attrKeysCacheTTL — время жизни одной записи кеша автокомплита ключей
// атрибутов (задача 6, C2, §6 спеки): «кеш per-project ~60с — авто-
// обнаружение ключей не обязано быть свежим до секунды». Тот же принцип, что
// у KeyCache ingest (internal/ingest/auth.go) — только здесь кешируется
// результат AttrKeys, а не резолв ключа проекта.
const attrKeysCacheTTL = 60 * time.Second

// maxAttrKeysCacheEntries — верхняя граница числа кешируемых комбинаций
// (projectID, prefix). prefix вводит пользователь произвольным текстом —
// без потолка поток разных префиксов раздул бы карту неограниченно (тот же
// класс дефекта, что maxKeyCacheEntries/maxRateLimitKeys в internal/ingest).
// При переполнении карта очищается целиком: записи короткоживущие (TTL 60с)
// и дёшевы для пересчёта, ярусное вытеснение как у KeyCache здесь не того
// стоит.
const maxAttrKeysCacheEntries = 10000

// attrKeysCacheKey — ключ кеша: пара (projectID, prefix) — тот же набор, что
// определяет результат AttrKeys для автокомплита (окно фиксировано, см.
// logsAttrKeysAutocompleteWindow).
type attrKeysCacheKey struct {
	projectID int64
	prefix    string
}

type attrKeysCacheEntry struct {
	values  []log.FacetValue
	expires time.Time
}

// attrKeysCache — per-(projectID, prefix) кеш ответов AttrKeys для
// автокомплита (задача 6, C2). now инжектируется для тестов без реального
// sleep (тот же приём, что KeyCache.now в internal/ingest/auth.go).
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

// get возвращает закешированные значения, если запись есть и не истекла.
func (c *attrKeysCache) get(projectID int64, prefix string) ([]log.FacetValue, bool) {
	key := attrKeysCacheKey{projectID: projectID, prefix: prefix}
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok || !e.expires.After(now) {
		return nil, false
	}
	return e.values, true
}

// put кладёт результат в кеш на attrKeysCacheTTL, вытесняя карту целиком при
// переполнении (см. maxAttrKeysCacheEntries).
func (c *attrKeysCache) put(projectID int64, prefix string, values []log.FacetValue) {
	key := attrKeysCacheKey{projectID: projectID, prefix: prefix}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) >= maxAttrKeysCacheEntries {
		c.entries = map[attrKeysCacheKey]attrKeysCacheEntry{}
	}
	c.entries[key] = attrKeysCacheEntry{values: values, expires: c.now().Add(attrKeysCacheTTL)}
}

// logsList — GET /projects/{id}/logs: просмотрщик логов проекта (задача 2 плана
// C2): фильтры (severity/service/environment/тело/окно времени), список с
// раскрытием строки (полное тело + атрибуты + trace/span), курсорная
// пагинация «показать старее», гистограмма объёма по времени и severity
// (задача 3), встроенные фасеты (задача 4) и атрибут-фасеты — авто-
// обнаруженные ключи Map-колонок с ленивыми значениями (задача 5, ядро-
// дифференциатор фичи). Автокомплит ключей — отдельный JSON-эндпоинт
// logsAttrKeys (задача 6, ниже).
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
	// h.LogQuery может быть nil в стендах без проводки логов (main.go
	// проставляет его только вместе с ClickHouse) — тогда честный 404, а не
	// паника на разыменовании (тот же приём, что у h.Metrics/h.Trace).
	if h.LogQuery == nil {
		h.notFound(w, r)
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

	// Окно времени резолвится один раз общим контролом (даёт cookie-липкость
	// и внутренний кламп на 90 дней), затем ЕЩЁ РАЗ обрезается по фактическому
	// TTL хранения логов (h.LogRetentionDays) — иначе запрос за окно шире
	// TTL сканирует партиции, в которых данных гарантированно уже нет.
	// Пресеты UI при этом не трогаем: слишком глубокий пресет просто даёт
	// обрезанное окно, это осознанное упрощение MVP (см. бриф задачи 2).
	rng := h.resolveTimeRange(w, r, "24h")
	q := r.URL.Query()
	f := parseLogFilter(q, rng, h.LogRetentionDays)

	rows, listErr := h.LogQuery.List(r.Context(), projectID, f)
	// Ошибка чтения ClickHouse — НЕ 500: логи вспомогательный раздел, который
	// может недомогать независимо от остального продукта (временная
	// недоступность CH-кластера, приём логов ещё не настроен). Дружелюбное
	// сообщение прямо на экране вместо стилизованной страницы ошибки —
	// список остаётся видимым (фильтры не пропадают), просто пуст.
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
		Range:       timeRangeVM(rng),
		Active: len(f.Severity) > 0 || f.Service != "" || f.Environment != "" || f.Query != "" || len(f.Attrs) > 0 ||
			rng.Key != "24h",
	}

	var olderHref string
	if before, tieSkip, hasMore := nextLogCursor(f, rows); hasMore {
		olderHref = templates.LogsPageURL(projectID, filter, before, tieSkip)
	}

	// loadFailed уже означает, что List не смог прочитать ClickHouse —
	// гистограмма и фасеты пропускаются тоже (Minor из ревью задачи 3: не
	// бить лишними запросами по CH, который уже недоступен) и приходят в
	// своём собственном состоянии отказа без похода в БД.
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
		facets = h.logsFacets(r.Context(), projectID, f, filter, q.Get("facet"))
	}

	_ = templates.LogsScreen(projectID, vmRows, filter, loadFailed, olderHref, histogram, facets, h.currentEmail(r)).Render(r.Context(), w)
}

// logsAttrKeys — GET /projects/{id}/logs/attr-keys?q=<prefix>: JSON-эндпоинт
// автокомплита ключей атрибутов (задача 6, C2, §6 спеки), потребляется
// typeahead полем поиска атрибута в сайдбаре (static/logs.js). Тот же гейт
// доступа, что у logsList (auth → parsePathProjectID → h.LogQuery==nil →
// 404 → CanAccessProject → 404): эндпоинт открывает те же данные (имена и
// частоты ключей log_attributes), что и сам список логов, изоляция проекта
// обязана быть той же.
//
// Окно фиксировано (logsAttrKeysAutocompleteWindow, последние 24ч) —
// текущее окно фильтра экрана сюда не передаётся, JS бьёт отдельным fetch
// только с q=<prefix>. Результат кешируется на attrKeysCacheTTL per
// (projectID, prefix): авто-обнаружение ключей не обязано быть свежим до
// секунды, а быстрый набор текста иначе бил бы по ClickHouse на каждый
// символ (клиентский дебаунс сглаживает, но не исключает гонки/повторы).
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

	if h.attrKeysCache != nil {
		if values, hit := h.attrKeysCache.get(projectID, prefix); hit {
			writeAttrKeysJSON(w, values)
			return
		}
	}

	now := time.Now().UTC()
	f := log.ListFilter{From: now.Add(-logsAttrKeysAutocompleteWindow), To: now}
	values, err := h.LogQuery.AttrKeys(r.Context(), projectID, f, prefix, logsAttrKeysAutocompleteLimit)
	if err != nil {
		// Та же деградация, что и у logsAttrFacets: предупреждение в лог,
		// клиенту — пустой список (typeahead просто ничего не покажет), а не
		// 500 — автокомплит вспомогателен, его сбой не должен мешать ручному
		// вводу в остальные фильтры экрана.
		slog.Warn("logs: attr keys autocomplete failed", "project_id", projectID, "err", err)
		writeAttrKeysJSON(w, nil)
		return
	}
	if h.attrKeysCache != nil {
		h.attrKeysCache.put(projectID, prefix, values)
	}
	writeAttrKeysJSON(w, values)
}

// attrKeyJSON — форма одного элемента ответа logsAttrKeys: log.FacetValue
// сериализуется под именами, которых ждёт logs.js (key/count), а не под
// именем Value/Count — оставлять поля FacetValue экспортируемыми как есть
// смешало бы вью-контракт JSON-эндпоинта с внутренним типом пакета log.
type attrKeyJSON struct {
	Key   string `json:"key"`
	Count int64  `json:"count"`
}

func writeAttrKeysJSON(w http.ResponseWriter, values []log.FacetValue) {
	out := make([]attrKeyJSON, len(values))
	for i, v := range values {
		out[i] = attrKeyJSON{Key: v.Value, Count: v.Count}
	}
	// Content-Type — тот же голый "application/json" без charset, что и у
	// остальных JSON-эндпоинтов web (probeapi.go writeProbeJSON, heartbeat.go).
	// Cache-Control: no-store уже проставлен глобально (securityHeaders) — сам
	// список ключей атрибутов вспомогателен, но может отражать имена
	// приватных полей приложения, отдельно повторять заголовок незачем.
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// logsHistogram считает гистограмму объёма (задача 3, C2) по тому же фильтру
// f, что и список (окно+severity/service/environment/полнотекст/attrs) — БЕЗ
// курсора: log.Query.Histogram сам его игнорирует, но f сюда передаётся
// целиком, чтобы гистограмма отражала те же условия, что и видимый список.
// Ошибка чтения ClickHouse — та же деградация, что и у List: предупреждение в
// лог, график молча скрывается (Empty=true), страница не падает.
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

// logsFacets считает три встроенных фасета (задача 4, C2): severity/service/
// environment — Query.Facet по тому же фильтру f, что и список/гистограмма.
// Ошибка/таймаут отдельного фасета (SETTINGS max_execution_time=5 внутри
// Query.Facet) — та же деградация, что и у logsHistogram: предупреждение в
// лог, секция рендерится пустой с пометкой «слишком много данных» вместо
// падения всей страницы (тот же принцип, что и у logsHistogram); остальные
// две секции при этом считаются независимо — падение одного фасета не тянет
// за собой другие.
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
		Service:     templates.NewServiceFacet(projectID, filter, svcValues, svcErr != nil),
		Environment: templates.NewEnvironmentFacet(projectID, filter, envValues, envErr != nil),
		Attrs:       h.logsAttrFacets(ctx, projectID, f, filter, expandedAttrKey),
	}
}

// logsAttrFacets считает секцию атрибут-фасетов сайдбара (задача 5, C2,
// ядро-дифференциатор): авто-обнаруженные ключи log_attributes
// (log.Query.AttrKeys, ограниченная свежая выборка — см. её комментарий) со
// счётчиками; если expandedKey непуст (?facet=<key> в URL) — для НЕГО ЖЕ
// дополнительно считаются значения (log.Query.AttrValues), остальные ключи
// остаются только счётчиком (ленивая подгрузка, §4 спеки C2). resource=false
// у AttrValues — атрибут-фасеты MVP раскрывают только log_attributes:
// AttrKeys (сайдбар) обнаруживает ключи ТОЛЬКО в log_attributes (§4 спеки —
// resource_attrs авто-обнаружению не подлежит), так что раскрытый ключ
// всегда оттуда же; resource_attrs остаётся доступен через ручной
// ?attr=res:key:value (§3), но не через сайдбар этой задачи.
//
// Деградация — по уровням, а не всё-или-ничего: ошибка/таймаут AttrKeys
// (SETTINGS max_execution_time=5) — вся секция пустеет с пометкой, как и
// встроенные фасеты; ошибка AttrValues раскрытого ключа НЕ рушит список
// ключей — тот же ключ просто рендерится раскрытым, но без значений.
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

	return templates.NewAttrFacets(projectID, filter, keys, expandedKey, values)
}

// logsHistogramHasData — во всех корзинах всех severity одни нули (окно
// пустое или все строки отфильтрованы) — тот же случай, что «нет данных»,
// график скрывается целиком, а не рисует плоскую шкалу без смысла.
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

// parseLogFilter собирает log.ListFilter из query-параметров и уже
// резолвленного окна времени rng (h.resolveTimeRange(w,r,"24h") — см.
// logsList). Вынесена в чистую функцию без (h, w): resolveTimeRange
// вызывается ровно один раз в logsList (его побочный эффект — запись cookie
// окна), а результирующий rng нужен ОБА раза — и для фильтра List, и для
// вью-модели формы (templates.LogsFilter.Range) — так что дублировать вызов
// незачем.
func parseLogFilter(q url.Values, rng TimeRange, retentionDays int) log.ListFilter {
	from := rng.From
	if retentionDays > 0 {
		if cutoff := time.Now().Add(-time.Duration(retentionDays) * 24 * time.Hour); from.Before(cutoff) {
			from = cutoff
		}
	}

	f := log.ListFilter{
		From:        from,
		To:          rng.To,
		Service:     q.Get("service"),
		Environment: q.Get("environment"),
		Query:       q.Get("q"),
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
		}
	}

	return f
}

// parseLogAttrFilter разбирает значение повторяющегося ?attr=[res:]key:value:
// необязательный префикс "res:" переключает фильтр на resource_attrs (иначе
// log_attributes), остаток делится по ПЕРВОМУ ":" на ключ/значение — значение
// само может содержать двоеточие (например URL). Без разделителя — ok=false,
// вызывающий молча игнорирует такой параметр (не роняет страницу на кривом
// ручном query).
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

// nextLogCursor вычисляет курсор («before», «tieSkip») для ссылки «показать
// старее» по уже полученной странице rows. Накопление TieSkip — та же логика,
// что и в тестовом цикле постраничного обхода internal/log/query_test.go
// («cursor pagination across timestamp tie»): если хвостовой Before между
// страницами НЕ меняется (тай растягивается больше чем на одну страницу),
// счётчик прибавляется, а не пересчитывается заново — иначе следующая
// страница переспросит уже показанные строки этой тай-группы и вернёт дубль.
//
// hasMore — эвристика без total: страница заполнена ровно до Limit, значит
// за курсором вероятно есть ещё строки. Пустая страница или страница короче
// Limit — конец списка, ссылки не будет.
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
