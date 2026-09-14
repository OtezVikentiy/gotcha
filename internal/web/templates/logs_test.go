package templates

import (
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/log"
)

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

func TestLogsAroundEventWindowAlways(t *testing.T) {
	ts := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)

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

	q2 := parseLogsLink(t, logsAroundEventPath(1, "", ts, "prod"), 1)
	unixWindow(t, q2)
	if q2.Get("trace_id") != "" {
		t.Fatalf("без-trace-ветка не должна ставить trace_id, получено %q", q2.Get("trace_id"))
	}
	if q2.Get("environment") != "prod" {
		t.Fatalf("без-trace-ветка: environment=%q, ожидался prod", q2.Get("environment"))
	}

	q3 := parseLogsLink(t, logsAroundEventPath(1, "", ts, ""), 1)
	unixWindow(t, q3)
	if q3.Get("trace_id") != "" || q3.Get("environment") != "" {
		t.Fatalf("пустая ветка должна нести только окно, получено trace_id=%q env=%q", q3.Get("trace_id"), q3.Get("environment"))
	}
}

func TestLogsForTraceSaturatedWindow(t *testing.T) {
	from := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)

	qn := parseLogsLink(t, logsForTracePath(1, "tr", from, 5_000_000 /* 5s */), 1)
	_, tn := unixWindow(t, qn)
	if span := tn.Sub(from); span > time.Minute {
		t.Fatalf("нормальный трейс: окно %v неожиданно широкое", span)
	}

	qs := parseLogsLink(t, logsForTracePath(1, "tr", from, ^uint32(0)), 1)
	_, ts := unixWindow(t, qs)
	if span := ts.Sub(from); span < 24*time.Hour {
		t.Fatalf("насыщенный TotalUS: окно %v < 24ч — хвост логов длинного трейса был бы отсечён", span)
	}
	if qs.Get("trace_id") != "tr" {
		t.Fatalf("trace_id должен присутствовать при любом окне")
	}
}

func TestLogsForHostPathEncoding(t *testing.T) {
	q := parseLogsLink(t, logsForHostPath(1, "web-01.dc:eu"), 1)
	attr := q.Get("attr")
	want := "res:host.name:web-01.dc:eu"
	if attr != want {
		t.Fatalf("attr=%q, ожидался %q (первое ':' делит префикс/ключ/значение, двоеточия значения сохраняются)", attr, want)
	}
}

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

func TestNewAttrFacetsExpandedKeyOutsideTop(t *testing.T) {
	keys := []log.FacetValue{
		{Value: "http.method", Count: 100},
		{Value: "http.status_code", Count: 80},
	}
	values := []log.FacetValue{
		{Value: "prod", Count: 7},
		{Value: "staging", Count: 3},
	}

	got := NewAttrFacets(ruCtx(), 1, LogsFilter{}, keys, "environment.tier", values)

	if len(got.Keys) != 3 {
		t.Fatalf("Keys len = %d, want 3 (2 из топа + 1 синтетический): %+v", len(got.Keys), got.Keys)
	}

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
	if item.Count != 10 {
		t.Errorf("синтетический элемент Count = %d, want 10 (сумма values)", item.Count)
	}

	if got.Keys[1].Key != "http.method" || got.Keys[1].Expanded {
		t.Errorf("Keys[1] = %+v, want нераскрытый http.method", got.Keys[1])
	}
	if got.Keys[2].Key != "http.status_code" || got.Keys[2].Expanded {
		t.Errorf("Keys[2] = %+v, want нераскрытый http.status_code", got.Keys[2])
	}
}

