package escalation_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/escalation"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

func TestSendStepIfDueLogsEnqueuedChannels(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	pid := newProject(t, pool)
	c1 := newChannel(t, pool, pid, true)
	c2 := newChannel(t, pool, pid, true)

	ladder := escalation.Ladder{{StepNo: 0, DelayMinutes: 0, ChannelIDs: []int64{c1, c2}}}
	const incidentID = int64(4242)

	var notifyStepCalls int
	var bumpCalls int
	sent, err := escalation.SendStepIfDue(ctx, ladder, "metric", pool, incidentID, 0, 0,
		func(chs []int64, step int) ([]int64, error) {
			notifyStepCalls++
			if step != 0 {
				t.Errorf("notifyStep step = %d, want 0", step)
			}
			return chs, nil
		},
		func(id int64, from int) (bool, error) {
			bumpCalls++
			if id != incidentID || from != 0 {
				t.Errorf("bump(id=%d, from=%d), want (%d, 0)", id, from, incidentID)
			}
			return true, nil
		})
	if err != nil {
		t.Fatalf("SendStepIfDue: %v", err)
	}
	if !sent {
		t.Fatal("sent = false, want true (delay0=0, notifyStep и bump успешны)")
	}
	if notifyStepCalls != 1 || bumpCalls != 1 {
		t.Fatalf("notifyStepCalls=%d bumpCalls=%d, want 1/1", notifyStepCalls, bumpCalls)
	}

	for _, ch := range []int64{c1, c2} {
		var count int
		if err := pool.QueryRow(ctx,
			"SELECT count(*) FROM incident_escalations WHERE incident_source='metric' AND incident_id=$1 AND channel_id=$2 AND step=0",
			incidentID, ch).Scan(&count); err != nil {
			t.Fatalf("select escalation log channel %d: %v", ch, err)
		}
		if count != 1 {
			t.Errorf("incident_escalations rows for channel %d = %d, want 1", ch, count)
		}
	}
}

func TestSendStepIfDueSkipsWhenDelayNotDue(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	pid := newProject(t, pool)
	c1 := newChannel(t, pool, pid, true)

	ladder := escalation.Ladder{{StepNo: 0, DelayMinutes: 15, ChannelIDs: []int64{c1}}}
	const incidentID = int64(4343)

	called := false
	sent, err := escalation.SendStepIfDue(ctx, ladder, "metric", pool, incidentID, 0, 0,
		func(chs []int64, step int) ([]int64, error) { called = true; return chs, nil },
		func(id int64, from int) (bool, error) { called = true; return true, nil })
	if err != nil {
		t.Fatalf("SendStepIfDue: %v", err)
	}
	if sent {
		t.Error("sent = true, want false (delay ещё не настал)")
	}
	if called {
		t.Error("notifyStep/bump вызваны, want ни одного (delay ещё не настал)")
	}

	var count int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM incident_escalations WHERE incident_source='metric' AND incident_id=$1",
		incidentID).Scan(&count); err != nil {
		t.Fatalf("select escalation log: %v", err)
	}
	if count != 0 {
		t.Errorf("incident_escalations rows = %d, want 0", count)
	}
}

func TestSendStepIfDueLevelBeyondLadder(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	ctx := context.Background()

	ladder := escalation.Ladder{{StepNo: 0, DelayMinutes: 0, ChannelIDs: []int64{1}}}
	sent, err := escalation.SendStepIfDue(ctx, ladder, "metric", pool, 4444, 1, time.Hour,
		func(chs []int64, step int) ([]int64, error) {
			t.Fatal("notifyStep не должен звать: level за пределами лесенки")
			return nil, nil
		},
		func(id int64, from int) (bool, error) {
			t.Fatal("bump не должен звать: level за пределами лесенки")
			return false, nil
		})
	if err != nil {
		t.Fatalf("SendStepIfDue: %v", err)
	}
	if sent {
		t.Error("sent = true, want false (лесенка исчерпана)")
	}
}

