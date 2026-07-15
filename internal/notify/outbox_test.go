package notify_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/notify"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// newChannel: прямые вставки — notify-пакет не зависит от alert.
func newChannel(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	_, channelID := newChannelInProject(t, pool)
	return channelID
}

// orgProjectSeq — счётчик для уникальных org/project slug'ов, когда один
// тест заводит несколько org/project через newChannelInProject (slug
// организации/проекта уникален по схеме).
var orgProjectSeq int64

// newChannelInProject — как newChannel, но также возвращает projectID
// (нужен FailedForProject-тестам, которые фильтруют по проекту). Каждый
// вызов заводит org/project с уникальным slug'ом, чтобы один тест мог
// вызвать его несколько раз (два "проекта" в одном сценарии) без коллизии
// по organizations_slug_key / projects_slug_key.
func newChannelInProject(t *testing.T, pool *pgxpool.Pool) (projectID, channelID int64) {
	t.Helper()
	ctx := context.Background()
	n := atomic.AddInt64(&orgProjectSeq, 1)
	slug := fmt.Sprintf("notifyorg%d", n)
	var orgID int64
	if err := pool.QueryRow(ctx,
		"INSERT INTO organizations (slug, name, event_quota) VALUES ($1,'Notify Org',1000000) RETURNING id", slug).Scan(&orgID); err != nil {
		t.Fatalf("org: %v", err)
	}
	if err := pool.QueryRow(ctx,
		"INSERT INTO projects (org_id, slug, name) VALUES ($1,'api','API') RETURNING id", orgID).Scan(&projectID); err != nil {
		t.Fatalf("project: %v", err)
	}
	if err := pool.QueryRow(ctx,
		"INSERT INTO alert_channels (project_id, kind, target) VALUES ($1,'email','ops@example.com') RETURNING id",
		projectID).Scan(&channelID); err != nil {
		t.Fatalf("channel: %v", err)
	}
	return projectID, channelID
}

