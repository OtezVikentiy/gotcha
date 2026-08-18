package log_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/log"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// TestQueryList поднимает один CH-контейнер, наполняет его через log.Writer и
// прогоняет фильтры log.Query.List подтестами (как trace/query_test).
func TestQueryList(t *testing.T) {
	conn := testenv.MigratedCH(t)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()

	const projectID = int64(70)
	const projectID2 = int64(71) // чужой проект — не должен утекать в List(projectID, ...)
	const projectID3 = int64(72) // отдельный проект под курсорный тест (границы по одинаковому timestamp)
	const projectID4 = int64(73) // курсорный тест: строки-«близнецы» с разными атрибутами в одну мс

	w := log.NewWriter(conn)
	go w.Run()

	base := time.Now().UTC().Truncate(time.Hour).Add(-2 * time.Hour)
	from := base
	to := base.Add(time.Hour)

	// 40 info-логов сервиса "api"/production — с атрибутами лога и ресурса.
	for i := 0; i < 40; i++ {
		w.Add(projectID, log.LogRecord{
			Timestamp:      base.Add(time.Duration(i) * time.Minute),
			ObservedTS:     base.Add(time.Duration(i) * time.Minute),
			Severity:       log.SevInfo,
			SeverityNumber: 9,
			SeverityText:   "INFO",
			Body:           fmt.Sprintf("request handled #%d", i),
			Service:        "api",
			Environment:    "production",
			LogAttributes:  map[string]string{"http.method": "GET"},
			ResourceAttrs:  map[string]string{"host.name": "web-01"},
		})
	}

	// 5 error-логов сервиса "worker"/staging, тело содержит "boom" — для
	// фильтров Severity/Service/Environment/Query и атрибута log_attributes.
	for i := 0; i < 5; i++ {
		w.Add(projectID, log.LogRecord{
			Timestamp:      base.Add(time.Duration(i) * time.Minute),
			ObservedTS:     base.Add(time.Duration(i) * time.Minute),
			Severity:       log.SevError,
			SeverityNumber: 17,
			SeverityText:   "ERROR",
			Body:           fmt.Sprintf("boom happened #%d", i),
			Service:        "worker",
			Environment:    "staging",
			LogAttributes:  map[string]string{"http.method": "POST"},
			ResourceAttrs:  map[string]string{"host.name": "worker-01"},
		})
	}

	// Другой проект — должен полностью отсутствовать в результатах List(projectID, ...).
	w.Add(projectID2, log.LogRecord{
		Timestamp:   base.Add(10 * time.Minute),
		ObservedTS:  base.Add(10 * time.Minute),
		Severity:    log.SevInfo,
		Body:        "other project log",
		Service:     "api",
		Environment: "production",
	})

	// Курсорный набор: 5 строк — самая старая, три с ОДИНАКОВЫМ timestamp
	// (одна и та же мс) и самая новая. При Limit=2 граница страницы неизбежно
	// падает ВНУТРИ тройки — ровно тот сценарий, который проверяет TieSkip.
	cursorFrom := base.Add(30 * time.Minute)
	cursorOld := cursorFrom.Add(1 * time.Second)
	cursorTie := cursorFrom.Add(2 * time.Second).Truncate(time.Millisecond)
	cursorNew := cursorFrom.Add(3 * time.Second)
	cursorTo := cursorFrom.Add(time.Minute)

	w.Add(projectID3, log.LogRecord{Timestamp: cursorOld, ObservedTS: cursorOld, Severity: log.SevInfo, Body: "cursor old", TraceID: "cursor-old"})
	for i := 0; i < 3; i++ {
		w.Add(projectID3, log.LogRecord{
			Timestamp: cursorTie, ObservedTS: cursorTie, Severity: log.SevInfo,
			Body: fmt.Sprintf("cursor tie #%d", i), TraceID: fmt.Sprintf("cursor-tie-%d", i),
		})
	}
	w.Add(projectID3, log.LogRecord{Timestamp: cursorNew, ObservedTS: cursorNew, Severity: log.SevInfo, Body: "cursor new", TraceID: "cursor-new"})

	// Пара строк-«близнецов»: совпадают по всем скалярным полям (timestamp,
	// observed_ts, severity, body, trace_id, span_id, service, environment —
	// у обеих нулевые/одинаковые значения), различаются ТОЛЬКО атрибутом
	// log_attributes. Проверяет, что хэш второго ключа сортировки учитывает
	// атрибуты и не схлопывает такие строки в один тай-порядок.
	twinTS := cursorFrom.Add(10 * time.Second).Truncate(time.Millisecond)
	for i := 0; i < 2; i++ {
		w.Add(projectID4, log.LogRecord{
			Timestamp: twinTS, ObservedTS: twinTS, Severity: log.SevInfo, SeverityNumber: 9, SeverityText: "INFO",
			Body:          "twin body",
			LogAttributes: map[string]string{"idx": fmt.Sprintf("%d", i)},
		})
	}

	if err := w.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := w.Dropped(); got != 0 {
		t.Fatalf("Dropped() = %d, want 0", got)
	}

	q := log.NewQuery(conn)

	t.Run("window and newest-first order", func(t *testing.T) {
		got, err := q.List(ctx, projectID, log.ListFilter{From: from, To: to, Limit: 500})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		want := 40 + 5
		if len(got) != want {
			t.Fatalf("len(got) = %d, want %d", len(got), want)
		}
		for i := 1; i < len(got); i++ {
			if got[i-1].Timestamp.Before(got[i].Timestamp) {
				t.Fatalf("not newest-first at %d: %v before %v", i, got[i-1].Timestamp, got[i].Timestamp)
			}
		}
	})

	t.Run("severity filter", func(t *testing.T) {
		got, err := q.List(ctx, projectID, log.ListFilter{From: from, To: to, Severity: []string{log.SevError}, Limit: 500})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(got) != 5 {
			t.Fatalf("len(got) = %d, want 5", len(got))
		}
		for _, r := range got {
			if r.Severity != log.SevError {
				t.Fatalf("severity = %q, want %q", r.Severity, log.SevError)
			}
		}
	})

	t.Run("service filter", func(t *testing.T) {
		got, err := q.List(ctx, projectID, log.ListFilter{From: from, To: to, Service: "worker", Limit: 500})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(got) != 5 {
			t.Fatalf("len(got) = %d, want 5", len(got))
		}
	})

	t.Run("environment filter", func(t *testing.T) {
		got, err := q.List(ctx, projectID, log.ListFilter{From: from, To: to, Environment: "staging", Limit: 500})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(got) != 5 {
			t.Fatalf("len(got) = %d, want 5", len(got))
		}
	})

	t.Run("body substring case-insensitive", func(t *testing.T) {
		got, err := q.List(ctx, projectID, log.ListFilter{From: from, To: to, Query: "BOOM", Limit: 500})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(got) != 5 {
			t.Fatalf("len(got) = %d, want 5", len(got))
		}
	})

	t.Run("log attribute filter", func(t *testing.T) {
		got, err := q.List(ctx, projectID, log.ListFilter{
			From: from, To: to,
			Attrs: []log.AttrFilter{{Key: "http.method", Value: "POST"}},
			Limit: 500,
		})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(got) != 5 {
			t.Fatalf("len(got) = %d, want 5", len(got))
		}
	})

	t.Run("resource attribute filter", func(t *testing.T) {
		got, err := q.List(ctx, projectID, log.ListFilter{
			From: from, To: to,
			Attrs: []log.AttrFilter{{Resource: true, Key: "host.name", Value: "web-01"}},
			Limit: 500,
		})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(got) != 40 {
			t.Fatalf("len(got) = %d, want 40", len(got))
		}
	})

	t.Run("limit truncates result", func(t *testing.T) {
		got, err := q.List(ctx, projectID, log.ListFilter{From: from, To: to, Limit: 3})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(got) != 3 {
			t.Fatalf("len(got) = %d, want 3", len(got))
		}
	})

	t.Run("zero limit defaults, all rows fit under default", func(t *testing.T) {
		got, err := q.List(ctx, projectID, log.ListFilter{From: from, To: to})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(got) != 45 { // дефолт 100 > 45 строк проекта — весь набор должен вернуться
			t.Fatalf("len(got) = %d, want 45", len(got))
		}
	})

	t.Run("injection safe: quote in service filters out cleanly", func(t *testing.T) {
		got, err := q.List(ctx, projectID, log.ListFilter{From: from, To: to, Service: "x'; DROP TABLE logs; --", Limit: 500})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("len(got) = %d, want 0", len(got))
		}
	})

	// Курсорная пагинация: собираем весь набор projectID3 (5 строк) постранично
	// с Limit=2, скармливая Before/TieSkip от предыдущей страницы. Проверяем,
	// что тройка с одинаковым timestamp не теряет и не дублирует строки на
	// границе страницы.
	t.Run("cursor pagination across timestamp tie", func(t *testing.T) {
		const limit = 2
		var before time.Time
		var tieSkip int
		seen := make(map[string]bool)
		var all []log.LogRow

		for page := 0; page < 10; page++ { // предохранитель от зацикливания
			got, err := q.List(ctx, projectID3, log.ListFilter{
				From: cursorFrom, To: cursorTo, Limit: limit, Before: before, TieSkip: tieSkip,
			})
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			if len(got) == 0 {
				break
			}
			for _, r := range got {
				if seen[r.TraceID] {
					t.Fatalf("duplicate row across pages: trace_id=%s", r.TraceID)
				}
				seen[r.TraceID] = true
				all = append(all, r)
			}
			last := got[len(got)-1]
			// TieSkip — СКОЛЬКО строк с timestamp==Before уже отдано вызывающему
			// ВСЕГО (не только этой страницей): если тай растягивается больше чем
			// на одну страницу (граница снова падает на тот же Before), счётчик
			// накапливается, а не пересчитывается заново — иначе следующая
			// страница переспросит уже показанные строки этой тай-группы.
			matches := 0
			for _, r := range got {
				if r.Timestamp.Equal(last.Timestamp) {
					matches++
				}
			}
			if last.Timestamp.Equal(before) {
				tieSkip += matches
			} else {
				tieSkip = matches
			}
			before = last.Timestamp
			if len(got) < limit {
				break
			}
		}

		if len(all) != 5 {
			t.Fatalf("total rows across pages = %d, want 5 (%+v)", len(all), all)
		}
		wantIDs := []string{"cursor-old", "cursor-tie-0", "cursor-tie-1", "cursor-tie-2", "cursor-new"}
		for _, id := range wantIDs {
			if !seen[id] {
				t.Fatalf("missing row across pages: trace_id=%s", id)
			}
		}
	})

	// TieSkip из URL заклампен в самой List: гигантский tskip не превращается в
	// LIMIT = limit + tskip (иначе одиночный GET материализует всё окно → OOM
	// мультитенантного процесса, находка финального ревью C2). Отрицательный
	// tskip тоже обнуляется (иначе отрицательный queryLimit роняет CH).
	t.Run("TieSkip из URL заклампен — гигантский/отрицательный не амплифицирует LIMIT", func(t *testing.T) {
		for _, tskip := range []int{2_000_000_000, -999_999} {
			got, err := q.List(ctx, projectID3, log.ListFilter{
				From: cursorFrom, To: cursorTo, Limit: 2, Before: cursorNew, TieSkip: tskip,
			})
			if err != nil {
				t.Fatalf("List(tskip=%d): %v (кламп не сработал — отрицательный queryLimit?)", tskip, err)
			}
			if len(got) > 2 {
				t.Fatalf("List(tskip=%d) вернул %d строк, ждём ≤ Limit(2) — LIMIT амплифицирован tskip", tskip, len(got))
			}
		}
	})

	// Проверяет фикс на «близнецах»: две строки с одинаковым timestamp и
	// одинаковыми всеми скалярными полями (различаются только log_attributes)
	// разведены по разным страницам (Limit=1) без дубля и без потери — второй
	// ключ сортировки должен учитывать атрибуты, а не только скаляры.
	t.Run("cursor pagination: twin rows differing only by attributes don't collide", func(t *testing.T) {
		page1, err := q.List(ctx, projectID4, log.ListFilter{From: cursorFrom, To: cursorTo, Limit: 1})
		if err != nil {
			t.Fatalf("List page1: %v", err)
		}
		if len(page1) != 1 {
			t.Fatalf("len(page1) = %d, want 1", len(page1))
		}
		page2, err := q.List(ctx, projectID4, log.ListFilter{
			From: cursorFrom, To: cursorTo, Limit: 1, Before: page1[0].Timestamp, TieSkip: 1,
		})
		if err != nil {
			t.Fatalf("List page2: %v", err)
		}
		if len(page2) != 1 {
			t.Fatalf("len(page2) = %d, want 1", len(page2))
		}
		idx1, idx2 := page1[0].LogAttributes["idx"], page2[0].LogAttributes["idx"]
		if idx1 == idx2 {
			t.Fatalf("duplicate twin row across pages: idx=%s returned on both pages", idx1)
		}
		if (idx1 != "0" && idx1 != "1") || (idx2 != "0" && idx2 != "1") {
			t.Fatalf("unexpected idx values: page1=%q page2=%q", idx1, idx2)
		}
	})
}