func TestSendStepIfDueNotifyStepTotalFailureSkipsLogAndBump(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	pid := newProject(t, pool)
	c1 := newChannel(t, pool, pid, true)

	ladder := escalation.Ladder{{StepNo: 0, DelayMinutes: 0, ChannelIDs: []int64{c1}}}
	const incidentID = int64(4545)
	wantErr := errors.New("enqueue failed")

	bumpCalled := false
	sent, err := escalation.SendStepIfDue(ctx, ladder, "metric", pool, incidentID, 0, 0,
		func(chs []int64, step int) ([]int64, error) { return nil, wantErr },
		func(id int64, from int) (bool, error) { bumpCalled = true; return true, nil })
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
	if sent {
		t.Error("sent = true, want false (notifyStep провалился тотально)")
	}
	if bumpCalled {
		t.Error("bump вызван при тотальном провале notifyStep — не должен")
	}

	var count int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM incident_escalations WHERE incident_source='metric' AND incident_id=$1",
		incidentID).Scan(&count); err != nil {
		t.Fatalf("select escalation log: %v", err)
	}
	if count != 0 {
		t.Errorf("incident_escalations rows = %d, want 0 (notifyStep провалился тотально)", count)
	}
}

func TestSendStepIfDueNotifyStepPartialFailureLogsAndBumps(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	pid := newProject(t, pool)
	c1 := newChannel(t, pool, pid, true)
	c2 := newChannel(t, pool, pid, true)

	ladder := escalation.Ladder{{StepNo: 0, DelayMinutes: 0, ChannelIDs: []int64{c1, c2}}}
	const incidentID = int64(4546)
	wantErr := errors.New("channel c2 enqueue failed")

	bumpCalled := false
	sent, err := escalation.SendStepIfDue(ctx, ladder, "metric", pool, incidentID, 0, 0,
		func(chs []int64, step int) ([]int64, error) { return []int64{c1}, wantErr },
		func(id int64, from int) (bool, error) {
			bumpCalled = true
			if id != incidentID || from != 0 {
				t.Errorf("bump(id=%d, from=%d), want (%d, 0)", id, from, incidentID)
			}
			return true, nil
		})
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want wrapped %v", err, wantErr)
	}
	if !sent {
		t.Error("sent = false, want true (хотя бы один канал заенкенился, bump применился)")
	}
	if !bumpCalled {
		t.Error("bump не вызван при частичном провале — должен, иначе лесенка клинит на плохом канале")
	}

	var count int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM incident_escalations WHERE incident_source='metric' AND incident_id=$1 AND channel_id=$2 AND step=0",
		incidentID, c1).Scan(&count); err != nil {
		t.Fatalf("select escalation log: %v", err)
	}
	if count != 1 {
		t.Errorf("incident_escalations rows for c1 = %d, want 1 (реально заенкенился, должен быть залогирован)", count)
	}

	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM incident_escalations WHERE incident_source='metric' AND incident_id=$1 AND channel_id=$2 AND step=0",
		incidentID, c2).Scan(&count); err != nil {
		t.Fatalf("select escalation log: %v", err)
	}
	if count != 0 {
		t.Errorf("incident_escalations rows for c2 = %d, want 0 (не заенкенился — не должен быть залогирован)", count)
	}
}

func TestSendStepIfDueClaimFailureBlocksNotifyAndBump(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	pid := newProject(t, pool)
	c1 := newChannel(t, pool, pid, true)

	ladder := escalation.Ladder{{StepNo: 0, DelayMinutes: 0, ChannelIDs: []int64{c1}}}
	const incidentID = int64(9001)

	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel()

	var notifyCalled, bumpCalled bool
	sent, err := escalation.SendStepIfDue(cancelledCtx, ladder, "metric", pool, incidentID, 0, 0,
		func(chs []int64, step int) ([]int64, error) {
			notifyCalled = true
			return chs, nil
		},
		func(id int64, from int) (bool, error) {
			bumpCalled = true
			return true, nil
		})
	if err == nil {
		t.Fatal("SendStepIfDue err = nil, want ошибку claim (отменённый ctx)")
	}
	if sent {
		t.Error("sent = true, want false: claim не прошёл, bump не должен применяться")
	}
	if notifyCalled {
		t.Error("notifyStep вызван, несмотря на провал claim — claim обязан стоять раньше отправки")
	}
	if bumpCalled {
		t.Error("bump вызван, несмотря на провал claim")
	}

	var count int
	if err := pool.QueryRow(context.Background(),
		"SELECT count(*) FROM incident_escalations WHERE incident_source='metric' AND incident_id=$1 AND channel_id=$2 AND step=0",
		incidentID, c1).Scan(&count); err != nil {
		t.Fatalf("select escalation log: %v", err)
	}
	if count != 0 {
		t.Errorf("incident_escalations rows = %d, want 0: LogStep должен был провалиться", count)
	}
}

