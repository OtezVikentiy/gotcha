package event_test

import (
	"context"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/event"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

func TestBatcherInsertsIntoClickHouse(t *testing.T) {
	conn := testenv.MigratedCH(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	b := event.NewBatcher(conn)
	go b.Run()
	ts := time.Now().UTC().Truncate(time.Millisecond)
	b.Add(event.Event{
		ID: "550e8400-e29b-41d4-a716-446655440000", ProjectID: 7, IssueID: 42,
		Timestamp: ts, Level: "error", Message: "boom",
		ExceptionType: "ValueError", ExceptionValue: "bad",
		Stacktrace: `{"frames":[]}`, Environment: "prod", Release: "1.0",
		ServerName: "web-1", SDK: "sentry.go/0.x",
		UserID: "u1", UserIP: "10.0.0.1", UserEmail: "u@example.com",
		Tags: map[string]string{"k": "v"}, Contexts: `{}`,
	})
	if err := b.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	var cnt uint64
	var level, msg string
	var tags map[string]string
	if err := conn.QueryRow(ctx,
		"SELECT count(*) FROM events WHERE project_id = 7 AND issue_id = 42").Scan(&cnt); err != nil || cnt != 1 {
		t.Fatalf("count=%d err=%v", cnt, err)
	}
	if err := conn.QueryRow(ctx,
		"SELECT level, message, tags FROM events WHERE project_id = 7").Scan(&level, &msg, &tags); err != nil {
		t.Fatalf("select: %v", err)
	}
	if level != "error" || msg != "boom" || tags["k"] != "v" {
		t.Fatalf("row mismatch: %s %s %v", level, msg, tags)
	}
}
