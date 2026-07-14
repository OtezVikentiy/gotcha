package ingest_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/getsentry/sentry-go"
)

func TestRealSentryGoSDK(t *testing.T) {
	s := newStack(t)

	dsn := fmt.Sprintf("http://%s@%s/%d",
		s.key.PublicKey, strings.TrimPrefix(s.srv.URL, "http://"), s.project.ID)

	err := sentry.Init(sentry.ClientOptions{
		Dsn:         dsn,
		Environment: "e2e",
		Release:     "gotcha-e2e@1.0",
	})
	if err != nil {
		t.Fatalf("sentry.Init: %v", err)
	}
	defer sentry.Flush(2 * time.Second)

	sentry.CaptureException(errors.New("gotcha e2e boom"))
	if ok := sentry.Flush(10 * time.Second); !ok {
		t.Fatal("sentry.Flush timed out — server did not accept the event")
	}

	// Issue в PG.
	waitIssue(t, s.pool, s.project.ID, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var title, level string
	if err := s.pool.QueryRow(ctx,
		"SELECT title, level FROM issues WHERE project_id=$1", s.project.ID).
		Scan(&title, &level); err != nil {
		t.Fatalf("issue: %v", err)
	}
	if !strings.Contains(title, "gotcha e2e boom") {
		t.Errorf("title = %q", title)
	}
	if level != "error" {
		t.Errorf("level = %q, want error", level)
	}

	// Событие в CH (доезжает после флаша батчера ≤5s — поллим до 20s).
	deadline := time.Now().Add(20 * time.Second)
	var cnt uint64
	for time.Now().Before(deadline) {
		if err := s.ch.QueryRow(ctx,
			"SELECT count(*) FROM events WHERE project_id = ?", uint64(s.project.ID)).
			Scan(&cnt); err == nil && cnt == 1 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if cnt != 1 {
		t.Fatalf("event count in CH = %d, want 1", cnt)
	}
	var environment, release, sdk string
	if err := s.ch.QueryRow(ctx,
		"SELECT environment, release, sdk FROM events WHERE project_id = ?",
		uint64(s.project.ID)).Scan(&environment, &release, &sdk); err != nil {
		t.Fatalf("event row: %v", err)
	}
	if environment != "e2e" || release != "gotcha-e2e@1.0" || !strings.HasPrefix(sdk, "sentry.go") {
		t.Errorf("event meta: env=%q release=%q sdk=%q", environment, release, sdk)
	}
}
