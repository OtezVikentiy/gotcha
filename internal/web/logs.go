package web

import (
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
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

// logsList — GET /projects/{id}/logs: просмотрщик логов проекта (задача 2 плана
// C2): фильтры (severity/service/environment/тело/окно времени), список с
// раскрытием строки (полное тело + атрибуты + trace/span), курсорная
// пагинация «показать старее». Гистограмма, фасеты и автокомплит атрибутов —
// задачи T3-T6, здесь их нет.
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
		Range:       timeRangeVM(rng),
		Active: len(f.Severity) > 0 || f.Service != "" || f.Environment != "" || f.Query != "" ||
			rng.Key != "24h",
	}

	var olderHref string
	if before, tieSkip, hasMore := nextLogCursor(f, rows); hasMore {
		olderHref = templates.LogsPageURL(projectID, filter, before, tieSkip)
	}

	_ = templates.LogsScreen(projectID, vmRows, filter, loadFailed, olderHref, h.currentEmail(r)).Render(r.Context(), w)
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
