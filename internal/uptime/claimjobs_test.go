package uptime_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
)

// TestClaimJobsConcurrentOverlapExactlyOnce: пакетный claim (один DELETE …
// RETURNING на пачку) под конкуренцией даёт то же, что построчный ClaimJob —
// каждое задание изымается ровно один раз. Две «пробы» одновременно клеймят
// пересекающиеся наборы: объединение изъятых — все задания, пересечение —
// пусто; повторный claim — пусто. Несколько раундов, чтобы поймать гонку, а
// не одно удачное чередование.
func TestClaimJobsConcurrentOverlapExactlyOnce(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pid := newProject(t, pool)
	const n = 6
	for i := 0; i < n; i++ {
		m := baseHTTPMonitor(pid)
		m.Config = httpConfig(t, uptime.HTTPConfig{Method: "GET", URL: "https://example.com/health"})
		mustCreateMonitor(t, pool, svc, ctx, m, []string{"local"})
	}

	const rounds = 5
	for round := 0; round < rounds; round++ {
		if round > 0 {
			// Вернуть задания в очередь: планировщик ставит монитор снова
			// только по сроку, так что срок сбрасываем; аренда будет новой.
			if _, err := pool.Exec(ctx, "UPDATE monitors SET last_scheduled_at = NULL"); err != nil {
				t.Fatalf("reset schedule: %v", err)
			}
		}
		if _, err := svc.Schedule(ctx); err != nil {
			t.Fatalf("round %d: Schedule: %v", round, err)
		}
		jobs, err := svc.LeaseLocal(ctx, "local", n)
		if err != nil || len(jobs) != n {
			t.Fatalf("round %d: LeaseLocal = %d jobs, err %v; want %d", round, len(jobs), err, n)
		}
		claims := make([]uptime.JobClaim, n)
		for i, j := range jobs {
			claims[i] = uptime.JobClaim{QueueID: j.QueueID, LeaseUntil: j.LeaseUntil}
		}
		// Наборы A = [0..4), B = [2..6): пересечение — задания 2 и 3.
		sets := [][]uptime.JobClaim{claims[:4], claims[2:]}

		results := make([]map[int64]bool, len(sets))
		errs := make([]error, len(sets))
		var start, done sync.WaitGroup
		start.Add(1)
		for i, set := range sets {
			done.Add(1)
			go func(i int, set []uptime.JobClaim) {
				defer done.Done()
				start.Wait()
				results[i], errs[i] = svc.ClaimJobs(ctx, set)
			}(i, set)
		}
		start.Done()
		done.Wait()
		for i, err := range errs {
			if err != nil {
				t.Fatalf("round %d: ClaimJobs #%d: %v", round, i, err)
			}
		}

		seen := map[int64]int{}
		for _, res := range results {
			for id, ok := range res {
				if ok {
					seen[id]++
				}
			}
		}
		for _, c := range claims {
			if got := seen[c.QueueID]; got != 1 {
				t.Fatalf("round %d: queue_id %d claimed %d times — must be exactly once", round, c.QueueID, got)
			}
		}
		if len(seen) != n {
			t.Fatalf("round %d: claimed %d distinct jobs, want %d", round, len(seen), n)
		}

		again, err := svc.ClaimJobs(ctx, claims)
		if err != nil {
			t.Fatalf("round %d: ClaimJobs (again): %v", round, err)
		}
		if len(again) != 0 {
			t.Fatalf("round %d: re-claim returned %v, want nothing (jobs already claimed)", round, again)
		}
		pending, err := svc.PendingCount(ctx)
		if err != nil {
			t.Fatalf("PendingCount: %v", err)
		}
		if pending != 0 {
			t.Fatalf("round %d: PendingCount() = %d, want 0", round, pending)
		}
	}
}

// TestClaimJobsStaleLeaseNotClaimed: как ClaimJob — lease_until, не совпавший
// с текущим (задание перевыдано), не даёт изъять задание; остальные строки
// пачки при этом клеймятся.
func TestClaimJobsStaleLeaseNotClaimed(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pid := newProject(t, pool)
	for i := 0; i < 2; i++ {
		m := baseHTTPMonitor(pid)
		m.Config = httpConfig(t, uptime.HTTPConfig{Method: "GET", URL: "https://example.com/health"})
		mustCreateMonitor(t, pool, svc, ctx, m, []string{"local"})
	}
	if _, err := svc.Schedule(ctx); err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	jobs, err := svc.LeaseLocal(ctx, "local", 2)
	if err != nil || len(jobs) != 2 {
		t.Fatalf("LeaseLocal = %d jobs, err %v; want 2", len(jobs), err)
	}
	claimed, err := svc.ClaimJobs(ctx, []uptime.JobClaim{
		{QueueID: jobs[0].QueueID, LeaseUntil: jobs[0].LeaseUntil.Add(-time.Second)},
		{QueueID: jobs[1].QueueID, LeaseUntil: jobs[1].LeaseUntil},
	})
	if err != nil {
		t.Fatalf("ClaimJobs: %v", err)
	}
	if claimed[jobs[0].QueueID] {
		t.Fatalf("job with stale lease_until was claimed: %v", claimed)
	}
	if !claimed[jobs[1].QueueID] {
		t.Fatalf("job with current lease_until was not claimed: %v", claimed)
	}
	pending, err := svc.PendingCount(ctx)
	if err != nil {
		t.Fatalf("PendingCount: %v", err)
	}
	if pending != 1 {
		t.Fatalf("PendingCount() = %d, want 1 (stale row stays queued)", pending)
	}
}
