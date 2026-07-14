package event_test

import (
	"context"
	"testing"
	"time"

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
		if len(points) != 7 {
			t.Fatalf("len(points) = %d, want 7", len(points))
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
		// Пропуски заполнены нулями: должна быть хотя бы одна нулевая точка
		// (в окне 7 точек, событий только 3, значит минимум 4 нуля).
		var zeros int
		for _, p := range points {
			if p.N == 0 {
				zeros++
			}
		}
		if zeros < 4 {
			t.Fatalf("zeros = %d, want >= 4 (points=%v)", zeros, points)
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