func TestNewAttrFacetsExpandedKeyInsideTop(t *testing.T) {
	keys := []log.FacetValue{
		{Value: "http.method", Count: 100},
		{Value: "http.status_code", Count: 80},
	}
	values := []log.FacetValue{{Value: "GET", Count: 5}}

	got := NewAttrFacets(ruCtx(), 1, LogsFilter{}, keys, "http.method", values)

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

func TestNewAttrFacetsNoExpandedKey(t *testing.T) {
	keys := []log.FacetValue{{Value: "http.method", Count: 100}}

	got := NewAttrFacets(ruCtx(), 1, LogsFilter{}, keys, "", nil)

	if len(got.Keys) != 1 {
		t.Fatalf("Keys len = %d, want 1", len(got.Keys))
	}
	if got.Keys[0].Expanded {
		t.Errorf("без ?facet= ни один элемент не должен быть раскрыт: %+v", got.Keys[0])
	}
}

func TestLogsPageURLPreservesFacet(t *testing.T) {
	got := LogsPageURL(1, LogsFilter{Facet: "http.method"}, time.UnixMilli(1000), 2)
	if !strings.Contains(got, "facet=http.method") {
		t.Fatalf("LogsPageURL(...) = %q, want facet=http.method сохранённым", got)
	}
}

func TestLogsPageURLNoFacetWhenEmpty(t *testing.T) {
	got := LogsPageURL(1, LogsFilter{}, time.Time{}, 0)
	if strings.Contains(got, "facet=") {
		t.Fatalf("LogsPageURL(...) = %q, facet не должен появляться без Filter.Facet", got)
	}
}

// GET-форма фильтра обязана переносить facet — иначе «Применить» при раскрытом
// ключе атрибута схлопывает сайдбар, хотя все прочие скрытые поля пережили сабмит.
func TestLogsScreenFormPreservesFacet(t *testing.T) {
	out := renderTo(t, LogsScreen(7, nil, LogsFilter{Range: TimeRangeVM{Key: "24h"}, Facet: "http.method"}, false, "", LogsHistogram{Empty: true}, LogFacets{}, "u@e.com", LogSavedFiltersPanel{}, ""))
	if !strings.Contains(out, `<input type="hidden" name="facet" value="http.method">`) {
		t.Fatalf("форма фильтра логов не переносит facet скрытым полем: %s", out)
	}
}

func TestLogsScreenFormOmitsFacetWhenEmpty(t *testing.T) {
	out := renderTo(t, LogsScreen(7, nil, LogsFilter{Range: TimeRangeVM{Key: "24h"}}, false, "", LogsHistogram{Empty: true}, LogFacets{}, "u@e.com", LogSavedFiltersPanel{}, ""))
	if strings.Contains(out, `name="facet"`) {
		t.Fatalf("форма фильтра логов не должна печатать facet без Filter.Facet: %s", out)
	}
}

func TestLogsPageURLCarriesDefaultSuppression(t *testing.T) {
	f := LogsFilter{DefaultSuppressed: true}
	got := LogsPageURL(1, f, time.UnixMilli(1000), 0)
	q := parseLogsLink(t, got, 1)
	if q.Get("nodefault") != "1" {
		t.Fatalf("LogsPageURL(...) = %q, want nodefault=1 сохранённым при переходе на следующую страницу", got)
	}
}

func TestLogsPageURLOmitsDefaultSuppressionWhenNotSuppressed(t *testing.T) {
	got := LogsPageURL(1, LogsFilter{}, time.UnixMilli(1000), 0)
	if strings.Contains(got, "nodefault") {
		t.Fatalf("LogsPageURL(...) = %q, nodefault не должен появляться без DefaultSuppressed", got)
	}
}

func TestLogNotChipRemoveURLCarriesDefaultSuppression(t *testing.T) {
	p := log.Predicate{Field: log.FieldService, Op: log.OpNeq, Value: "worker"}
	f := LogsFilter{DefaultSuppressed: true, Not: []log.Predicate{p}}
	got := logNotChipRemoveURL(1, f, p)
	q := parseLogsLink(t, got, 1)
	if len(q["service_not"]) != 0 {
		t.Fatalf("logNotChipRemoveURL(...) = %q, чип должен быть снят", got)
	}
	if q.Get("nodefault") != "1" {
		t.Fatalf("logNotChipRemoveURL(...) = %q, want nodefault=1 сохранённым после снятия последнего чипа", got)
	}
}

func TestLogTracePath(t *testing.T) {
	got := logTracePath("abc123")
	want := "/traces/abc123"
	if got != want {
		t.Fatalf("logTracePath(%q) = %q, want %q", "abc123", got, want)
	}
}

func TestIncludeURLReplacesSingleValueField(t *testing.T) {
	f := LogsFilter{Service: "api"}
	got := logIncludeURL(7, f, log.Predicate{Field: log.FieldService, Op: log.OpEq, Value: "worker"})

	if strings.Count(got, "service=") != 1 {
		t.Fatalf("сервис должен замещаться, а не накапливаться: %s", got)
	}
	if !strings.Contains(got, "service=worker") {
		t.Fatalf("новое значение не подставлено: %s", got)
	}
}

func TestIncludeURLAccumulatesSeverity(t *testing.T) {
	f := LogsFilter{Severity: []string{"info"}}
	got := logIncludeURL(7, f, log.Predicate{Field: log.FieldSeverity, Value: "error"})

	q := parseLogsLink(t, got, 7)
	if sev := q["severity"]; len(sev) != 2 || sev[0] != "info" || sev[1] != "error" {
		t.Fatalf("severity должен накапливаться (info+error), получили %v: %s", sev, got)
	}
}

func TestIncludeURLAccumulatesAttr(t *testing.T) {
	f := LogsFilter{Attrs: []log.AttrFilter{{Key: "host", Value: "a1"}}}
	got := logIncludeURL(7, f, log.Predicate{Field: log.FieldAttr, Key: "source", Value: "nginx"})

	for _, want := range []string{"attr=host%3Aa1", "attr=source%3Anginx"} {
		if !strings.Contains(got, want) {
			t.Fatalf("attr должен накапливаться, не хватает %s: %s", want, got)
		}
	}
}

func TestLogRowAttrPredicate(t *testing.T) {
	got := logRowAttrPredicate(logAttrRow{Key: "resource.host.name", RawKey: "host.name", Val: "web-1", Resource: true}, log.OpNeq)
	want := log.Predicate{Field: log.FieldResourceAttr, Key: "host.name", Op: log.OpNeq, Value: "web-1"}
	if got != want {
		t.Fatalf("logRowAttrPredicate(resource) = %+v, want %+v", got, want)
	}

	got = logRowAttrPredicate(logAttrRow{Key: "source", RawKey: "source", Val: "nginx"}, log.OpEq)
	want = log.Predicate{Field: log.FieldAttr, Key: "source", Op: log.OpEq, Value: "nginx"}
	if got != want {
		t.Fatalf("logRowAttrPredicate(log attr) = %+v, want %+v", got, want)
	}

	got = logRowAttrPredicate(logAttrRow{Key: "resource.pool", RawKey: "resource.pool", Val: "db-1"}, log.OpNeq)
	want = log.Predicate{Field: log.FieldAttr, Key: "resource.pool", Op: log.OpNeq, Value: "db-1"}
	if got != want {
		t.Fatalf("logRowAttrPredicate(log attr named resource.pool) = %+v, want %+v", got, want)
	}
}
