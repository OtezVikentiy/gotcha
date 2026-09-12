package export

import (
	"context"
	"errors"
	"testing"
	"time"
)

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

func TestSnapshotBeforeFirstProbeIsZero(t *testing.T) {
	var s Stats
	if got := s.Snapshot(); got != (QueueSnapshot{}) {
		t.Errorf("Snapshot до опроса = %+v, want нули", got)
	}
	if got := s.OldestPendingAgeSeconds(); got != 0 {
		t.Errorf("OldestPendingAgeSeconds = %d, want 0", got)
	}
}

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