func TestLogStepIdempotentOnRetry(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	pid := newProject(t, pool)
	c1 := newChannel(t, pool, pid, true)
	const incidentID = int64(9002)

	if err := escalation.LogStep(ctx, pool, "metric", incidentID, c1, 0); err != nil {
		t.Fatalf("LogStep (1st): %v", err)
	}
	if err := escalation.LogStep(ctx, pool, "metric", incidentID, c1, 0); err != nil {
		t.Fatalf("LogStep (2nd, retry) = %v, want nil (idempotent)", err)
	}

	var count int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM incident_escalations WHERE incident_source='metric' AND incident_id=$1 AND channel_id=$2 AND step=0",
		incidentID, c1).Scan(&count); err != nil {
		t.Fatalf("select escalation log: %v", err)
	}
	if count != 1 {
		t.Errorf("incident_escalations rows = %d, want 1 (повтор — no-op, не дубль)", count)
	}
}

func TestSendStepIfDueSkipsNotifyWhenStepAlreadyClaimed(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	pid := newProject(t, pool)
	c1 := newChannel(t, pool, pid, true)
	c2 := newChannel(t, pool, pid, true)
	const incidentID = int64(9010)

	// Ступень 0 уже занята за оба канала — LogStep делает тот же INSERT, что и ClaimStepChannels.
	if err := escalation.LogStep(ctx, pool, "metric", incidentID, c1, 0); err != nil {
		t.Fatalf("LogStep c1: %v", err)
	}
	if err := escalation.LogStep(ctx, pool, "metric", incidentID, c2, 0); err != nil {
		t.Fatalf("LogStep c2: %v", err)
	}

	ladder := escalation.Ladder{{StepNo: 0, DelayMinutes: 0, ChannelIDs: []int64{c1, c2}}}
	var notifyCalls int
	var bumpFrom int
	bumpCalled := false
	sent, err := escalation.SendStepIfDue(ctx, ladder, "metric", pool, incidentID, 0, 0,
		func(chs []int64, step int) ([]int64, error) { notifyCalls++; return chs, nil },
		func(id int64, from int) (bool, error) { bumpCalled = true; bumpFrom = from; return true, nil })
	if err != nil {
		t.Fatalf("SendStepIfDue: %v", err)
	}
	if !sent {
		t.Error("sent = false, want true (ступень занята, но bump применяется)")
	}
	if notifyCalls != 0 {
		t.Errorf("notifyStep вызван %d раз, want 0 (ступень уже целиком занята)", notifyCalls)
	}
	if !bumpCalled {
		t.Fatal("bump не вызван — занятая ступень всё равно должна продвигать уровень")
	}
	if bumpFrom != 0 {
		t.Errorf("bump(from=%d), want 0", bumpFrom)
	}
}

