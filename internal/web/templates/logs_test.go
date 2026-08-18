package templates

import (
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/log"
)

// parseLogsLink разбирает URL, построенный хелперами контекст-ссылок C3, и
// отдаёт его query-параметры для проверки. Заодно проверяет, что путь — тот же
// относительный /projects/{id}/logs (не абсолютный, не чужой).
func parseLogsLink(t *testing.T, raw string, projectID int64) url.Values {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("не разобрать ссылку %q: %v", raw, err)
	}
	if want := logsPath(projectID); u.Path != want {
		t.Fatalf("путь ссылки = %q, ожидался %q", u.Path, want)
	}
	return u.Query()
}

// unixWindow достаёт start/end из query и проверяет, что они заданы, парсятся
// как unix-секунды и образуют невырожденное окно (start < end). Возвращает обе
// границы для дополнительных проверок.
func unixWindow(t *testing.T, q url.Values) (from, to time.Time) {
	t.Helper()
	s, e := q.Get("start"), q.Get("end")
	if s == "" || e == "" {
		t.Fatalf("окно неполно: start=%q end=%q", s, e)
	}
	si, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		t.Fatalf("start не unix-секунды: %q", s)
	}
	ei, err := strconv.ParseInt(e, 10, 64)
	if err != nil {
		t.Fatalf("end не unix-секунды: %q", e)
	}
	if si >= ei {
		t.Fatalf("окно вырождено: start=%d >= end=%d", si, ei)
	}
	return time.Unix(si, 0).UTC(), time.Unix(ei, 0).UTC()
}

// TestLogsAroundEventWindowAlways — ключевой инвариант блокера ревью плана:
// «Логи вокруг события» ВСЕГДА задают временное окно, в ОБЕИХ ветках (с trace_id
// и без) — без окна /logs берёт дефолтные 24ч и отсёк бы логи события старше
// суток.
func TestLogsAroundEventWindowAlways(t *testing.T) {
	ts := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)

	// Ветка с trace_id: окно есть И trace_id добавлен поверх, environment нет.
	q := parseLogsLink(t, logsAroundEventPath(1, "tr-abc", ts, "prod"), 1)
	from, to := unixWindow(t, q)
	if q.Get("trace_id") != "tr-abc" {
		t.Fatalf("trace_id-ветка: trace_id=%q, ожидался tr-abc", q.Get("trace_id"))
	}
	if q.Get("environment") != "" {
		t.Fatalf("trace_id-ветка не должна ставить environment, получено %q", q.Get("environment"))
	}
	if !from.Before(ts) || !to.After(ts) {
		t.Fatalf("окно [%v,%v] не окружает событие %v", from, to, ts)
	}

	// Ветка без trace_id: окно есть, скоуп по environment, trace_id нет.
	q2 := parseLogsLink(t, logsAroundEventPath(1, "", ts, "prod"), 1)
	unixWindow(t, q2)
	if q2.Get("trace_id") != "" {
		t.Fatalf("без-trace-ветка не должна ставить trace_id, получено %q", q2.Get("trace_id"))
	}
	if q2.Get("environment") != "prod" {
		t.Fatalf("без-trace-ветка: environment=%q, ожидался prod", q2.Get("environment"))
	}

	// Без trace_id и без environment: только окно, ничего лишнего.
	q3 := parseLogsLink(t, logsAroundEventPath(1, "", ts, ""), 1)
	unixWindow(t, q3)
	if q3.Get("trace_id") != "" || q3.Get("environment") != "" {
		t.Fatalf("пустая ветка должна нести только окно, получено trace_id=%q env=%q", q3.Get("trace_id"), q3.Get("environment"))
	}
}

// TestLogsForTraceSaturatedWindow — аудит QA P1: TotalUS насыщается на ^uint32(0)
// (~71 мин) у очень длинных трейсов; тесное окно отсекло бы хвост логов. При
// насыщении окно обязано быть заведомо широким (>=24ч от начала), а не ~71 мин.
func TestLogsForTraceSaturatedWindow(t *testing.T) {
	from := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)

	// Нормальный трейс: окно ~= длительность (+1с), несколько секунд.
	qn := parseLogsLink(t, logsForTracePath(1, "tr", from, 5_000_000 /* 5s */), 1)
	_, tn := unixWindow(t, qn)
	if span := tn.Sub(from); span > time.Minute {
		t.Fatalf("нормальный трейс: окно %v неожиданно широкое", span)
	}

	// Насыщенный TotalUS: окно должно быть >=24ч, а не ~71 мин.
	qs := parseLogsLink(t, logsForTracePath(1, "tr", from, ^uint32(0)), 1)
	_, ts := unixWindow(t, qs)
	if span := ts.Sub(from); span < 24*time.Hour {
		t.Fatalf("насыщенный TotalUS: окно %v < 24ч — хвост логов длинного трейса был бы отсечён", span)
	}
	if qs.Get("trace_id") != "tr" {
		t.Fatalf("trace_id должен присутствовать при любом окне")
	}
}

