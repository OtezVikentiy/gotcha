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

// TestSendStepIfDueClaimFailureNeverForcesBump — АДАПТИРОВАН под
// claim-before-notify (аудит перед 1.0, K1-1): до этой правки SendStepIfDue
// логировал ПОСЛЕ notifyStep, и устойчиво падающий LogStep грозил
// пейджинг-штормом (bump заблокирован → тот же шаг на следующем тике →
// notifyStep пейджит снова → LogStep падает снова, каждый тик) —
// maxLogFailureAttempts продавливал bump принудительно после N провалов,
// проверялось это здесь. Теперь ЛОГ И ЕСТЬ CLAIM (ClaimStepChannels), и он
// стоит ДО notifyStep — устойчивый провал claim (тот же forcing-constraint,
// что и раньше) означает, что notifyStep вообще ни разу не вызывается, и
// пейджинг-шторма, от которого защищал потолок, физически быть не может:
// продавливать прогресс уже нечем и незачем (см. докблок SendStepIfDue,
// случай 4). Тест теперь фиксирует обратное: claim падает на КАЖДОМ вызове
// без ограничения попыток, sent всегда false, bump никогда не зовётся —
// maxLogFailureAttempts/escalation_step_log_failures в эту функцию больше
// не участвуют (остаются только для LogStepChannels — uptime-шаг 0).
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

	// ClaimStepChannels — тот же INSERT в incident_escalations, что раньше
	// делал LogStep — обязан провалиться детерминированно.
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

	// Несколько подряд вызовов — больше maxLogFailureAttempts: старый
	// потолок попыток здесь бы уже продавил bump, новый claim-путь не
	// продавливает НИКОГДА, пока PG (constraint) не отпустит.
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