func TestSendStepIfDueReleasesClaimOnTotalNotifyFailure(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	pid := newProject(t, pool)
	c1 := newChannel(t, pool, pid, true)
	const incidentID = int64(9011)
	errBoom := errors.New("outbox down")

	ladder := escalation.Ladder{{StepNo: 0, DelayMinutes: 0, ChannelIDs: []int64{c1}}}
	bumpCalled := false
	sent, err := escalation.SendStepIfDue(ctx, ladder, "metric", pool, incidentID, 0, 0,
		func(chs []int64, step int) ([]int64, error) { return nil, errBoom },
		func(id int64, from int) (bool, error) { bumpCalled = true; return true, nil })
	if !errors.Is(err, errBoom) {
		t.Fatalf("err = %v, want %v", err, errBoom)
	}
	if sent {
		t.Error("sent = true, want false (тотальный провал notifyStep)")
	}
	if bumpCalled {
		t.Error("bump вызван при тотальном провале notifyStep — не должен")
	}

	var count int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM incident_escalations WHERE incident_source='metric' AND incident_id=$1 AND step=0",
		incidentID).Scan(&count); err != nil {
		t.Fatalf("select escalation log: %v", err)
	}
	if count != 0 {
		t.Errorf("incident_escalations rows = %d, want 0 (claim освобождён после тотального провала)", count)
	}

	// Ретрай: claim свободен, notifyStep зовётся заново.
	var notifyCalls int
	sent, err = escalation.SendStepIfDue(ctx, ladder, "metric", pool, incidentID, 0, 0,
		func(chs []int64, step int) ([]int64, error) { notifyCalls++; return chs, nil },
		func(id int64, from int) (bool, error) { bumpCalled = true; return true, nil })
	if err != nil {
		t.Fatalf("SendStepIfDue (retry): %v", err)
	}
	if !sent {
		t.Error("sent = false после ретрая, want true")
	}
	if notifyCalls != 1 {
		t.Errorf("notifyStep вызван %d раз на ретрае, want 1 (claim был освобождён)", notifyCalls)
	}
}

func TestSendStepIfDueReleasesUnenqueuedChannels(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	pid := newProject(t, pool)
	c1 := newChannel(t, pool, pid, true)
	c2 := newChannel(t, pool, pid, true)
	const incidentID = int64(9012)

	ladder := escalation.Ladder{{StepNo: 0, DelayMinutes: 0, ChannelIDs: []int64{c1, c2}}}
	bumpCalled := false
	sent, err := escalation.SendStepIfDue(ctx, ladder, "metric", pool, incidentID, 0, 0,
		func(chs []int64, step int) ([]int64, error) { return []int64{c1}, nil },
		func(id int64, from int) (bool, error) { bumpCalled = true; return true, nil })
	if err != nil {
		t.Fatalf("SendStepIfDue: %v", err)
	}
	if !sent {
		t.Error("sent = false, want true")
	}
	if !bumpCalled {
		t.Error("bump не вызван — частичный провал не должен блокировать прогресс")
	}

	var countC1, countC2 int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM incident_escalations WHERE incident_source='metric' AND incident_id=$1 AND channel_id=$2 AND step=0",
		incidentID, c1).Scan(&countC1); err != nil {
		t.Fatalf("select escalation log c1: %v", err)
	}
	if countC1 != 1 {
		t.Errorf("incident_escalations rows for c1 = %d, want 1 (реально заенкенился)", countC1)
	}
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM incident_escalations WHERE incident_source='metric' AND incident_id=$1 AND channel_id=$2 AND step=0",
		incidentID, c2).Scan(&countC2); err != nil {
		t.Fatalf("select escalation log c2: %v", err)
	}
	if countC2 != 0 {
		t.Errorf("incident_escalations rows for c2 = %d, want 0 (claim освобождён — не заенкенился)", countC2)
	}
}

func TestSendStepIfDueNotifiesOnlyWonChannels(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	pid := newProject(t, pool)
	c1 := newChannel(t, pool, pid, true)
	c2 := newChannel(t, pool, pid, true)
	const incidentID = int64(9013)

	if err := escalation.LogStep(ctx, pool, "metric", incidentID, c1, 0); err != nil {
		t.Fatalf("LogStep c1 (pre-claim): %v", err)
	}

	ladder := escalation.Ladder{{StepNo: 0, DelayMinutes: 0, ChannelIDs: []int64{c1, c2}}}
	var gotChs []int64
	sent, err := escalation.SendStepIfDue(ctx, ladder, "metric", pool, incidentID, 0, 0,
		func(chs []int64, step int) ([]int64, error) { gotChs = chs; return chs, nil },
		func(id int64, from int) (bool, error) { return true, nil })
	if err != nil {
		t.Fatalf("SendStepIfDue: %v", err)
	}
	if !sent {
		t.Error("sent = false, want true")
	}
	if len(gotChs) != 1 || gotChs[0] != c2 {
		t.Fatalf("notifyStep получил %v, want [%d] (c1 уже занят)", gotChs, c2)
	}
}