// TestQueryListBeforeCursorSubMillisecondPrecision — регрессия: Before с
// НЕнулевыми миллисекундами (реальные timestamp почти никогда не выровнены
// на секунду) не должен терять граничную строку. До фикса (toDateTime64(?, 3)
// + строковый аргумент вместо голого time.Time в "?") позиционный биндинг
// clickhouse-go форматировал ЛЮБОЙ time.Time-аргумент с TimeUnit=Seconds
// (bindPositional в драйвере жёстко использует эту шкалу), то есть Before
// обрезался до целой секунды на пути в SQL — "timestamp <= Before" ложно
// сравнивалось для самой граничной строки, и курсор «показать старее» терял
// её молча. Тест выше (TestQueryList/cursor pagination…) этот баг не ловил:
// его фикстуры построены на Truncate(time.Hour)+целые секунды, то есть
// специально без миллисекунд — там обрезка до секунды ничего не меняла.
func TestQueryListBeforeCursorSubMillisecondPrecision(t *testing.T) {
	conn := testenv.MigratedCH(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	const projectID = int64(74)

	w := log.NewWriter(conn)
	go w.Run()

	// .280мс — намеренно НЕ выровнено на секунду/на ноль.
	base := time.Now().UTC().Truncate(time.Millisecond)
	t1 := base.Add(-2 * time.Minute)
	t2 := base.Add(-time.Minute)
	w.Add(projectID, log.LogRecord{Timestamp: t1, ObservedTS: t1, Severity: log.SevInfo, Body: "older"})
	w.Add(projectID, log.LogRecord{Timestamp: t2, ObservedTS: t2, Severity: log.SevInfo, Body: "boundary"})
	if err := w.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	q := log.NewQuery(conn)
	got, err := q.List(ctx, projectID, log.ListFilter{
		From: base.Add(-time.Hour), To: base.Add(time.Hour), Limit: 100, Before: t2,
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2 (Before must include the boundary row itself): %+v", len(got), got)
	}
	if got[0].Body != "boundary" || got[1].Body != "older" {
		t.Fatalf("unexpected rows: %+v", got)
	}
}

// TestQueryHistogram — задача 3 плана C2: гистограмма объёма логов по времени
// и severity. Окно 6 часов, buckets=6 (шаг 1ч): корзина 0 — 3 info + 2 error,
// корзина 2 — 4 warn, корзина 5 — 1 fatal, корзины 1/3/4 — пусты (проверка
// добивки нулями по ВСЕМ 6 severity, не только по тем, что встретились в
// окне).
func TestQueryHistogram(t *testing.T) {
	conn := testenv.MigratedCH(t)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()

	const projectID = int64(75)

	w := log.NewWriter(conn)
	go w.Run()

	base := time.Now().UTC().Truncate(time.Hour).Add(-8 * time.Hour)
	from := base
	to := base.Add(6 * time.Hour)
	const buckets = 6 // шаг = (to-from)/buckets = 1ч

	for i := 0; i < 3; i++ {
		ts := base.Add(time.Duration(i) * time.Minute)
		w.Add(projectID, log.LogRecord{Timestamp: ts, ObservedTS: ts, Severity: log.SevInfo, Body: "b0-info"})
	}
	for i := 0; i < 2; i++ {
		ts := base.Add(time.Duration(i) * time.Minute)
		w.Add(projectID, log.LogRecord{Timestamp: ts, ObservedTS: ts, Severity: log.SevError, Body: "b0-error"})
	}
	b2 := base.Add(2 * time.Hour)
	for i := 0; i < 4; i++ {
		ts := b2.Add(time.Duration(i) * time.Minute)
		w.Add(projectID, log.LogRecord{Timestamp: ts, ObservedTS: ts, Severity: log.SevWarn, Body: "b2-warn"})
	}
	b5 := base.Add(5 * time.Hour)
	w.Add(projectID, log.LogRecord{Timestamp: b5, ObservedTS: b5, Severity: log.SevFatal, Body: "b5-fatal"})

	if err := w.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := w.Dropped(); got != 0 {
		t.Fatalf("Dropped() = %d, want 0", got)
	}

	q := log.NewQuery(conn)
	times, series, err := q.Histogram(ctx, projectID, log.ListFilter{From: from, To: to}, buckets)
	if err != nil {
		t.Fatalf("Histogram: %v", err)
	}

	if len(times) != buckets {
		t.Fatalf("len(times) = %d, want %d", len(times), buckets)
	}
	for _, sev := range log.Severities {
		if len(series[sev]) != buckets {
			t.Fatalf("len(series[%s]) = %d, want %d", sev, len(series[sev]), buckets)
		}
	}

	// Пустые корзины (1, 3, 4) — нули по ВСЕМ severity, не только по тем, что
	// встретились где-то ещё в окне.
	for _, idx := range []int{1, 3, 4} {
		for _, sev := range log.Severities {
			if series[sev][idx] != 0 {
				t.Fatalf("series[%s][%d] = %d, want 0 (empty bucket)", sev, idx, series[sev][idx])
			}
		}
	}

	if series[log.SevInfo][0] != 3 {
		t.Fatalf("series[info][0] = %d, want 3", series[log.SevInfo][0])
	}
	if series[log.SevError][0] != 2 {
		t.Fatalf("series[error][0] = %d, want 2", series[log.SevError][0])
	}
	if series[log.SevWarn][2] != 4 {
		t.Fatalf("series[warn][2] = %d, want 4", series[log.SevWarn][2])
	}
	if series[log.SevFatal][5] != 1 {
		t.Fatalf("series[fatal][5] = %d, want 1", series[log.SevFatal][5])
	}
	if series[log.SevTrace][0] != 0 || series[log.SevDebug][0] != 0 {
		t.Fatalf("series[trace/debug][0] should be 0, got trace=%d debug=%d", series[log.SevTrace][0], series[log.SevDebug][0])
	}

	// Сумма по всем корзинам и severity = число написанных строк.
	var total int64
	for _, sev := range log.Severities {
		for _, v := range series[sev] {
			total += v
		}
	}
	if total != 10 {
		t.Fatalf("total = %d, want 10", total)
	}
}

// facetCount — count() значения value в результате Facet, 0 если значения
// вообще нет в срезе (facet ORDER BY count() DESC GROUP BY — отсутствующее
// значение просто не встретилось в окне+фильтрах, это не ошибка теста).
func facetCount(values []log.FacetValue, value string) int64 {
	for _, v := range values {
		if v.Value == value {
			return v.Count
		}
	}
	return 0
}

// TestQueryFacet — задача 4 плана C2: встроенные фасеты severity/service/
// environment. Ключевая проверка — exclude-self: фасет severity игнорирует
// СВОЙ фильтр (f.Severity), но применяет остальные (Service/Environment/...),
// тогда как фасеты service/environment применяют ВСЕ фильтры без исключений
// (включая severity) — иначе клик по невыбранному значению фасета не мог бы
// расширить выборку обратно (счётчик всегда 0, раз строки уже отфильтрованы).
func TestQueryFacet(t *testing.T) {
	conn := testenv.MigratedCH(t)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()

	const projectID = int64(76)

	w := log.NewWriter(conn)
	go w.Run()

	base := time.Now().UTC().Truncate(time.Hour).Add(-3 * time.Hour)
	from := base
	to := base.Add(2 * time.Hour)

	add := func(n int, sev, service, env string) {
		for i := 0; i < n; i++ {
			ts := base.Add(time.Duration(i) * time.Second)
			w.Add(projectID, log.LogRecord{
				Timestamp: ts, ObservedTS: ts, Severity: sev, Body: "x",
				Service: service, Environment: env,
			})
		}
	}
	add(10, log.SevInfo, "web", "production")
	add(3, log.SevError, "web", "production")
	add(5, log.SevWarn, "api", "staging")
	add(1, log.SevDebug, "worker", "production")

	if err := w.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := w.Dropped(); got != 0 {
		t.Fatalf("Dropped() = %d, want 0", got)
	}

	q := log.NewQuery(conn)

	t.Run("severity facet excludes its own filter", func(t *testing.T) {
		got, err := q.Facet(ctx, projectID, log.ListFilter{From: from, To: to, Severity: []string{log.SevError}}, "severity")
		if err != nil {
			t.Fatalf("Facet: %v", err)
		}
		// exclude-self: собственный фильтр Severity=["error"] игнорируется —
		// видны ВСЕ уровни, встретившиеся в окне, не только error.
		if c := facetCount(got, log.SevInfo); c != 10 {
			t.Fatalf("info count = %d, want 10 (%+v)", c, got)
		}
		if c := facetCount(got, log.SevWarn); c != 5 {
			t.Fatalf("warn count = %d, want 5 (%+v)", c, got)
		}
		if c := facetCount(got, log.SevError); c != 3 {
			t.Fatalf("error count = %d, want 3 (%+v)", c, got)
		}
		if c := facetCount(got, log.SevDebug); c != 1 {
			t.Fatalf("debug count = %d, want 1 (%+v)", c, got)
		}
		for i := 1; i < len(got); i++ {
			if got[i-1].Count < got[i].Count {
				t.Fatalf("not sorted count() DESC at %d: %+v", i, got)
			}
		}
	})

	t.Run("severity facet still applies other filters", func(t *testing.T) {
		got, err := q.Facet(ctx, projectID, log.ListFilter{From: from, To: to, Service: "web"}, "severity")
		if err != nil {
			t.Fatalf("Facet: %v", err)
		}
		if c := facetCount(got, log.SevInfo); c != 10 {
			t.Fatalf("info count = %d, want 10 (%+v)", c, got)
		}
		if c := facetCount(got, log.SevError); c != 3 {
			t.Fatalf("error count = %d, want 3 (%+v)", c, got)
		}
		if c := facetCount(got, log.SevWarn); c != 0 {
			t.Fatalf("warn count = %d, want 0 — service=web не должен пропускать warn (сервис api) (%+v)", c, got)
		}
	})

	t.Run("service facet applies severity filter (no exclude-self outside own column)", func(t *testing.T) {
		got, err := q.Facet(ctx, projectID, log.ListFilter{From: from, To: to, Severity: []string{log.SevError}}, "service")
		if err != nil {
			t.Fatalf("Facet: %v", err)
		}
		if len(got) != 1 || got[0].Value != "web" || got[0].Count != 3 {
			t.Fatalf("service facet = %+v, want ровно [{web 3}]", got)
		}
	})

	t.Run("environment facet", func(t *testing.T) {
		got, err := q.Facet(ctx, projectID, log.ListFilter{From: from, To: to}, "environment")
		if err != nil {
			t.Fatalf("Facet: %v", err)
		}
		if c := facetCount(got, "production"); c != 14 {
			t.Fatalf("production count = %d, want 14 (10 info + 3 error + 1 debug) (%+v)", c, got)
		}
		if c := facetCount(got, "staging"); c != 5 {
			t.Fatalf("staging count = %d, want 5 (%+v)", c, got)
		}
	})

	t.Run("invalid column rejected by whitelist", func(t *testing.T) {
		_, err := q.Facet(ctx, projectID, log.ListFilter{From: from, To: to}, "body")
		if err == nil {
			t.Fatal("Facet(col=\"body\"): want error (не в whitelist), got nil")
		}
	})
}

// TestQueryFacetLimit — Facet отдаёт top-N по count() DESC (facetLimit=10),
// а не все различающиеся значения колонки: 15 сервисов с УБЫВАЮЩИМ числом
// строк (svc00 больше всех, svc14 меньше всех) — должны остаться ровно
// svc00..svc09 в порядке убывания.
func TestQueryFacetLimit(t *testing.T) {
	conn := testenv.MigratedCH(t)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()

	const projectID = int64(77)

	w := log.NewWriter(conn)
	go w.Run()

	base := time.Now().UTC().Truncate(time.Hour).Add(-3 * time.Hour)
	from := base
	to := base.Add(time.Hour)

	const n = 15
	for i := 0; i < n; i++ {
		count := n - i
		svc := fmt.Sprintf("svc%02d", i)
		for j := 0; j < count; j++ {
			ts := base.Add(time.Duration(i*count+j) * time.Second)
			w.Add(projectID, log.LogRecord{Timestamp: ts, ObservedTS: ts, Severity: log.SevInfo, Body: "x", Service: svc})
		}
	}

	if err := w.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := w.Dropped(); got != 0 {
		t.Fatalf("Dropped() = %d, want 0", got)
	}

	q := log.NewQuery(conn)
	got, err := q.Facet(ctx, projectID, log.ListFilter{From: from, To: to}, "service")
	if err != nil {
		t.Fatalf("Facet: %v", err)
	}
	if len(got) != 10 {
		t.Fatalf("len(got) = %d, want 10 (facetLimit): %+v", len(got), got)
	}
	for i, v := range got {
		wantSvc := fmt.Sprintf("svc%02d", i)
		wantCount := int64(n - i)
		if v.Value != wantSvc || v.Count != wantCount {
			t.Fatalf("got[%d] = %+v, want {%s %d}", i, v, wantSvc, wantCount)
		}
	}
}

// TestQueryAttrKeys — задача 5 плана C2: авто-обнаружение ключей
// log_attributes (Query.AttrKeys) — топ ключей+counts DESC и фильтрация по
// префиксу. Наивная реализация ("ARRAY JOIN mapKeys по всему окну") на
// целевом трафике (150k rpm) обрывается SETTINGS max_execution_time=5 —
// поэтому AttrKeys считает по ограниченной СВЕЖЕЙ выборке (LIMIT 50000
// внутреннего подзапроса), но это деталь стоимости, не корректности: тест
// проверяет только правильность результата на маленьком наборе (полный
// прогон 50k+ строк — отдельная забота нагрузочного тестирования, не
// unit/integration уровня).
func TestQueryAttrKeys(t *testing.T) {
	conn := testenv.MigratedCH(t)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()

	const projectID = int64(78)

	w := log.NewWriter(conn)
	go w.Run()

	base := time.Now().UTC().Truncate(time.Hour).Add(-time.Hour)
	from := base
	to := base.Add(time.Hour)

	addAttr := func(n int, attrs map[string]string) {
		for i := 0; i < n; i++ {
			ts := base.Add(time.Duration(i) * time.Second)
			w.Add(projectID, log.LogRecord{
				Timestamp: ts, ObservedTS: ts, Severity: log.SevInfo, Body: "x",
				LogAttributes: attrs,
			})
		}
	}
	addAttr(8, map[string]string{"http.method": "GET"})
	addAttr(3, map[string]string{"http.method": "POST"})
	addAttr(5, map[string]string{"http.status": "200"})
	addAttr(2, map[string]string{"other.thing": "x"})

	if err := w.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := w.Dropped(); got != 0 {
		t.Fatalf("Dropped() = %d, want 0", got)
	}

	q := log.NewQuery(conn)

	t.Run("top keys by count DESC, no prefix", func(t *testing.T) {
		got, err := q.AttrKeys(ctx, projectID, log.ListFilter{From: from, To: to}, "", 20)
		if err != nil {
			t.Fatalf("AttrKeys: %v", err)
		}
		if c := facetCount(got, "http.method"); c != 11 {
			t.Fatalf("http.method count = %d, want 11 (8 GET + 3 POST) (%+v)", c, got)
		}
		if c := facetCount(got, "http.status"); c != 5 {
			t.Fatalf("http.status count = %d, want 5 (%+v)", c, got)
		}
		if c := facetCount(got, "other.thing"); c != 2 {
			t.Fatalf("other.thing count = %d, want 2 (%+v)", c, got)
		}
		for i := 1; i < len(got); i++ {
			if got[i-1].Count < got[i].Count {
				t.Fatalf("not sorted count() DESC at %d: %+v", i, got)
			}
		}
	})

	t.Run("prefix filter", func(t *testing.T) {
		got, err := q.AttrKeys(ctx, projectID, log.ListFilter{From: from, To: to}, "http.", 20)
		if err != nil {
			t.Fatalf("AttrKeys: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("len(got) = %d, want 2 (http.method + http.status only): %+v", len(got), got)
		}
		if c := facetCount(got, "http.method"); c != 11 {
			t.Fatalf("http.method count = %d, want 11: %+v", c, got)
		}
		if c := facetCount(got, "http.status"); c != 5 {
			t.Fatalf("http.status count = %d, want 5: %+v", c, got)
		}
		if c := facetCount(got, "other.thing"); c != 0 {
			t.Fatalf("prefix \"http.\" should not match other.thing: %+v", got)
		}
	})

	t.Run("limit caps result", func(t *testing.T) {
		got, err := q.AttrKeys(ctx, projectID, log.ListFilter{From: from, To: to}, "", 1)
		if err != nil {
			t.Fatalf("AttrKeys: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("len(got) = %d, want 1 (limit): %+v", len(got), got)
		}
		if got[0].Value != "http.method" {
			t.Fatalf("got[0] = %+v, want top key http.method (count 11)", got[0])
		}
	})
}

// TestQueryAttrValues — задача 5 плана C2: значения раскрытого ключа
// атрибут-фасета (Query.AttrValues) — topN+counts DESC, mapContains-гард
// (строки без ключа НЕ должны склеиваться в бакет "") и источник
// log_attributes/resource_attrs по флагу resource.
func TestQueryAttrValues(t *testing.T) {
	conn := testenv.MigratedCH(t)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()

	const projectID = int64(79)

	w := log.NewWriter(conn)
	go w.Run()

	base := time.Now().UTC().Truncate(time.Hour).Add(-time.Hour)
	from := base
	to := base.Add(time.Hour)

	add := func(n int, logAttrs, resAttrs map[string]string) {
		for i := 0; i < n; i++ {
			ts := base.Add(time.Duration(i) * time.Second)
			w.Add(projectID, log.LogRecord{
				Timestamp: ts, ObservedTS: ts, Severity: log.SevInfo, Body: "x",
				LogAttributes: logAttrs, ResourceAttrs: resAttrs,
			})
		}
	}
	add(8, map[string]string{"http.method": "GET"}, map[string]string{"host.name": "web-01"})
	add(3, map[string]string{"http.method": "POST"}, map[string]string{"host.name": "web-01"})
	// Строки БЕЗ ключа http.method вообще (только http.status) — mapContains
	// обязан их исключить из значений http.method, а не склеить в бакет "".
	add(5, map[string]string{"http.status": "200"}, map[string]string{"host.name": "web-02"})

	if err := w.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := w.Dropped(); got != 0 {
		t.Fatalf("Dropped() = %d, want 0", got)
	}

	q := log.NewQuery(conn)

	t.Run("log_attributes values with mapContains guard", func(t *testing.T) {
		got, err := q.AttrValues(ctx, projectID, log.ListFilter{From: from, To: to}, false, "http.method", 10)
		if err != nil {
			t.Fatalf("AttrValues: %v", err)
		}
		if c := facetCount(got, "GET"); c != 8 {
			t.Fatalf("GET count = %d, want 8: %+v", c, got)
		}
		if c := facetCount(got, "POST"); c != 3 {
			t.Fatalf("POST count = %d, want 3: %+v", c, got)
		}
		if c := facetCount(got, ""); c != 0 {
			t.Fatalf("mapContains guard failed: bucket \"\" = %d, want 0 (rows without the key must not leak in): %+v", c, got)
		}
		var total int64
		for _, v := range got {
			total += v.Count
		}
		if total != 11 {
			t.Fatalf("total = %d, want 11 (8 GET + 3 POST, NOT the 5 rows without http.method): %+v", total, got)
		}
		for i := 1; i < len(got); i++ {
			if got[i-1].Count < got[i].Count {
				t.Fatalf("not sorted count() DESC at %d: %+v", i, got)
			}
		}
	})

	t.Run("resource_attrs source (resource=true)", func(t *testing.T) {
		got, err := q.AttrValues(ctx, projectID, log.ListFilter{From: from, To: to}, true, "host.name", 10)
		if err != nil {
			t.Fatalf("AttrValues: %v", err)
		}
		if c := facetCount(got, "web-01"); c != 11 {
			t.Fatalf("web-01 count = %d, want 11 (8 + 3): %+v", c, got)
		}
		if c := facetCount(got, "web-02"); c != 5 {
			t.Fatalf("web-02 count = %d, want 5: %+v", c, got)
		}
	})

	t.Run("limit caps result", func(t *testing.T) {
		got, err := q.AttrValues(ctx, projectID, log.ListFilter{From: from, To: to}, false, "http.method", 1)
		if err != nil {
			t.Fatalf("AttrValues: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("len(got) = %d, want 1 (limit): %+v", len(got), got)
		}
		if got[0].Value != "GET" {
			t.Fatalf("got[0] = %+v, want top value GET (count 8)", got[0])
		}
	})
}
