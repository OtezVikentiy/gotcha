package export

import (
	"context"
	"errors"
	"testing"
	"time"
)

// fakeProbe отдаёт заданный снимок или ошибку. Симметрично
// internal/notify/stats_internal_test.go — Stats устроена так же.
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
// выглядела бы как здоровая очередь выгрузок: «ждёт 0 заявок, старейшей 0
// секунд». Заодно проверяет OldestPendingAgeSeconds — единственное из трёх
// чисел, которое отличает «очередь пуста» от «очередь стоит» (см. докблок
// QueueSnapshot.OldestPendingAge), и до этого теста не было проверено ни
// разу.
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