func TestSendStepIfDueBumpsWithoutNotifyWhenStepHasNoChannels(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	const incidentID = int64(9014)

	ladder := escalation.Ladder{{StepNo: 0, DelayMinutes: 0, ChannelIDs: nil}}
	var notifyCalls int
	var bumpCalled bool
	var bumpFrom int
	sent, err := escalation.SendStepIfDue(ctx, ladder, "metric", pool, incidentID, 0, 0,
		func(chs []int64, step int) ([]int64, error) { notifyCalls++; return chs, nil },
		func(id int64, from int) (bool, error) { bumpCalled = true; bumpFrom = from; return true, nil })
	if err != nil {
		t.Fatalf("SendStepIfDue: %v", err)
	}
	if !sent {
		t.Error("sent = false, want true (bump применился)")
	}
	if notifyCalls != 0 {
		t.Errorf("notifyStep вызван %d раз, want 0 (лесенка без каналов на этой ступени)", notifyCalls)
	}
	if !bumpCalled {
		t.Fatal("bump не вызван — лесенка без каналов не должна клинить эскалацию")
	}
	if bumpFrom != 0 {
		t.Errorf("bump(from=%d), want 0", bumpFrom)
	}

	var count int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM incident_escalations WHERE incident_source='metric' AND incident_id=$1 AND step=0",
		incidentID).Scan(&count); err != nil {
		t.Fatalf("select incident_escalations: %v", err)
	}
	if count != 0 {
		t.Errorf("incident_escalations rows = %d, want 0 (claim не звался — каналов нет)", count)
	}
}

func TestSendStepIfDueLogsReleaseErrorOnTotalNotifyFailure(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	pid := newProject(t, pool)
	c1 := newChannel(t, pool, pid, true)
	const incidentID = int64(9015)
	errBoom := errors.New("outbox down")

	// BEFORE DELETE триггер с RAISE EXCEPTION: claim (INSERT) проходит штатно, должен
	// провалиться именно ReleaseStepChannels (DELETE).
	if _, err := pool.Exec(ctx, `
		CREATE OR REPLACE FUNCTION test_force_release_fail() RETURNS trigger AS $$
		BEGIN
			RAISE EXCEPTION 'test: release forbidden';
		END;
		$$ LANGUAGE plpgsql`); err != nil {
		t.Fatalf("create trigger function: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		CREATE TRIGGER test_force_release_fail_trg BEFORE DELETE ON incident_escalations
		FOR EACH ROW EXECUTE FUNCTION test_force_release_fail()`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = pool.Exec(ctx, "DROP TRIGGER IF EXISTS test_force_release_fail_trg ON incident_escalations")
		_, _ = pool.Exec(ctx, "DROP FUNCTION IF EXISTS test_force_release_fail()")
	})

	ladder := escalation.Ladder{{StepNo: 0, DelayMinutes: 0, ChannelIDs: []int64{c1}}}
	var bumpCalled bool
	sent, err := escalation.SendStepIfDue(ctx, ladder, "metric", pool, incidentID, 0, 0,
		func(chs []int64, step int) ([]int64, error) { return nil, errBoom },
		func(id int64, from int) (bool, error) { bumpCalled = true; return true, nil })
	if sent {
		t.Error("sent = true, want false (тотальный провал notifyStep)")
	}
	if !errors.Is(err, errBoom) {
		t.Fatalf("err = %v, want содержит errBoom", err)
	}
	if !strings.Contains(err.Error(), "release") {
		t.Fatalf("err = %v, want ТАКЖЕ содержит ошибку release (не только errBoom)", err)
	}
	if bumpCalled {
		t.Error("bump вызван — не должен при тотальном провале")
	}

	// Release не смог удалить строку — claim остался залогированным.
	var count int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM incident_escalations WHERE incident_source='metric' AND incident_id=$1 AND channel_id=$2 AND step=0",
		incidentID, c1).Scan(&count); err != nil {
		t.Fatalf("select incident_escalations: %v", err)
	}
	if count != 1 {
		t.Errorf("incident_escalations rows for c1 = %d, want 1 (release провалился — claim не откатился)", count)
	}
}