// TestLogsForHostPathEncoding — host→логи: значение host.name с спецсимволами
// (точки, дефисы, двоеточия) должно корректно кодироваться в ?attr=res:host.name:…
// и переживать разбор web.parseLogAttrFilter (режет по ПЕРВОМУ ":").
func TestLogsForHostPathEncoding(t *testing.T) {
	q := parseLogsLink(t, logsForHostPath(1, "web-01.dc:eu"), 1)
	attr := q.Get("attr")
	want := "res:host.name:web-01.dc:eu"
	if attr != want {
		t.Fatalf("attr=%q, ожидался %q (первое ':' делит префикс/ключ/значение, двоеточия значения сохраняются)", attr, want)
	}
}

// TestLogAttrChipRemoveURL — снятие attr-чипа убирает ИМЕННО целевой фильтр (по
// Resource+Key+Value), не трогая остальные; работает и для resource-атрибута
// (host.name из ссылки «Логи хоста»), у которого фасета в сайдбаре нет.
func TestLogAttrChipRemoveURL(t *testing.T) {
	f := LogsFilter{Attrs: []log.AttrFilter{
		{Resource: true, Key: "host.name", Value: "web-01"},
		{Resource: false, Key: "http.method", Value: "GET"},
	}}
	target := log.AttrFilter{Resource: true, Key: "host.name", Value: "web-01"}
	q := parseLogsLink(t, logAttrChipRemoveURL(1, f, target), 1)
	attrs := q["attr"]
	if len(attrs) != 1 || attrs[0] != "http.method:GET" {
		t.Fatalf("после снятия host.name остаться должен только http.method:GET, получено %v", attrs)
	}
}

// TestNewAttrFacetsExpandedKeyOutsideTop — carry-fix из ревью задачи T5
// (задача 6, C2): раскрытый в URL ключ (?facet=<key>), найденный автокомплитом
// (web.logsAttrKeys, задача 6), может не входить в топ-N сайдбара
// (log.Query.AttrKeys с ограниченной выборкой). До фикса цикл NewAttrFacets
// искал expandedKey ТОЛЬКО среди keys — если совпадения не было, посчитанные
// для него values нигде не оказывались: клик по такому ключу из автокомплита
// визуально ничего не раскрывал. Теперь для него добавляется синтетический
// элемент списка с values.
func TestNewAttrFacetsExpandedKeyOutsideTop(t *testing.T) {
	keys := []log.FacetValue{
		{Value: "http.method", Count: 100},
		{Value: "http.status_code", Count: 80},
	}
	values := []log.FacetValue{
		{Value: "prod", Count: 7},
		{Value: "staging", Count: 3},
	}

	got := NewAttrFacets(1, LogsFilter{}, keys, "environment.tier", values)

	if len(got.Keys) != 3 {
		t.Fatalf("Keys len = %d, want 3 (2 из топа + 1 синтетический): %+v", len(got.Keys), got.Keys)
	}

	// Синтетический элемент раскрытого ключа вне топа — с values и
	// Expanded=true, иначе то же самое, что "кликнул из автокомплита —
	// ничего не раскрылось".
	item := got.Keys[0]
	if item.Key != "environment.tier" {
		t.Fatalf("первый элемент Key = %q, want %q (синтетический элемент ожидается в начале списка): %+v", item.Key, "environment.tier", got.Keys)
	}
	if !item.Expanded {
		t.Fatalf("синтетический элемент должен быть Expanded=true: %+v", item)
	}
	if len(item.Values) != 2 {
		t.Fatalf("синтетический элемент: Values len = %d, want 2: %+v", len(item.Values), item)
	}
	if item.Values[0].Value != "prod" || item.Values[0].Count != 7 {
		t.Errorf("Values[0] = %+v, want {prod 7 ...}", item.Values[0])
	}
	// Count синтетического элемента — сумма values (10), а не 0 и не
	// придуманное число: точного счётчика AttrKeys для ключа вне выборки нет.
	if item.Count != 10 {
		t.Errorf("синтетический элемент Count = %d, want 10 (сумма values)", item.Count)
	}

	// Обычные ключи из топа остаются на месте, без values (не раскрыты).
	if got.Keys[1].Key != "http.method" || got.Keys[1].Expanded {
		t.Errorf("Keys[1] = %+v, want нераскрытый http.method", got.Keys[1])
	}
	if got.Keys[2].Key != "http.status_code" || got.Keys[2].Expanded {
		t.Errorf("Keys[2] = %+v, want нераскрытый http.status_code", got.Keys[2])
	}
}

