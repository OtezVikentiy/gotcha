package escalation

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// newLogFailureProject/newLogFailureChannel — минимальные raw-SQL сиды,
// продублированные из escalation_test (package escalation_test, недоступен
// отсюда — этот файл package escalation, ради доступа к неэкспортированным
// maxLogFailureAttempts/recordLogFailure/clearLogFailure).
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

// TestSendStepIfDueForcesBumpAfterMaxLogFailures — W2-C находка 3, условие 2
// ревью: устойчиво падающий LogStep не должен клинить ступень в бесконечный
// пейджинг-шторм (bump заблокирован → тот же шаг на следующем тике →
// notifyStep пейджит снова → LogStep падает снова, каждый тик). После
// maxLogFailureAttempts подряд неудачных попыток bump обязан продавиться
// принудительно, а счётчик — сброситься.
//
// LogStep форсированно ломается constraint CHECK (false) на
// incident_escalations — не отзывом привилегий (роль тестового контейнера
// суперпользователь, REVOKE на неё не действует), а constraint'ом, который
// не обходит даже суперпользователь. escalation_step_log_failures — другая
// таблица, ничем не тронута, и её запись продолжает работать как обычно —
// это и позволяет счётчику попыток дойти до границы, пока LogStep
// стабильно падает.
func TestSendStepIfDueForcesBumpAfterMaxLogFailures(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	pid := newLogFailureProject(t, pool)
	c1 := newLogFailureChannel(t, pool, pid)
	const incidentID = int64(9600)
	const source = "metric"

	// Предзаполняем счётчик до maxLogFailureAttempts-1: следующий (единственный
	// в тесте) вызов SendStepIfDue доводит его РОВНО до границы.
	var attempts int
	for i := 0; i < maxLogFailureAttempts-1; i++ {
		var err error
		attempts, err = recordLogFailure(ctx, pool, source, incidentID, 0)
		if err != nil {
			t.Fatalf("recordLogFailure seed #%d: %v", i, err)
		}
	}
	if attempts != maxLogFailureAttempts-1 {
		t.Fatalf("seeded attempts = %d, want %d", attempts, maxLogFailureAttempts-1)
	}

	// LogStep обязан провалиться детерминированно на ЭТОМ вызове.
	if _, err := pool.Exec(ctx, "ALTER TABLE incident_escalations ADD CONSTRAINT test_force_log_fail CHECK (false)"); err != nil {
		t.Fatalf("add forcing constraint: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "ALTER TABLE incident_escalations DROP CONSTRAINT IF EXISTS test_force_log_fail")
	})

	ladder := Ladder{{StepNo: 0, DelayMinutes: 0, ChannelIDs: []int64{c1}}}
	var bumpCalled bool
	var bumpFrom int
	sent, err := SendStepIfDue(ctx, ladder, source, pool, incidentID, 0, 0,
		func(chs []int64, step int) ([]int64, error) { return chs, nil }, // notifyStep "успешен"
		func(id int64, from int) (bool, error) {
			bumpCalled = true
			bumpFrom = from
			return true, nil
		})
	if err == nil {
		t.Fatal("SendStepIfDue err = nil, want ошибку LogStep (даже при принудительном bump — провал реален и не должен маскироваться)")
	}
	if !sent {
		t.Error("sent = false, want true: bump обязан продавиться принудительно на границе попыток")
	}
	if !bumpCalled {
		t.Fatal("bump не вызван — находка storm-prevention: граница попыток должна была продавить прогресс")
	}
	if bumpFrom != 0 {
		t.Errorf("bump(from=%d), want 0", bumpFrom)
	}

	// Счётчик сброшен после принудительного bump — следующая ступень того
	// же инцидента начинает с чистого листа, а не с уже исчерпанной границы.
	var remaining int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM escalation_step_log_failures WHERE incident_source=$1 AND incident_id=$2 AND step=0",
		source, incidentID).Scan(&remaining); err != nil {
		t.Fatalf("select escalation_step_log_failures: %v", err)
	}
	if remaining != 0 {
		t.Errorf("escalation_step_log_failures rows after forced bump = %d, want 0 (cleared)", remaining)
	}
}

// TestRecordLogFailureIncrementsAndClearResets проверяет саму механику
// счётчика в изоляции от SendStepIfDue: последовательные вызовы
// recordLogFailure на одну и ту же (source, incident, step) увеличивают
// attempts на 1 каждый раз; clearLogFailure сбрасывает его — следующий
// recordLogFailure снова возвращает 1, не продолжает с прежнего значения.
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
