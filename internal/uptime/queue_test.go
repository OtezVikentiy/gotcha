package uptime_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
)

func mustCreateMonitor(t *testing.T, svc *uptime.Service, ctx context.Context, m uptime.Monitor, regions []string) uptime.Monitor {
	t.Helper()
	created, err := svc.Create(ctx, m, regions, nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return created
}

func TestScheduleQueuesDueMonitorAndSkipsWhilePending(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pid := newProject(t, pool)
	m := baseHTTPMonitor(pid)
	m.IntervalSeconds = 30
	m.TimeoutSeconds = 5
	m.Config = httpConfig(t, uptime.HTTPConfig{Method: "GET", URL: "https://example.com/health"})
	created := mustCreateMonitor(t, svc, ctx, m, []string{"local"})

	n, err := svc.Schedule(ctx)
	if err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	if n != 1 {
		t.Fatalf("Schedule() = %d, want 1", n)
	}

	pending, err := svc.PendingCount(ctx)
	if err != nil {
		t.Fatalf("PendingCount: %v", err)
	}
	if pending != 1 {
		t.Fatalf("PendingCount() = %d, want 1", pending)
	}

	// Force the monitor to look "due" again by rewinding last_scheduled_at,
	// but the job is still sitting in the queue (not completed) — the
	// unique (monitor_id, region) index must stop a duplicate from being
	// queued.
	if _, err := pool.Exec(ctx, "UPDATE monitors SET last_scheduled_at = now() - interval '1 hour' WHERE id = $1", created.ID); err != nil {
		t.Fatalf("rewind last_scheduled_at: %v", err)
	}

	// Read last_scheduled_at after rewinding but before the second Schedule call.
	var lastScheduledAtBefore *time.Time
	if err := pool.QueryRow(ctx, "SELECT last_scheduled_at FROM monitors WHERE id = $1", created.ID).Scan(&lastScheduledAtBefore); err != nil {
		t.Fatalf("select last_scheduled_at (before 2nd schedule): %v", err)
	}

	n2, err := svc.Schedule(ctx)
	if err != nil {
		t.Fatalf("Schedule (2nd): %v", err)
	}
	if n2 != 0 {
		t.Fatalf("Schedule() (2nd) = %d, want 0 (job still pending)", n2)
	}

	// Read last_scheduled_at after the second Schedule call.
	var lastScheduledAtAfter *time.Time
	if err := pool.QueryRow(ctx, "SELECT last_scheduled_at FROM monitors WHERE id = $1", created.ID).Scan(&lastScheduledAtAfter); err != nil {
		t.Fatalf("select last_scheduled_at (after 2nd schedule): %v", err)
	}

	// The key assertion: last_scheduled_at should NOT have changed on the second
	// Schedule call, because the job was already pending (INSERT was skipped by
	// ON CONFLICT DO NOTHING). If last_scheduled_at advances despite the job
	// not being queued, the effective check cadence stretches.
	if lastScheduledAtBefore == nil || lastScheduledAtAfter == nil {
		t.Fatalf("last_scheduled_at before=%v after=%v, both should be set", lastScheduledAtBefore, lastScheduledAtAfter)
	}
	if !lastScheduledAtBefore.Equal(*lastScheduledAtAfter) {
		t.Fatalf("last_scheduled_at should not have changed: before=%v after=%v (changed by %.3f sec)",
			lastScheduledAtBefore, lastScheduledAtAfter,
			lastScheduledAtAfter.Sub(*lastScheduledAtBefore).Seconds())
	}

	pending2, err := svc.PendingCount(ctx)
	if err != nil {
		t.Fatalf("PendingCount (2nd): %v", err)
	}
	if pending2 != 1 {
		t.Fatalf("PendingCount() (2nd) = %d, want 1 (no duplicate)", pending2)
	}
}

func TestScheduleUpdatesLastScheduledAt(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pid := newProject(t, pool)
	m := baseHTTPMonitor(pid)
	m.Config = httpConfig(t, uptime.HTTPConfig{Method: "GET", URL: "https://example.com/health"})
	created := mustCreateMonitor(t, svc, ctx, m, []string{"local"})

	before := time.Now().UTC()
	if _, err := svc.Schedule(ctx); err != nil {
		t.Fatalf("Schedule: %v", err)
	}

	var lastScheduledAt *time.Time
	if err := pool.QueryRow(ctx, "SELECT last_scheduled_at FROM monitors WHERE id = $1", created.ID).Scan(&lastScheduledAt); err != nil {
		t.Fatalf("select last_scheduled_at: %v", err)
	}
	if lastScheduledAt == nil {
		t.Fatalf("last_scheduled_at is NULL, want set")
	}
	if lastScheduledAt.Before(before.Add(-time.Second)) {
		t.Fatalf("last_scheduled_at = %v, want >= %v", lastScheduledAt, before)
	}
}

func TestScheduleSkipsDisabledAndHeartbeat(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pid := newProject(t, pool)

	disabled := baseHTTPMonitor(pid)
	disabled.Name = "Disabled"
	disabled.Enabled = false
	disabled.Config = httpConfig(t, uptime.HTTPConfig{Method: "GET", URL: "https://example.com/health"})
	mustCreateMonitor(t, svc, ctx, disabled, []string{"local"})

	hb := baseHTTPMonitor(pid)
	hb.Name = "Heartbeat"
	hb.Kind = uptime.KindHeartbeat
	hb.Config = heartbeatConfig(t, uptime.HeartbeatConfig{GraceSeconds: 60})
	mustCreateMonitor(t, svc, ctx, hb, []string{"local"})

	n, err := svc.Schedule(ctx)
	if err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	if n != 0 {
		t.Fatalf("Schedule() = %d, want 0 (disabled + heartbeat monitors excluded)", n)
	}

	pending, err := svc.PendingCount(ctx)
	if err != nil {
		t.Fatalf("PendingCount: %v", err)
	}
	if pending != 0 {
		t.Fatalf("PendingCount() = %d, want 0", pending)
	}
}

func TestScheduleTwoRegionsProducesTwoJobs(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pid := newProject(t, pool)
	m := baseHTTPMonitor(pid)
	m.Config = httpConfig(t, uptime.HTTPConfig{Method: "GET", URL: "https://example.com/health"})
	mustCreateMonitor(t, svc, ctx, m, []string{"local", "eu"})

	n, err := svc.Schedule(ctx)
	if err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	if n != 2 {
		t.Fatalf("Schedule() = %d, want 2", n)
	}

	pending, err := svc.PendingCount(ctx)
	if err != nil {
		t.Fatalf("PendingCount: %v", err)
	}
	if pending != 2 {
		t.Fatalf("PendingCount() = %d, want 2", pending)
	}
}

func TestLeaseLocalOnlyOwnRegionAndRespectsLease(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pid := newProject(t, pool)
	m := baseHTTPMonitor(pid)
	m.Config = httpConfig(t, uptime.HTTPConfig{Method: "GET", URL: "https://example.com/health"})
	created := mustCreateMonitor(t, svc, ctx, m, []string{"local", "eu"})

	if _, err := svc.Schedule(ctx); err != nil {
		t.Fatalf("Schedule: %v", err)
	}

	jobs, err := svc.LeaseLocal(ctx, "local", 10)
	if err != nil {
		t.Fatalf("LeaseLocal: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("LeaseLocal() = %d jobs, want 1 (only region=local)", len(jobs))
	}
	j := jobs[0]
	if j.MonitorID != created.ID || j.Region != "local" {
		t.Fatalf("job = %+v, want monitor %d region local", j, created.ID)
	}
	if j.Monitor.ID != created.ID || j.Monitor.Kind != uptime.KindHTTP {
		t.Fatalf("job.Monitor = %+v, want id=%d kind=http", j.Monitor, created.ID)
	}
	if j.QueueID == 0 {
		t.Fatalf("job.QueueID = 0, want non-zero")
	}

	// Second immediate lease of the same region must return nothing — the
	// job is now leased.
	jobs2, err := svc.LeaseLocal(ctx, "local", 10)
	if err != nil {
		t.Fatalf("LeaseLocal (2nd): %v", err)
	}
	if len(jobs2) != 0 {
		t.Fatalf("LeaseLocal() (2nd) = %d jobs, want 0 (already leased)", len(jobs2))
	}

	// Expire the lease and try again — should be handed out once more.
	if _, err := pool.Exec(ctx, "UPDATE check_queue SET lease_until = now() - interval '1 minute' WHERE id = $1", j.QueueID); err != nil {
		t.Fatalf("expire lease: %v", err)
	}
	jobs3, err := svc.LeaseLocal(ctx, "local", 10)
	if err != nil {
		t.Fatalf("LeaseLocal (after expiry): %v", err)
	}
	if len(jobs3) != 1 {
		t.Fatalf("LeaseLocal() (after expiry) = %d jobs, want 1", len(jobs3))
	}
}

func TestLeaseForProbeSetsLeasedBy(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	orgID := newOrgID(t, pool)
	probe, _, err := svc.CreateProbe(ctx, orgID, "eu-west", "Probe 1")
	if err != nil {
		t.Fatalf("CreateProbe: %v", err)
	}

	pid := newProjectInOrg(t, pool, orgID)
	m := baseHTTPMonitor(pid)
	m.Config = httpConfig(t, uptime.HTTPConfig{Method: "GET", URL: "https://example.com/health"})
	mustCreateMonitor(t, svc, ctx, m, []string{"eu-west"})

	if _, err := svc.Schedule(ctx); err != nil {
		t.Fatalf("Schedule: %v", err)
	}

	jobs, err := svc.LeaseForProbe(ctx, probe, 10)
	if err != nil {
		t.Fatalf("LeaseForProbe: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("LeaseForProbe() = %d jobs, want 1", len(jobs))
	}

	var leasedBy *int64
	if err := pool.QueryRow(ctx, "SELECT leased_by FROM check_queue WHERE id = $1", jobs[0].QueueID).Scan(&leasedBy); err != nil {
		t.Fatalf("select leased_by: %v", err)
	}
	if leasedBy == nil || *leasedBy != probe.ID {
		t.Fatalf("leased_by = %v, want %d", leasedBy, probe.ID)
	}
}

// TestLeaseForProbeIsScopedToProbeOrg — регрессия на межтенантную утечку:
// регион — свободная строка, две независимые организации легко назовут свой
// регион одинаково ("eu-west"). Проба организации A не должна получать задания
// мониторов организации B (в config монитора лежат чужие заголовки/токены, а
// присланный результат гонял бы чужие инциденты).
func TestLeaseForProbeIsScopedToProbeOrg(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	orgA := newOrgID(t, pool)
	orgB := newOrgID(t, pool)

	probeA, _, err := svc.CreateProbe(ctx, orgA, "eu-west", "Probe A")
	if err != nil {
		t.Fatalf("CreateProbe (org A): %v", err)
	}

	mA := baseHTTPMonitor(newProjectInOrg(t, pool, orgA))
	mA.Name = "A health"
	mA.Config = httpConfig(t, uptime.HTTPConfig{Method: "GET", URL: "https://a.example.com/health"})
	createdA := mustCreateMonitor(t, svc, ctx, mA, []string{"eu-west"})

	mB := baseHTTPMonitor(newProjectInOrg(t, pool, orgB))
	mB.Name = "B health"
	mB.Config = httpConfig(t, uptime.HTTPConfig{Method: "GET", URL: "https://b.example.com/health"})
	createdB := mustCreateMonitor(t, svc, ctx, mB, []string{"eu-west"})

	if n, err := svc.Schedule(ctx); err != nil || n != 2 {
		t.Fatalf("Schedule() = %d, %v; want 2, nil", n, err)
	}

	jobs, err := svc.LeaseForProbe(ctx, probeA, 10)
	if err != nil {
		t.Fatalf("LeaseForProbe: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("LeaseForProbe() = %d jobs, want 1 (only org A's job)", len(jobs))
	}
	if jobs[0].MonitorID != createdA.ID {
		t.Fatalf("leased monitor %d, want %d (org A); org B's monitor is %d — cross-tenant leak",
			jobs[0].MonitorID, createdA.ID, createdB.ID)
	}

	// Задание организации B осталось в очереди нетронутым.
	pending, err := svc.PendingCount(ctx)
	if err != nil {
		t.Fatalf("PendingCount: %v", err)
	}
	if pending != 2 {
		t.Fatalf("PendingCount() = %d, want 2", pending)
	}
	var leasedBy *int64
	var leaseUntil *time.Time
	if err := pool.QueryRow(ctx,
		"SELECT leased_by, lease_until FROM check_queue WHERE monitor_id = $1", createdB.ID,
	).Scan(&leasedBy, &leaseUntil); err != nil {
		t.Fatalf("select org B queue row: %v", err)
	}
	if leasedBy != nil || leaseUntil != nil {
		t.Fatalf("org B job leased_by=%v lease_until=%v, want NULL/NULL (must stay queued)", leasedBy, leaseUntil)
	}
}

// TestLeasedJobRejectsJobOfAnotherOrg — вторая линия обороны: даже если строка
// очереди уже числится за пробой (например, монитор/проект переехал в другую
// организацию после выдачи lease), центр не отдаёт пробе чужой монитор.
func TestLeasedJobRejectsJobOfAnotherOrg(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	orgA := newOrgID(t, pool)
	orgB := newOrgID(t, pool)

	probeA, _, err := svc.CreateProbe(ctx, orgA, "eu-west", "Probe A")
	if err != nil {
		t.Fatalf("CreateProbe (org A): %v", err)
	}

	mB := baseHTTPMonitor(newProjectInOrg(t, pool, orgB))
	mB.Config = httpConfig(t, uptime.HTTPConfig{Method: "GET", URL: "https://b.example.com/health"})
	createdB := mustCreateMonitor(t, svc, ctx, mB, []string{"eu-west"})

	if _, err := svc.Schedule(ctx); err != nil {
		t.Fatalf("Schedule: %v", err)
	}

	// Насильно записываем чужую пробу в leased_by (так выглядело бы задание,
	// выданное до смены организации проекта).
	var queueID int64
	if err := pool.QueryRow(ctx, `
		UPDATE check_queue SET leased_by = $1, lease_until = now() + interval '5 minutes'
		WHERE monitor_id = $2 RETURNING id`, probeA.ID, createdB.ID).Scan(&queueID); err != nil {
		t.Fatalf("force lease: %v", err)
	}

	if _, err := svc.LeasedJob(ctx, queueID, probeA.ID); !errors.Is(err, uptime.ErrNotFound) {
		t.Fatalf("LeasedJob for a job of another org: err = %v, want ErrNotFound", err)
	}
}

// TestLeaseLocalIsNotOrgScoped — локальный пробер обслуживает все организации
// сразу (leased_by NULL, регион "local"); org-скоуп проб его не касается.
func TestLeaseLocalIsNotOrgScoped(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	for _, url := range []string{"https://a.example.com/health", "https://b.example.com/health"} {
		m := baseHTTPMonitor(newProject(t, pool)) // newProject заводит свою организацию
		m.Config = httpConfig(t, uptime.HTTPConfig{Method: "GET", URL: url})
		mustCreateMonitor(t, svc, ctx, m, []string{"local"})
	}

	if n, err := svc.Schedule(ctx); err != nil || n != 2 {
		t.Fatalf("Schedule() = %d, %v; want 2, nil", n, err)
	}

	jobs, err := svc.LeaseLocal(ctx, "local", 10)
	if err != nil {
		t.Fatalf("LeaseLocal: %v", err)
	}
	if len(jobs) != 2 {
		t.Fatalf("LeaseLocal() = %d jobs, want 2 (both orgs)", len(jobs))
	}
}

// TestClaimJobIsExactlyOnce — claim снимает задание с очереди ровно один раз:
// повторный claim того же задания (второй одновременный POST /probe/results,
// вторая реплика) не проходит, и его вызывающий не имеет права применять
// результат (см. Ingestor.Accept).
func TestClaimJobIsExactlyOnce(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pid := newProject(t, pool)
	m := baseHTTPMonitor(pid)
	m.Config = httpConfig(t, uptime.HTTPConfig{Method: "GET", URL: "https://example.com/health"})
	mustCreateMonitor(t, svc, ctx, m, []string{"local"})

	if _, err := svc.Schedule(ctx); err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	jobs, err := svc.LeaseLocal(ctx, "local", 10)
	if err != nil {
		t.Fatalf("LeaseLocal: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("LeaseLocal() = %d jobs, want 1", len(jobs))
	}
	job := jobs[0]
	if job.LeaseUntil.IsZero() {
		t.Fatal("LeaseLocal() returned a job with a zero LeaseUntil — the claim token must be set")
	}

	claimed, err := svc.ClaimJob(ctx, job.QueueID, job.LeaseUntil)
	if err != nil {
		t.Fatalf("ClaimJob: %v", err)
	}
	if !claimed {
		t.Fatal("ClaimJob() = false, want true (fresh lease)")
	}

	pending, err := svc.PendingCount(ctx)
	if err != nil {
		t.Fatalf("PendingCount: %v", err)
	}
	if pending != 0 {
		t.Fatalf("PendingCount() = %d, want 0 (claim removes the job)", pending)
	}

	claimed, err = svc.ClaimJob(ctx, job.QueueID, job.LeaseUntil)
	if err != nil {
		t.Fatalf("ClaimJob (2nd): %v", err)
	}
	if claimed {
		t.Fatal("ClaimJob() (2nd, same job) = true, want false — a job must be claimable exactly once")
	}
}

// TestClaimJobRejectsStaleLease — задание, чей lease истёк и которое успела
// перелизить другая реплика, старому держателю уже не принадлежит: его claim с
// прежним lease_until не проходит, задание остаётся в очереди за новым
// держателем.
func TestClaimJobRejectsStaleLease(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pid := newProject(t, pool)
	m := baseHTTPMonitor(pid)
	m.Config = httpConfig(t, uptime.HTTPConfig{Method: "GET", URL: "https://example.com/health"})
	mustCreateMonitor(t, svc, ctx, m, []string{"local"})

	if _, err := svc.Schedule(ctx); err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	first, err := svc.LeaseLocal(ctx, "local", 10)
	if err != nil || len(first) != 1 {
		t.Fatalf("LeaseLocal() = %d jobs, %v; want 1, nil", len(first), err)
	}

	// Lease первого держателя протух — задание берёт вторая реплика.
	if _, err := pool.Exec(ctx,
		"UPDATE check_queue SET lease_until = now() - interval '1 minute' WHERE id = $1", first[0].QueueID); err != nil {
		t.Fatalf("expire lease: %v", err)
	}
	second, err := svc.LeaseLocal(ctx, "local", 10)
	if err != nil || len(second) != 1 {
		t.Fatalf("LeaseLocal (re-lease) = %d jobs, %v; want 1, nil", len(second), err)
	}
	if second[0].QueueID != first[0].QueueID {
		t.Fatalf("re-leased queue id = %d, want %d", second[0].QueueID, first[0].QueueID)
	}

	// Первый держатель наконец досчитал свою проверку — но она уже не его.
	claimed, err := svc.ClaimJob(ctx, first[0].QueueID, first[0].LeaseUntil)
	if err != nil {
		t.Fatalf("ClaimJob (stale holder): %v", err)
	}
	if claimed {
		t.Fatal("ClaimJob() with a stale lease = true, want false — the job was re-leased to somebody else")
	}
	pending, err := svc.PendingCount(ctx)
	if err != nil {
		t.Fatalf("PendingCount: %v", err)
	}
	if pending != 1 {
		t.Fatalf("PendingCount() = %d, want 1 (the job still belongs to the new holder)", pending)
	}

	// А новый держатель забирает его штатно.
	claimed, err = svc.ClaimJob(ctx, second[0].QueueID, second[0].LeaseUntil)
	if err != nil {
		t.Fatalf("ClaimJob (new holder): %v", err)
	}
	if !claimed {
		t.Fatal("ClaimJob() by the current lease holder = false, want true")
	}
}

// TestLeasedJobRejectsRevokedProbe — отозванная проба не может подтвердить
// даже собственное, ещё не протухшее задание (clause revoked_at IS NULL в
// LeasedJob). По HTTP этот путь недостижим — probeAuth отвечает 401 раньше, —
// поэтому проверяется напрямую: иначе clause можно было бы удалить, и ни один
// тест бы не покраснел.
func TestLeasedJobRejectsRevokedProbe(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	orgID := newOrgID(t, pool)
	probe, _, err := svc.CreateProbe(ctx, orgID, "eu-west", "Probe 1")
	if err != nil {
		t.Fatalf("CreateProbe: %v", err)
	}

	pid := newProjectInOrg(t, pool, orgID)
	m := baseHTTPMonitor(pid)
	m.Config = httpConfig(t, uptime.HTTPConfig{Method: "GET", URL: "https://example.com/health"})
	mustCreateMonitor(t, svc, ctx, m, []string{"eu-west"})

	if _, err := svc.Schedule(ctx); err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	jobs, err := svc.LeaseForProbe(ctx, probe, 10)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("LeaseForProbe() = %d jobs, %v; want 1, nil", len(jobs), err)
	}
	queueID := jobs[0].QueueID

	// Пока lease жив, задание своё.
	if _, err := svc.LeasedJob(ctx, queueID, probe.ID); err != nil {
		t.Fatalf("LeasedJob before revoke: %v", err)
	}

	if err := svc.RevokeProbe(ctx, probe.ID); err != nil {
		t.Fatalf("RevokeProbe: %v", err)
	}
	if _, err := svc.LeasedJob(ctx, queueID, probe.ID); !errors.Is(err, uptime.ErrNotFound) {
		t.Fatalf("LeasedJob after revoke: err = %v, want ErrNotFound", err)
	}
}