// TestNewAttrFacetsExpandedKeyInsideTop — обычный случай (не carry-fix):
// раскрытый ключ найден среди keys — синтетический элемент не добавляется,
// длина списка не меняется.
func TestNewAttrFacetsExpandedKeyInsideTop(t *testing.T) {
	keys := []log.FacetValue{
		{Value: "http.method", Count: 100},
		{Value: "http.status_code", Count: 80},
	}
	values := []log.FacetValue{{Value: "GET", Count: 5}}

	got := NewAttrFacets(1, LogsFilter{}, keys, "http.method", values)

	if len(got.Keys) != 2 {
		t.Fatalf("Keys len = %d, want 2 (без синтетического элемента): %+v", len(got.Keys), got.Keys)
	}
	if !got.Keys[0].Expanded || got.Keys[0].Key != "http.method" {
		t.Fatalf("Keys[0] должен быть раскрытым http.method: %+v", got.Keys[0])
	}
	if got.Keys[0].Count != 100 {
		t.Errorf("Count раскрытого ключа из топа не должен подменяться суммой values: got %d, want 100", got.Keys[0].Count)
	}
	if len(got.Keys[0].Values) != 1 || got.Keys[0].Values[0].Value != "GET" {
		t.Errorf("Keys[0].Values = %+v, want [{GET 5 ...}]", got.Keys[0].Values)
	}
	if got.Keys[1].Expanded {
		t.Errorf("Keys[1] не должен быть раскрыт: %+v", got.Keys[1])
	}
}

// TestNewAttrFacetsNoExpandedKey — ?facet= отсутствует: ни один элемент не
// раскрыт, синтетический элемент не добавляется.
func TestNewAttrFacetsNoExpandedKey(t *testing.T) {
	keys := []log.FacetValue{{Value: "http.method", Count: 100}}

	got := NewAttrFacets(1, LogsFilter{}, keys, "", nil)

	if len(got.Keys) != 1 {
		t.Fatalf("Keys len = %d, want 1", len(got.Keys))
	}
	if got.Keys[0].Expanded {
		t.Errorf("без ?facet= ни один элемент не должен быть раскрыт: %+v", got.Keys[0])
	}
}

// TestLogsPageURLPreservesFacet — правка ревью UX Minor #1: раскрытый
// атрибут-фасет (?facet=<key>) должен сохраняться в ссылке «показать
// старее» — иначе раскрытая секция сайдбара схлопывалась бы при переходе на
// следующую страницу списка.
func TestLogsPageURLPreservesFacet(t *testing.T) {
	got := LogsPageURL(1, LogsFilter{Facet: "http.method"}, time.UnixMilli(1000), 2)
	if !strings.Contains(got, "facet=http.method") {
		t.Fatalf("LogsPageURL(...) = %q, want facet=http.method сохранённым", got)
	}
}

// TestLogsPageURLNoFacetWhenEmpty — пустой Filter.Facet не добавляет
// параметр в URL (обычная ссылка без раскрытого атрибут-фасета).
func TestLogsPageURLNoFacetWhenEmpty(t *testing.T) {
	got := LogsPageURL(1, LogsFilter{}, time.Time{}, 0)
	if strings.Contains(got, "facet=") {
		t.Fatalf("LogsPageURL(...) = %q, facet не должен появляться без Filter.Facet", got)
	}
}

// TestLogTracePath — правка ревью UX Important #4: trace_id лога должен
// вести на реальную страницу трейса (/traces/{trace_id}), не на общий
// раздел «Производительность».
func TestLogTracePath(t *testing.T) {
	got := logTracePath("abc123")
	want := "/traces/abc123"
	if got != want {
		t.Fatalf("logTracePath(%q) = %q, want %q", "abc123", got, want)
	}
}
