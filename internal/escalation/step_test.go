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

// TestSendStepIfDueNotifyStepTotalFailureSkipsLogAndBump — ТОТАЛЬНЫЙ провал
// notifyStep (ни один канал не заенкенился — enqueued пуст) не логирует
// (нечего логировать) и не бампает уровень: следующий тик повторит ступень
// целиком, а не молчаливо продвинет эскалацию дальше. QA P2-3: до фикса
// discard всех enqueued при err != nil был общим для тотального и частичного
// сбоя — здесь фиксируем, что для тотального сбоя (пустой enqueued) поведение
// осталось прежним; частичный сбой см. TestSendStepIfDueNotifyStepPartialFailureLogsAndBumps.
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

// TestSendStepIfDueNotifyStepPartialFailureLogsAndBumps — ЧАСТИЧНЫЙ провал
// notifyStep (c1 реально заенкенился, но вызов вернул ошибку — напр. второй
// канал в очередь не встал) обязан залогировать c1 в incident_escalations
// (иначе recovery не найдёт его и не пришлёт отбой запейдженному каналу) И
// продвинуть уровень (иначе один битый канал клинит лесенку бесконечным
// пере-пейджем c1). Ошибка при этом не проглатывается — прокидывается вызывающему. QA P2-3.
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
