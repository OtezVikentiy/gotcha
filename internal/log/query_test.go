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
}
