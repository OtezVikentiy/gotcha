package event_test

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"gitflic.ru/otezvikentiy/gotcha/internal/event"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

func TestQueryReadsFromClickHouse(t *testing.T) {
	conn := testenv.MigratedCH(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	const projectID = int64(11)
	const issueA = int64(101)
	const issueB = int64(102)

	// Окно Series: последний час, выровненный по 10-минутным границам.
	now := time.Now().UTC()
	windowFrom := now.Truncate(10 * time.Minute).Add(-time.Hour)
	windowTo := windowFrom.Add(time.Hour)

	// issueA: 3 события внутри окна Series (враспорку).
	tsA1 := windowFrom.Add(5 * time.Minute)
	tsA2 := windowFrom.Add(25 * time.Minute)
	tsA3 := windowFrom.Add(55 * time.Minute)
	// issueB: 2 события (другой issue того же проекта), тоже внутри окна.
	tsB1 := windowFrom.Add(15 * time.Minute)
	tsB2 := windowFrom.Add(45 * time.Minute)

	idA1 := "550e8400-e29b-41d4-a716-446655440001"
	idA2 := "550e8400-e29b-41d4-a716-446655440002"
	idA3 := "550e8400-e29b-41d4-a716-446655440003"
	idB1 := "550e8400-e29b-41d4-a716-446655440004"
	idB2 := "550e8400-e29b-41d4-a716-446655440005"

	b := event.NewBatcher(conn)
	go b.Run()
	b.Add(event.Event{
		ID: idA1, ProjectID: projectID, IssueID: issueA, Timestamp: tsA1,
		Level: "error", Message: "boom A1",
		ExceptionType: "ValueError", ExceptionValue: "bad a1",
		Stacktrace:  `{"values":[{"type":"ValueError"}]}`,
		Environment: "prod", Release: "1.0", ServerName: "web-1", SDK: "sentry.go/0.x",
		UserID: "u1", UserIP: "10.0.0.1", UserEmail: "u1@example.com",
		Tags: map[string]string{"env": "prod", "seq": "a1"}, Contexts: `{}`,
	})
	b.Add(event.Event{
		ID: idA2, ProjectID: projectID, IssueID: issueA, Timestamp: tsA2,
		Level: "error", Message: "boom A2",
		ExceptionType: "ValueError", ExceptionValue: "bad a2",
		Stacktrace:  `{"values":[{"type":"ValueError"}]}`,
		Environment: "prod", Release: "1.0", ServerName: "web-1", SDK: "sentry.go/0.x",
		UserID: "u1", UserIP: "10.0.0.1", UserEmail: "u1@example.com",
		Tags: map[string]string{"env": "prod", "seq": "a2"}, Contexts: `{}`,
	})
	b.Add(event.Event{
		ID: idA3, ProjectID: projectID, IssueID: issueA, Timestamp: tsA3,
		Level: "error", Message: "boom A3",
		ExceptionType: "ValueError", ExceptionValue: "bad a3",
		Stacktrace:  `{"values":[{"type":"ValueError"}]}`,
		Environment: "prod", Release: "1.0", ServerName: "web-1", SDK: "sentry.go/0.x",
		UserID: "u1", UserIP: "10.0.0.1", UserEmail: "u1@example.com",
		Tags: map[string]string{"env": "prod", "seq": "a3"}, Contexts: `{}`,
	})
	b.Add(event.Event{
		ID: idB1, ProjectID: projectID, IssueID: issueB, Timestamp: tsB1,
		Level: "warning", Message: "boom B1",
		ExceptionType: "KeyError", ExceptionValue: "bad b1",
		Stacktrace:  `{"values":[{"type":"KeyError"}]}`,
		Environment: "prod", Release: "1.0", ServerName: "web-2", SDK: "sentry.go/0.x",
		UserID: "u2", UserIP: "10.0.0.2", UserEmail: "u2@example.com",
		Tags: map[string]string{"env": "prod", "seq": "b1"}, Contexts: `{}`,
	})
	b.Add(event.Event{
		ID: idB2, ProjectID: projectID, IssueID: issueB, Timestamp: tsB2,
		Level: "warning", Message: "boom B2",
		ExceptionType: "KeyError", ExceptionValue: "bad b2",
		Stacktrace:  `{"values":[{"type":"KeyError"}]}`,
		Environment: "prod", Release: "1.0", ServerName: "web-2", SDK: "sentry.go/0.x",
		UserID: "u2", UserIP: "10.0.0.2", UserEmail: "u2@example.com",
		Tags: map[string]string{"env": "prod", "seq": "b2"}, Contexts: `{}`,
	})
	if err := b.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	q := event.NewQuery(conn)

	t.Run("EventsForIssue", func(t *testing.T) {
		got, err := q.EventsForIssue(ctx, projectID, issueA, 10)
		if err != nil {
			t.Fatalf("EventsForIssue: %v", err)
		}
		if len(got) != 3 {
			t.Fatalf("len(got) = %d, want 3", len(got))
		}
		// DESC по timestamp: A3, A2, A1
		wantOrder := []string{idA3, idA2, idA1}
		for i, ev := range got {
			if ev.ID != wantOrder[i] {
				t.Fatalf("got[%d].ID = %s, want %s (order mismatch)", i, ev.ID, wantOrder[i])
			}
		}
		if !got[0].Timestamp.After(got[1].Timestamp) || !got[1].Timestamp.After(got[2].Timestamp) {
			t.Fatalf("events not in DESC order by timestamp: %v", got)
		}
		first := got[0] // A3
		if first.Level != "error" || first.Message != "boom A3" {
			t.Fatalf("unexpected fields: %+v", first)
		}
		if first.ExceptionType != "ValueError" || first.ExceptionValue != "bad a3" {
			t.Fatalf("unexpected exception fields: %+v", first)
		}
		if first.Stacktrace != `{"values":[{"type":"ValueError"}]}` {
			t.Fatalf("unexpected stacktrace: %s", first.Stacktrace)
		}
		if first.Environment != "prod" || first.Release != "1.0" || first.ServerName != "web-1" || first.SDK != "sentry.go/0.x" {
			t.Fatalf("unexpected meta fields: %+v", first)
		}
		if first.UserID != "u1" || first.UserIP != "10.0.0.1" || first.UserEmail != "u1@example.com" {
			t.Fatalf("unexpected user fields: %+v", first)
		}
		if first.Tags["env"] != "prod" || first.Tags["seq"] != "a3" {
			t.Fatalf("tags did not arrive: %+v", first.Tags)
		}
		if first.Contexts != "{}" {
			t.Fatalf("unexpected contexts: %s", first.Contexts)
		}
	})

	t.Run("EventByID", func(t *testing.T) {
		got, found, err := q.EventByID(ctx, projectID, idA2)
		if err != nil {
			t.Fatalf("EventByID: %v", err)
		}
		if !found {
			t.Fatalf("event %s not found", idA2)
		}
		if got.ID != idA2 || got.Message != "boom A2" {
			t.Fatalf("unexpected event: %+v", got)
		}

		// Чужой projectID — не находит.
		_, found, err = q.EventByID(ctx, projectID+1, idA2)
		if err != nil {
			t.Fatalf("EventByID wrong project: %v", err)
		}
		if found {
			t.Fatalf("event found under wrong projectID")
		}

		// Несуществующий UUID — не находит.
		_, found, err = q.EventByID(ctx, projectID, "550e8400-e29b-41d4-a716-446655449999")
		if err != nil {
			t.Fatalf("EventByID unknown id: %v", err)
		}
		if found {
			t.Fatalf("unexpected event found for unknown id")
		}
	})

	t.Run("Series", func(t *testing.T) {
		points, err := q.Series(ctx, projectID, issueA, windowFrom, windowTo, 10*time.Minute)
		if err != nil {
			t.Fatalf("Series: %v", err)
		}
		// Окно час, шаг 10м, выровнено по epoch → ровно 6 корзин. Седьмой быть не
		// может: она покрывала бы [to, to+10м), а запрос фильтрует ts < to.
		if len(points) != 6 {
			t.Fatalf("len(points) = %d, want 6", len(points))
		}
		// хронологический порядок
		for i := 1; i < len(points); i++ {
			if !points[i].T.After(points[i-1].T) {
				t.Fatalf("points not in chronological order: %v", points)
			}
		}
		var sum uint64
		for _, p := range points {
			sum += p.N
		}
		if sum != 3 {
			t.Fatalf("sum(N) = %d, want 3", sum)
		}
		// Первая точка окна должна совпасть с windowFrom.
		if !points[0].T.Equal(windowFrom) {
			t.Fatalf("points[0].T = %v, want %v", points[0].T, windowFrom)
		}
		// Пропуски заполнены нулями: в окне 6 корзин, событий только 3, значит
		// ровно 3 нуля (раньше ждали 4 — четвёртым был фантомный хвост сетки).
		var zeros int
		for _, p := range points {
			if p.N == 0 {
				zeros++
			}
		}
		if zeros < 3 {
			t.Fatalf("zeros = %d, want >= 3 (points=%v)", zeros, points)
		}

		// Test with step not dividing 24h evenly (7 minutes).
		// Must verify epoch-based grid alignment with ClickHouse toStartOfInterval.
		t.Run("epoch-aligned-7min", func(t *testing.T) {
			points7m, err := q.Series(ctx, projectID, issueA, windowFrom, windowTo, 7*time.Minute)
			if err != nil {
				t.Fatalf("Series with 7m step: %v", err)
			}
			if len(points7m) == 0 {
				t.Fatalf("len(points) = 0, got empty result")
			}
			// Sum of N across all points must equal event count.
			var sum uint64
			for _, p := range points7m {
				sum += p.N
			}
			if sum != 3 {
				t.Fatalf("sum(N) = %d, want 3 (all-zeros means grid misalignment)", sum)
			}
		})
	})

	t.Run("CountSince", func(t *testing.T) {
		// issueA has 3 events at tsA1/tsA2/tsA3 (windowFrom+5m/+25m/+55m).
		gotAll, err := q.CountSince(ctx, projectID, issueA, windowFrom)
		if err != nil {
			t.Fatalf("CountSince: %v", err)
		}
		if gotAll != 3 {
			t.Fatalf("CountSince(from windowFrom) = %d, want 3", gotAll)
		}

		// since after tsA1 but before tsA2 -> only A2, A3 counted.
		gotPartial, err := q.CountSince(ctx, projectID, issueA, tsA1.Add(time.Second))
		if err != nil {
			t.Fatalf("CountSince: %v", err)
		}
		if gotPartial != 2 {
			t.Fatalf("CountSince(from after tsA1) = %d, want 2", gotPartial)
		}

		// far future -> 0.
		gotNone, err := q.CountSince(ctx, projectID, issueA, now.Add(24*time.Hour))
		if err != nil {
			t.Fatalf("CountSince: %v", err)
		}
		if gotNone != 0 {
			t.Fatalf("CountSince(from future) = %d, want 0", gotNone)
		}

		// other issue in same project unaffected.
		gotB, err := q.CountSince(ctx, projectID, issueB, windowFrom)
		if err != nil {
			t.Fatalf("CountSince: %v", err)
		}
		if gotB != 2 {
			t.Fatalf("CountSince(issueB) = %d, want 2", gotB)
		}
	})

	t.Run("Sparklines", func(t *testing.T) {
		since := now.Add(-24 * time.Hour)
		out, err := q.Sparklines(ctx, projectID, []int64{issueA, issueB}, since, 24)
		if err != nil {
			t.Fatalf("Sparklines: %v", err)
		}
		if len(out) != 2 {
			t.Fatalf("len(out) = %d, want 2", len(out))
		}
		for _, id := range []int64{issueA, issueB} {
			buckets, ok := out[id]
			if !ok {
				t.Fatalf("missing issue %d in sparklines result", id)
			}
			if len(buckets) != 24 {
				t.Fatalf("issue %d: len(buckets) = %d, want 24", id, len(buckets))
			}
		}
		var sumA, sumB uint64
		for _, n := range out[issueA] {
			sumA += n
		}
		for _, n := range out[issueB] {
			sumB += n
		}
		if sumA != 3 {
			t.Fatalf("sumA = %d, want 3", sumA)
		}
		if sumB != 2 {
			t.Fatalf("sumB = %d, want 2", sumB)
		}
	})
}

// TestStreamForExportOrdersByIssueThenTime — обход выгрузки обязан идти
// ГРУППАМИ в порядке списка issueIDs, внутри группы — timestamp DESC (K4-2,
// аудит перед 1.0): issueIDs передаётся вызывающим уже отсортированным
// (last_seen DESC — самые активные группы первыми), а не по возрастанию
// issue_id, и именно порядок списка обязан определять, какие группы
// усечение LIMIT оставит первыми (см. TestStreamForExportTruncation
// KeepsFirstListedIssues). Список ниже намеренно ставит issue2 (числом
// БОЛЬШЕ issue1) первым — тест обязан провалиться, если реализация
// вернулась бы к сортировке по возрастанию issue_id вместо порядка списка.
func TestStreamForExportOrdersByIssueThenTime(t *testing.T) {
	ctx := context.Background()
	conn := testenv.MigratedCH(t)
	const projectID = int64(51001)
	const issue1 = int64(910001)
	const issue2 = int64(910002)

	t0 := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	t1 := t0.Add(1 * time.Minute)
	t2 := t0.Add(2 * time.Minute)
	t3 := t0.Add(3 * time.Minute)

	b := event.NewBatcher(conn)
	go b.Run()
	b.Add(event.Event{ID: uuid.NewString(), ProjectID: projectID, IssueID: issue2, Timestamp: t2, Level: "error", Message: "2a"})
	b.Add(event.Event{ID: uuid.NewString(), ProjectID: projectID, IssueID: issue1, Timestamp: t1, Level: "error", Message: "1a"})
	b.Add(event.Event{ID: uuid.NewString(), ProjectID: projectID, IssueID: issue1, Timestamp: t3, Level: "error", Message: "1b"})
	b.Add(event.Event{ID: uuid.NewString(), ProjectID: projectID, IssueID: issue2, Timestamp: t1, Level: "error", Message: "2b"})
	if err := b.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	q := event.NewQuery(conn)
	var got []string
	err := q.StreamForExport(ctx, projectID, []int64{issue2, issue1}, t0, t0.Add(time.Hour), 100, func(ev event.Stored) error {
		got = append(got, fmt.Sprintf("%d@%s", ev.IssueID, ev.Timestamp.UTC().Format(time.RFC3339)))
		return nil
	})
	if err != nil {
		t.Fatalf("StreamForExport: %v", err)
	}
	want := []string{
		fmt.Sprintf("%d@%s", issue2, t2.Format(time.RFC3339)),
		fmt.Sprintf("%d@%s", issue2, t1.Format(time.RFC3339)),
		fmt.Sprintf("%d@%s", issue1, t3.Format(time.RFC3339)),
		fmt.Sprintf("%d@%s", issue1, t1.Format(time.RFC3339)),
	}
	if !slices.Equal(got, want) {
		t.Errorf("порядок строк = %v, want %v (порядок списка issueIDs, не возрастание issue_id)", got, want)
	}
}

// TestStreamForExportFollowsGivenIssueOrder — три группы, список issueIDs в
// произвольном порядке (не по возрастанию и не по времени вставки): обход
// обязан вернуть строки группами строго в порядке списка (K4-2).
func TestStreamForExportFollowsGivenIssueOrder(t *testing.T) {
	ctx := context.Background()
	conn := testenv.MigratedCH(t)
	const projectID = int64(51003)
	const issueA = int64(930001)
	const issueB = int64(930002)
	const issueC = int64(930003)

	t0 := time.Date(2026, 8, 22, 10, 0, 0, 0, time.UTC)

	b := event.NewBatcher(conn)
	go b.Run()
	b.Add(event.Event{ID: uuid.NewString(), ProjectID: projectID, IssueID: issueA, Timestamp: t0, Level: "error", Message: "a"})
	b.Add(event.Event{ID: uuid.NewString(), ProjectID: projectID, IssueID: issueB, Timestamp: t0, Level: "error", Message: "b"})
	b.Add(event.Event{ID: uuid.NewString(), ProjectID: projectID, IssueID: issueC, Timestamp: t0, Level: "error", Message: "c"})
	if err := b.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	q := event.NewQuery(conn)
	var got []int64
	err := q.StreamForExport(ctx, projectID, []int64{issueC, issueA, issueB}, t0, t0.Add(time.Hour), 100, func(ev event.Stored) error {
		got = append(got, ev.IssueID)
		return nil
	})
	if err != nil {
		t.Fatalf("StreamForExport: %v", err)
	}
	want := []int64{issueC, issueA, issueB}
	if !slices.Equal(got, want) {
		t.Errorf("порядок групп = %v, want %v (порядок списка issueIDs)", got, want)
	}
}

// TestStreamForExportTruncationKeepsFirstListedIssues — усечение LIMIT
// обязано отбросить наименее активные группы (последние в списке issueIDs,
// который вызывающий сортирует по last_seen DESC), а не произвольные
// строки, оставшиеся после сортировки по issue_id (K4-2): C — первая в
// списке и самая «активная» по числу событий, A и B — позади неё. LIMIT,
// равный числу событий C, обязан вернуть ровно события C и ни одного
// события A/B.
func TestStreamForExportTruncationKeepsFirstListedIssues(t *testing.T) {
	ctx := context.Background()
	conn := testenv.MigratedCH(t)
	const projectID = int64(51004)
	const issueA = int64(930011)
	const issueB = int64(930012)
	const issueC = int64(930013)
	const cEvents = 4

	t0 := time.Date(2026, 8, 22, 11, 0, 0, 0, time.UTC)

	b := event.NewBatcher(conn)
	go b.Run()
	for i := 0; i < cEvents; i++ {
		b.Add(event.Event{ID: uuid.NewString(), ProjectID: projectID, IssueID: issueC,
			Timestamp: t0.Add(time.Duration(i) * time.Minute), Level: "error", Message: "c"})
	}
	for i := 0; i < 2; i++ {
		b.Add(event.Event{ID: uuid.NewString(), ProjectID: projectID, IssueID: issueA,
			Timestamp: t0.Add(time.Duration(i) * time.Minute), Level: "error", Message: "a"})
		b.Add(event.Event{ID: uuid.NewString(), ProjectID: projectID, IssueID: issueB,
			Timestamp: t0.Add(time.Duration(i) * time.Minute), Level: "error", Message: "b"})
	}
	if err := b.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	q := event.NewQuery(conn)
	var got []int64
	err := q.StreamForExport(ctx, projectID, []int64{issueC, issueA, issueB}, t0, t0.Add(time.Hour), cEvents, func(ev event.Stored) error {
		got = append(got, ev.IssueID)
		return nil
	})
	if err != nil {
		t.Fatalf("StreamForExport: %v", err)
	}
	if len(got) != cEvents {
		t.Fatalf("получено %d строк, want %d (LIMIT = число событий C)", len(got), cEvents)
	}
	for _, id := range got {
		if id != issueC {
			t.Errorf("усечение отдало событие группы %d — ожидали только группу %d, первую в списке issueIDs", id, issueC)
		}
	}
}

// TestStreamForExportRespectsLimit — LIMIT в запросе обязан реально
// ограничивать число строк, а не быть декоративным параметром.
func TestStreamForExportRespectsLimit(t *testing.T) {
	ctx := context.Background()
	conn := testenv.MigratedCH(t)
	const projectID = int64(51002)
	const issueID = int64(920001)
	t0 := time.Date(2026, 8, 21, 10, 0, 0, 0, time.UTC)

	b := event.NewBatcher(conn)
	go b.Run()
	for i := 0; i < 5; i++ {
		b.Add(event.Event{ID: uuid.NewString(), ProjectID: projectID, IssueID: issueID,
			Timestamp: t0.Add(time.Duration(i) * time.Minute), Level: "error", Message: "m"})
	}
	if err := b.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	q := event.NewQuery(conn)
	var n int
	err := q.StreamForExport(ctx, projectID, []int64{issueID}, t0, t0.Add(time.Hour), 2, func(event.Stored) error {
		n++
		return nil
	})
	if err != nil {
		t.Fatalf("StreamForExport: %v", err)
	}
	if n != 2 {
		t.Errorf("получено %d строк, want ровно 2 (limit)", n)
	}
}

// TestStreamForExportTiedTimestampsNoLossOrDuplication — несколько событий
// одной группы с ОДИНАКОВЫМ timestamp: ORDER BY issue_id, timestamp DESC не
// уникален внутри такой группы, но обход не должен ни терять, ни задваивать
// строки, пока LIMIT не отсекает часть связки.
func TestStreamForExportTiedTimestampsNoLossOrDuplication(t *testing.T) {
	ctx := context.Background()
	conn := testenv.MigratedCH(t)
	const projectID = int64(51003)
	const issueID = int64(930001)
	tied := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)

	ids := []string{uuid.NewString(), uuid.NewString(), uuid.NewString()}
	b := event.NewBatcher(conn)
	go b.Run()
	for _, id := range ids {
		b.Add(event.Event{ID: id, ProjectID: projectID, IssueID: issueID, Timestamp: tied, Level: "error", Message: "m"})
	}
	if err := b.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	q := event.NewQuery(conn)
	seen := map[string]int{}
	err := q.StreamForExport(ctx, projectID, []int64{issueID}, tied.Add(-time.Minute), tied.Add(time.Minute), len(ids),
		func(ev event.Stored) error {
			seen[ev.ID]++
			return nil
		})
	if err != nil {
		t.Fatalf("StreamForExport: %v", err)
	}
	if len(seen) != len(ids) {
		t.Fatalf("получено %d уникальных id при одинаковом timestamp, want %d: %v", len(seen), len(ids), seen)
	}
	for id, c := range seen {
		if c != 1 {
			t.Errorf("id %s встретился %d раз, want 1 (дубликат)", id, c)
		}
	}
}

// TestStreamForExportDoesNotSortByComputedKey — закрепление I2 (финревью
// волны 1 аудита перед 1.0): ORDER BY transform(issue_id, …) исключает
// read-in-order при любой версии ClickHouse — сервер обязан прочитать и
// отсортировать весь отфильтрованный набор целиком вместо потокового
// чтения по первичному ключу (project_id, issue_id, timestamp). Порядок
// строк (группами в порядке issueIDs, внутри группы timestamp DESC)
// закреплён поведенческими тестами выше и обязан сохраняться без
// вычисляемого ключа сортировки — эта проверка ловит только регресс
// СПОСОБА, а не результата: возврат к transform() молча вернул бы прежний
// результат на тестовых объёмах, но заново снял бы потоковость на проде.
func TestStreamForExportDoesNotSortByComputedKey(t *testing.T) {
	src, err := os.ReadFile("query.go")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if strings.Contains(string(src), "ORDER BY transform") {
		t.Error("query.go снова сортирует по вычисляемому ключу (transform(...)) — " +
			"это исключает optimize_read_in_order и заставляет ClickHouse " +
			"материализовать и сортировать весь отфильтрованный набор целиком (I2)")
	}
}