func TestOutboxLifecycle(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ob := notify.NewOutbox(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	chID := newChannel(t, pool)

	if err := ob.Enqueue(ctx, chID, map[string]any{"issue_id": float64(42), "title": "boom"}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	jobs, err := ob.Claim(ctx, 10)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("Claim: %+v err=%v, want 1 job", jobs, err)
	}
	job := jobs[0]
	if job.ChannelID != chID || job.Attempts != 1 {
		t.Errorf("job = %+v, want channel=%d attempts=1", job, chID)
	}
	if job.Payload["title"] != "boom" || job.Payload["issue_id"] != float64(42) {
		t.Errorf("job.Payload = %+v", job.Payload)
	}

	// Задача уже забрана (не pending) — второй Claim её не видит.
	jobs, err = ob.Claim(ctx, 10)
	if err != nil || len(jobs) != 0 {
		t.Fatalf("Claim (already claimed): %+v err=%v, want 0", jobs, err)
	}

	if err := ob.MarkSent(ctx, job.ID); err != nil {
		t.Fatalf("MarkSent: %v", err)
	}
	if err := ob.MarkSent(ctx, 999999); !errors.Is(err, notify.ErrNotFound) {
		t.Fatalf("MarkSent (missing): got %v, want ErrNotFound", err)
	}

	var status string
	var sentAt *time.Time
	if err := pool.QueryRow(ctx,
		"SELECT status, sent_at FROM notification_outbox WHERE id = $1", job.ID).Scan(&status, &sentAt); err != nil {
		t.Fatalf("select after MarkSent: %v", err)
	}
	if status != "sent" || sentAt == nil {
		t.Errorf("after MarkSent: status=%q sent_at=%v, want sent/non-nil", status, sentAt)
	}
}

func TestOutboxRetryAndFailed(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ob := notify.NewOutbox(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	chID := newChannel(t, pool)

	if err := ob.Enqueue(ctx, chID, map[string]any{"a": "b"}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	jobs, err := ob.Claim(ctx, 10)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("Claim: %+v err=%v", jobs, err)
	}
	job := jobs[0]

	future := time.Now().Add(time.Hour).UTC()
	if err := ob.MarkRetry(ctx, job.ID, errors.New("smtp timeout"), future); err != nil {
		t.Fatalf("MarkRetry: %v", err)
	}
	// next_retry_at в будущем — не должна быть заклеймлена снова.
	jobs, err = ob.Claim(ctx, 10)
	if err != nil || len(jobs) != 0 {
		t.Fatalf("Claim after retry (future): %+v err=%v, want 0", jobs, err)
	}

	var status, lastErr string
	if err := pool.QueryRow(ctx,
		"SELECT status, last_error FROM notification_outbox WHERE id = $1", job.ID).Scan(&status, &lastErr); err != nil {
		t.Fatalf("select after MarkRetry: %v", err)
	}
	if status != "pending" || lastErr != "smtp timeout" {
		t.Errorf("after MarkRetry: status=%q last_error=%q", status, lastErr)
	}

	if err := ob.MarkFailed(ctx, job.ID, errors.New("giving up")); err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}
	if err := pool.QueryRow(ctx,
		"SELECT status, last_error FROM notification_outbox WHERE id = $1", job.ID).Scan(&status, &lastErr); err != nil {
		t.Fatalf("select after MarkFailed: %v", err)
	}
	if status != "failed" || lastErr != "giving up" {
		t.Errorf("after MarkFailed: status=%q last_error=%q", status, lastErr)
	}
}

// TestClaimConcurrentNoOverlap: две горутины забирают из одной очереди
// одновременно — FOR UPDATE SKIP LOCKED гарантирует, что задачи не
// пересекаются между вызовами.
func TestClaimConcurrentNoOverlap(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ob := notify.NewOutbox(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	chID := newChannel(t, pool)

	const total = 40
	for i := 0; i < total; i++ {
		if err := ob.Enqueue(ctx, chID, map[string]any{"n": float64(i)}); err != nil {
			t.Fatalf("Enqueue %d: %v", i, err)
		}
	}

	var mu sync.Mutex
	seen := make(map[int64]int)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				jobs, err := ob.Claim(ctx, 3)
				if err != nil {
					errs <- fmt.Errorf("claim: %w", err)
					return
				}
				if len(jobs) == 0 {
					return
				}
				mu.Lock()
				for _, j := range jobs {
					seen[j.ID]++
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}

	if len(seen) != total {
		t.Fatalf("claimed %d distinct jobs, want %d", len(seen), total)
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("job %d claimed %d times, want exactly 1", id, n)
		}
	}
}

// insertChannel inserts an alert_channels row of the given kind/target
// directly (notify doesn't depend on alert), returning its id.
func insertChannel(t *testing.T, pool *pgxpool.Pool, projectID int64, kind, target string) int64 {
	t.Helper()
	var channelID int64
	if err := pool.QueryRow(context.Background(),
		"INSERT INTO alert_channels (project_id, kind, target) VALUES ($1,$2,$3) RETURNING id",
		projectID, kind, target).Scan(&channelID); err != nil {
		t.Fatalf("insert channel: %v", err)
	}
	return channelID
}

// TestOutboxFailedForProject — spec §7: failed уведомления должны быть
// видны в настройках проекта. FailedForProject должна: (1) вернуть только
// failed-задачи (не pending/sent), (2) join'ить channel_kind/target из
// alert_channels (payload сам по себе не отдаётся — там могут быть секреты
// вроде webhook HMAC-ключа или telegram bot token), (3) не задевать чужие
// проекты.
func TestOutboxFailedForProject(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ob := notify.NewOutbox(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	projectID, emailChID := newChannelInProject(t, pool)
	webhookChID := insertChannel(t, pool, projectID, "webhook", "https://hooks.example.com/in")

	// Failed job on the email channel.
	if err := ob.Enqueue(ctx, emailChID, map[string]any{"secret": "should-not-leak", "title": "boom"}); err != nil {
		t.Fatalf("enqueue email: %v", err)
	}
	jobs, err := ob.Claim(ctx, 10)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("claim email job: %+v err=%v", jobs, err)
	}
	if err := ob.MarkFailed(ctx, jobs[0].ID, errors.New("smtp: connection refused")); err != nil {
		t.Fatalf("mark failed email: %v", err)
	}

	// A second, still-pending job on the webhook channel of the SAME
	// project: must NOT show up in FailedForProject (status filter).
	if err := ob.Enqueue(ctx, webhookChID, map[string]any{"title": "pending one"}); err != nil {
		t.Fatalf("enqueue webhook pending: %v", err)
	}

	// A failed job belonging to a DIFFERENT project: must not leak across
	// projects. Claim(ctx, 10) also re-claims the still-pending webhook job
	// enqueued above (it's a different project's job, but Claim isn't
	// project-scoped) — pick out the one belonging to otherChID and only
	// fail that one, leaving the webhook job's status as 'pending'.
	_, otherChID := newChannelInProject(t, pool)
	if err := ob.Enqueue(ctx, otherChID, map[string]any{"title": "other project"}); err != nil {
		t.Fatalf("enqueue other project: %v", err)
	}
	claimed, err := ob.Claim(ctx, 10)
	if err != nil {
		t.Fatalf("claim other project job: %v", err)
	}
	var otherJobID int64
	found := false
	for _, j := range claimed {
		if j.ChannelID == otherChID {
			otherJobID = j.ID
			found = true
		}
	}
	if !found {
		t.Fatalf("claimed jobs %+v did not include the other-project job (channel %d)", claimed, otherChID)
	}
	if err := ob.MarkFailed(ctx, otherJobID, errors.New("other project failure")); err != nil {
		t.Fatalf("mark failed other project: %v", err)
	}

	failed, err := ob.FailedForProject(ctx, projectID, 10)
	if err != nil {
		t.Fatalf("FailedForProject: %v", err)
	}
	if len(failed) != 1 {
		t.Fatalf("FailedForProject = %+v, want 1 entry", failed)
	}
	f := failed[0]
	if f.ChannelKind != "email" {
		t.Errorf("ChannelKind = %q, want email", f.ChannelKind)
	}
	if f.Target != "ops@example.com" {
		t.Errorf("Target = %q, want ops@example.com", f.Target)
	}
	if f.LastError != "smtp: connection refused" {
		t.Errorf("LastError = %q, want %q", f.LastError, "smtp: connection refused")
	}
	if f.Attempts != 1 {
		t.Errorf("Attempts = %d, want 1", f.Attempts)
	}
	if f.CreatedAt.IsZero() {
		t.Errorf("CreatedAt is zero, want set")
	}
	if f.ID != jobs[0].ID {
		t.Errorf("ID = %d, want %d", f.ID, jobs[0].ID)
	}
}

func TestOutboxPurgeOld(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ob := notify.NewOutbox(pool)
	ctx := context.Background()
	chID := newChannel(t, pool)

	ins := func(status string, ageDays int) {
		if _, err := pool.Exec(ctx,
			"INSERT INTO notification_outbox (channel_id, payload, status, created_at) VALUES ($1,'{}',$2, now() - make_interval(days => $3))",
			chID, status, ageDays); err != nil {
			t.Fatalf("insert %s: %v", status, err)
		}
	}
	ins("sent", 10)    // старый доставленный → удалить
	ins("failed", 10)  // старый проваленный → удалить
	ins("pending", 10) // старый, но pending → щадим
	ins("sent", 1)     // свежий доставленный → щадим

	deleted, err := ob.PurgeOld(ctx, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("PurgeOld: %v", err)
	}
	if deleted != 2 {
		t.Fatalf("deleted = %d, want 2", deleted)
	}
	var remaining int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM notification_outbox WHERE channel_id = $1", chID).Scan(&remaining); err != nil {
		t.Fatalf("count: %v", err)
	}
	if remaining != 2 {
		t.Fatalf("remaining = %d, want 2 (pending + fresh sent)", remaining)
	}
}
