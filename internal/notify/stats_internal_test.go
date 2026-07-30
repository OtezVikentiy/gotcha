package notify

import (
	"context"
	"errors"
	"testing"
	"time"
)

// fakeProbe отдаёт заданный снимок или ошибку.
type fakeProbe struct {
	snap  QueueSnapshot
	err   error
	calls int
}

func (f *fakeProbe) QueueSnapshot(ctx context.Context) (QueueSnapshot, error) {
	f.calls++
	if f.err != nil {
		return QueueSnapshot{}, f.err
	}
	return f.snap, nil
}

// TestStatsCountersAreIndependent: три исхода доставки считаются раздельно —
// иначе «отправлено 100» ничего не говорит о том, сколько из них дошло с
// первого раза.
func TestStatsCountersAreIndependent(t *testing.T) {
	var s Stats
	s.countSent()
	s.countSent()
	s.countFailed()
	s.countRetried()
	s.countRetried()
	s.countRetried()

	if got := s.Sent(); got != 2 {
		t.Errorf("Sent = %d, want 2", got)
	}
	if got := s.Failed(); got != 1 {
		t.Errorf("Failed = %d, want 1", got)
	}
	if got := s.Retried(); got != 3 {
		t.Errorf("Retried = %d, want 3", got)
	}
}

// TestSnapshotBeforeFirstProbeIsZero: до первого опроса метрики отдают нули, а
// не мусор.
func TestSnapshotBeforeFirstProbeIsZero(t *testing.T) {
	var s Stats
	if got := s.Snapshot(); got != (QueueSnapshot{}) {
		t.Errorf("Snapshot до опроса = %+v, want нули", got)
	}
	if got := s.OldestPendingAgeSeconds(); got != 0 {
		t.Errorf("OldestPendingAgeSeconds = %d, want 0", got)
	}
}

// TestRefreshKeepsLastSnapshotOnError — ключевое решение: неудачный опрос
// оставляет прежний снимок. Обнулять его нельзя, иначе недоступная база
// выглядела бы как здоровая доставка: «ждёт 0 задач, старейшей 0 секунд».
func TestRefreshKeepsLastSnapshotOnError(t *testing.T) {
	var s Stats
	probe := &fakeProbe{snap: QueueSnapshot{Pending: 42, Failed: 7, OldestPendingAge: 3 * time.Hour}}
	s.refresh(context.Background(), probe)

	probe.err = errors.New("connection refused")
	s.refresh(context.Background(), probe)

	if got := s.Pending(); got != 42 {
		t.Errorf("Pending = %d, want 42: неудачный опрос обнулил снимок, и стоящая "+
			"очередь стала выглядеть пустой", got)
	}
	if got := s.FailedJobs(); got != 7 {
		t.Errorf("FailedJobs = %d, want 7", got)
	}
	if got := s.OldestPendingAgeSeconds(); got != int64(3*time.Hour/time.Second) {
		t.Errorf("OldestPendingAgeSeconds = %d, want %d", got, int64(3*time.Hour/time.Second))
	}
}

// TestRunSnapshotsProbesImmediately: первый опрос идёт сразу, а не через
// интервал. Иначе после рестарта метрики четверть минуты показывают нули —
// ровно тогда, когда на них смотрят.
func TestRunSnapshotsProbesImmediately(t *testing.T) {
	var s Stats
	probe := &fakeProbe{snap: QueueSnapshot{Pending: 5}}
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		s.RunSnapshots(ctx, probe)
		close(done)
	}()

	deadline := time.After(2 * time.Second)
	for s.Pending() == 0 {
		select {
		case <-deadline:
			cancel()
			t.Fatal("снимок не появился за 2 секунды — первый опрос ждёт тикера")
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunSnapshots не завершился по отмене контекста")
	}
}
