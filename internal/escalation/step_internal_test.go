package escalation

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// Продублировано из escalation_test: этот файл — package escalation, для доступа
// к неэкспортированным maxLogFailureAttempts/recordLogFailure/clearLogFailure.
func newLogFailureProject(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	ctx := context.Background()
	var orgID, projectID int64
	if err := pool.QueryRow(ctx, "INSERT INTO organizations (slug, name) VALUES ($1,$2) RETURNING id",
		"esc-lf-"+t.Name(), "Esc LF").Scan(&orgID); err != nil {
		t.Fatalf("insert org: %v", err)
	}
	if err := pool.QueryRow(ctx, "INSERT INTO projects (org_id, slug, name) VALUES ($1,$2,$3) RETURNING id",
		orgID, "esc-lf-"+t.Name(), "Esc LF").Scan(&projectID); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	return projectID
}

func newLogFailureChannel(t *testing.T, pool *pgxpool.Pool, projectID int64) int64 {
	t.Helper()
	var chID int64
	if err := pool.QueryRow(context.Background(), `
		INSERT INTO alert_channels (project_id, kind, target, enabled)
		VALUES ($1, 'webhook', 'https://example.com/hook', true) RETURNING id`, projectID).Scan(&chID); err != nil {
		t.Fatalf("insert channel: %v", err)
	}
	return chID
}

func TestSendStepIfDueClaimFailureNeverForcesBump(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	pid := newLogFailureProject(t, pool)
	c1 := newLogFailureChannel(t, pool, pid)
	const incidentID = int64(9600)
	const source = "metric"

	// INSERT в incident_escalations обязан провалиться детерминированно.
	if _, err := pool.Exec(ctx, "ALTER TABLE incident_escalations ADD CONSTRAINT test_force_log_fail CHECK (false)"); err != nil {
		t.Fatalf("add forcing constraint: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "ALTER TABLE incident_escalations DROP CONSTRAINT IF EXISTS test_force_log_fail")
	})

	ladder := Ladder{{StepNo: 0, DelayMinutes: 0, ChannelIDs: []int64{c1}}}
	var notifyCalled, bumpCalled bool
	notifyStep := func(chs []int64, step int) ([]int64, error) { notifyCalled = true; return chs, nil }
	bump := func(id int64, from int) (bool, error) { bumpCalled = true; return true, nil }

	// Claim-путь не продавливает прогресс никогда, пока PG (constraint) не отпустит.
	for i := 0; i < maxLogFailureAttempts+2; i++ {
		notifyCalled, bumpCalled = false, false
		sent, err := SendStepIfDue(ctx, ladder, source, pool, incidentID, 0, 0, notifyStep, bump)
		if err == nil {
			t.Fatalf("вызов #%d: SendStepIfDue err = nil, want ошибку claim", i)
		}
		if sent {
			t.Errorf("вызов #%d: sent = true, want false — claim не проходит, форсировать прогресс нечем", i)
		}
		if notifyCalled {
			t.Errorf("вызов #%d: notifyStep вызван — не должен: claim падает раньше", i)
		}
		if bumpCalled {
			t.Errorf("вызов #%d: bump вызван — claim-путь не продавливает прогресс принудительно", i)
		}
	}

	// Лог ступени пуст: ни одна попытка claim не закоммитилась (constraint
	// откатывает INSERT целиком).
	var count int
	if err := pool.QueryRow(context.Background(),
		"SELECT count(*) FROM incident_escalations WHERE incident_source=$1 AND incident_id=$2 AND step=0",
		source, incidentID).Scan(&count); err != nil {
		t.Fatalf("select incident_escalations: %v", err)
	}
	if count != 0 {
		t.Errorf("incident_escalations rows = %d, want 0 (claim ни разу не прошёл)", count)
	}
}

func TestRecordLogFailureIncrementsAndClearResets(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	const incidentID = int64(9601)
	const source = "trace"

	for want := 1; want <= 3; want++ {
		got, err := recordLogFailure(ctx, pool, source, incidentID, 2)
		if err != nil {
			t.Fatalf("recordLogFailure #%d: %v", want, err)
		}
		if got != want {
			t.Fatalf("recordLogFailure #%d = %d, want %d", want, got, want)
		}
	}

	if err := clearLogFailure(ctx, pool, source, incidentID, 2); err != nil {
		t.Fatalf("clearLogFailure: %v", err)
	}

	got, err := recordLogFailure(ctx, pool, source, incidentID, 2)
	if err != nil {
		t.Fatalf("recordLogFailure after clear: %v", err)
	}
	if got != 1 {
		t.Fatalf("recordLogFailure after clear = %d, want 1 (reset, not continuing from 3)", got)
	}

	// step отдельный — не должен делить счётчик с step=2 выше.
	got, err = recordLogFailure(ctx, pool, source, incidentID, 7)
	if err != nil {
		t.Fatalf("recordLogFailure (different step): %v", err)
	}
	if got != 1 {
		t.Fatalf("recordLogFailure (different step) = %d, want 1 (independent counter)", got)
	}
}
