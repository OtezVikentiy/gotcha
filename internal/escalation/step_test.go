package escalation_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/escalation"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// TestSendStepIfDueLogsEnqueuedChannels — T7-fix: логирование incident_
// escalations живёт в SendStepIfDue (оркестрация), не в нотифаере — оно
// обязано писаться независимо от того, что стоит за notifyStep (реальный
// Outbox-нотифаер или тестовый мок эволюатора), лишь бы тот вернул реально
// заенкенные каналы. Здесь notifyStep — фейк, возвращающий фиксированный
// набор без похода в Outbox вовсе, и лог всё равно появляется — это и есть
// гарантия, которую раньше давал только реальный нотифаер (T6), а с мок-
// нотифаерами (host/slo) она молчала.
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
			// Симулирует реальный нотифаер: возвращает то, что "реально
			// поставлено в очередь" — здесь всё, что дала лесенка.
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

// TestSendStepIfDueSkipsWhenDelayNotDue — задержка ступени ещё не настала
// (elapsed < DelayMinutes): ни notifyStep, ни bump не зовутся, лог пуст —
// планировщик (T8) отправит эту ступень позже.
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

// TestSendStepIfDueLevelBeyondLadder — level >= len(ladder) (лесенка
// исчерпана, эскалировать дальше некуда): sent=false, ничего не зовётся.
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

// TestSendStepIfDueNotifyStepErrorSkipsLogAndBump — provал notifyStep не
// логирует (нечего логировать — возвращённый слайс отбрасывается вместе с
// ошибкой) и не бампает уровень: провалившаяся отправка не должна молчаливо
// продвигать эскалацию дальше.
func TestSendStepIfDueNotifyStepErrorSkipsLogAndBump(t *testing.T) {
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
		t.Error("sent = true, want false (notifyStep провалился)")
	}
	if bumpCalled {
		t.Error("bump вызван при провале notifyStep — не должен")
	}

	var count int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM incident_escalations WHERE incident_source='metric' AND incident_id=$1",
		incidentID).Scan(&count); err != nil {
		t.Fatalf("select escalation log: %v", err)
	}
	if count != 0 {
		t.Errorf("incident_escalations rows = %d, want 0 (notifyStep провалился)", count)
	}
}
