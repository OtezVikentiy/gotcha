package uptime

// Concurrency tests for the SSL/reminder claim-before-notify fix. These live
// in package uptime (not uptime_test) because they call checkSSL/
// checkReminders directly — both unexported — to simulate two
// `gotcha --mode=uptime` replicas racing checkSSL (resp. checkReminders)
// against the same monitor/incident in the same DB. The rest of the
// package's tests use the external uptime_test package and its own
// fakeNotifier/newProject helpers; those aren't reachable from here (package
// uptime can't import uptime_test — uptime_test imports uptime, so the
// reverse would cycle), hence the small duplicated helpers below.

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// countingNotifier — minimal Notifier for these tests: counts Notify calls
// per Event.Kind. The external fakeNotifier (watchdog_test.go, package
// uptime_test) records full events, which these tests don't need.
type countingNotifier struct {
	mu     sync.Mutex
	counts map[string]int
}

func (n *countingNotifier) Notify(_ context.Context, ev Event) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.counts == nil {
		n.counts = map[string]int{}
	}
	n.counts[ev.Kind]++
	return nil
}

func (n *countingNotifier) count(kind string) int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.counts[kind]
}

// newConcurrencyTestProject inserts the minimal user/org/project chain —
// mirrors uptime_test's newProject (monitor_test.go); duplicated here since
// this package's test files can't reach that external-package helper (see
// the package comment above).
func newConcurrencyTestProject(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	ctx := context.Background()
	n := time.Now().UnixNano()
	var userID, orgID, projectID int64
	if err := pool.QueryRow(ctx,
		"INSERT INTO users (email, password_hash) VALUES ($1,'x') RETURNING id",
		fmt.Sprintf("wdc-%d@example.com", n)).Scan(&userID); err != nil {
		t.Fatalf("user: %v", err)
	}
	if err := pool.QueryRow(ctx,
		"INSERT INTO organizations (slug, name, event_quota) VALUES ($1,'WD',1000000) RETURNING id",
		fmt.Sprintf("wdc-%d", n)).Scan(&orgID); err != nil {
		t.Fatalf("org: %v", err)
	}
	if err := pool.QueryRow(ctx,
		"INSERT INTO projects (org_id, slug, name) VALUES ($1,'api','API') RETURNING id", orgID).Scan(&projectID); err != nil {
		t.Fatalf("project: %v", err)
	}
	return projectID
}

func concurrencyTestHTTPMonitor(projectID int64) Monitor {
	return Monitor{
		ProjectID:         projectID,
		Name:              "concurrency",
		Kind:              KindHTTP,
		Enabled:           true,
		IntervalSeconds:   60,
		TimeoutSeconds:    10,
		FailThreshold:     1,
		RecoveryThreshold: 1,
		Consensus:         ConsensusMajority,
		SSLAlertDays:      14,
		Config:            json.RawMessage(`{"method":"GET","url":"https://example.com/health"}`),
	}
}

// TestCheckSSLConcurrentTicksNotifyExactlyOnce simulates two
// `--mode=uptime` replicas whose watchdog ticks land on checkSSL at the same
// moment for the same monitor: both read the same not-yet-alerted
// thresholds, so without an atomic claim (see ClaimSSLAlert) both would
// Notify. Exactly one must win.
func TestCheckSSLConcurrentTicksNotifyExactlyOnce(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pid := newConcurrencyTestProject(t, pool)
	created, err := svc.Create(ctx, concurrencyTestHTTPMonitor(pid), []string{"local"}, nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	expires := time.Now().UTC().Add(4*24*time.Hour + 12*time.Hour) // daysLeft == 5
	if err := svc.SetSSLExpiry(ctx, created.ID, expires); err != nil {
		t.Fatalf("SetSSLExpiry: %v", err)
	}

	notifier := &countingNotifier{}
	wd := &Watchdog{Svc: svc, Notifier: notifier, Region: "local"}

	const n = 20
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			wd.checkSSL(ctx)
		}()
	}
	close(start)
	wg.Wait()

	if got := notifier.count("ssl_expiring"); got != 1 {
		t.Fatalf("ssl_expiring notifications = %d, want exactly 1 (two replicas raced checkSSL on the same monitor)", got)
	}

	var alerted []int
	if err := pool.QueryRow(ctx, "SELECT ssl_alerted_days FROM monitors WHERE id = $1", created.ID).Scan(&alerted); err != nil {
		t.Fatalf("select ssl_alerted_days: %v", err)
	}
	alertedSet := map[int]bool{}
	for _, d := range alerted {
		alertedSet[d] = true
	}
	if !alertedSet[14] || !alertedSet[7] {
		t.Fatalf("ssl_alerted_days = %v, want it to contain 14 and 7", alerted)
	}
}

// TestCheckRemindersConcurrentTicksNotifyExactlyOnce mirrors the SSL test
// above for checkReminders/ClaimReminder: two replicas racing on the same
// open incident, due for exactly one reminder.
func TestCheckRemindersConcurrentTicksNotifyExactlyOnce(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pid := newConcurrencyTestProject(t, pool)
	m := concurrencyTestHTTPMonitor(pid)
	m.RemindEveryMinutes = 10
	created, err := svc.Create(ctx, m, []string{"local"}, nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	inc, openedNow, err := svc.OpenIncident(ctx, created.ID, "concurrent down", []string{"local"}, false)
	if err != nil {
		t.Fatalf("OpenIncident: %v", err)
	}
	if !openedNow {
		t.Fatalf("OpenIncident: created = false, want true")
	}
	if _, err := pool.Exec(ctx,
		"UPDATE incidents SET started_at = started_at - interval '30 minutes' WHERE id = $1", inc.ID); err != nil {
		t.Fatalf("backdate incident: %v", err)
	}

	notifier := &countingNotifier{}
	wd := &Watchdog{Svc: svc, Notifier: notifier, Region: "local"}

	const n = 20
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			wd.checkReminders(ctx)
		}()
	}
	close(start)
	wg.Wait()

	if got := notifier.count("reminder"); got != 1 {
		t.Fatalf("reminder notifications = %d, want exactly 1 (two replicas raced checkReminders on the same incident)", got)
	}

	var lastRemindedAt *time.Time
	if err := pool.QueryRow(ctx, "SELECT last_reminded_at FROM incidents WHERE id = $1", inc.ID).Scan(&lastRemindedAt); err != nil {
		t.Fatalf("select last_reminded_at: %v", err)
	}
	if lastRemindedAt == nil {
		t.Fatalf("last_reminded_at is nil, want it set")
	}
}
