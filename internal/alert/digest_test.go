package alert

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// fakeEnqueuer — Enqueuer в памяти, фиксирующий вызовы по каналам и умеющий
// провалиться для заданного channelID (K1-2: дискриминирует "один битый
// канал не должен глушить остальные" от Digester.send).
type fakeEnqueuer struct {
	failFor map[int64]error
	calls   []int64
}

func (f *fakeEnqueuer) Enqueue(ctx context.Context, channelID int64, payload map[string]any) error {
	f.calls = append(f.calls, channelID)
	if err, ok := f.failFor[channelID]; ok {
		return err
	}
	return nil
}

// newDigestProject — whitebox-сид (package alert, недоступны хелперы
// alert_test), тот же приём, что и rewrap_secrets_internal_test.go.
func newDigestProject(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	ctx := context.Background()
	var orgID, projectID int64
	if err := pool.QueryRow(ctx, "INSERT INTO organizations (slug, name, event_quota) VALUES ($1,$1,1000000) RETURNING id",
		"digest-"+t.Name()).Scan(&orgID); err != nil {
		t.Fatalf("insert org: %v", err)
	}
	if err := pool.QueryRow(ctx, "INSERT INTO projects (org_id, slug, name) VALUES ($1,$2,$2) RETURNING id",
		orgID, "digest-"+t.Name()).Scan(&projectID); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	return projectID
}

// TestDigesterSendContinuesAfterEnqueueFailure — K1-2 (аудит перед 1.0):
// раньше первая же провалившаяся Enqueue обрывала send через return — канал,
// идущий по списку ПОСЛЕ битого, не получал сводку вовсе, хотя сам был
// исправен. Два включённых webhook-канала, первый — падает на Enqueue:
// второй обязан получить вызов Enqueue всё равно, а итоговая ошибка —
// быть ненулевой и называть id первого канала.
func TestDigesterSendContinuesAfterEnqueueFailure(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	svc := NewService(pool)
	pid := newDigestProject(t, pool)

	ch1, err := svc.CreateChannel(ctx, Channel{ProjectID: pid, Kind: ChannelWebhook, Enabled: true, Target: "https://example.com/hook1"})
	if err != nil {
		t.Fatalf("CreateChannel ch1: %v", err)
	}
	ch2, err := svc.CreateChannel(ctx, Channel{ProjectID: pid, Kind: ChannelWebhook, Enabled: true, Target: "https://example.com/hook2"})
	if err != nil {
		t.Fatalf("CreateChannel ch2: %v", err)
	}

	wantErr := errors.New("channel 1 unreachable")
	fe := &fakeEnqueuer{failFor: map[int64]error{ch1: wantErr}}
	d := &Digester{Svc: svc, Outbox: fe, BaseURL: "https://gotcha.example", Details: NewDetailPolicy("", nil, true)}

	err = d.send(ctx, SuppressedBatch{ProjectID: pid, Suppressed: 3, Since: time.Now().Add(-time.Hour)})
	if err == nil {
		t.Fatal("send err = nil, want ошибку (Enqueue первого канала провалился)")
	}
	if !strings.Contains(err.Error(), "channel 1 unreachable") && !errors.Is(err, wantErr) {
		t.Errorf("err = %v, want содержащую wantErr/id первого канала", err)
	}

	if len(fe.calls) != 2 {
		t.Fatalf("Enqueue вызван %d раз, want 2 (второй канал должен получить вызов несмотря на провал первого)", len(fe.calls))
	}
	found1, found2 := false, false
	for _, id := range fe.calls {
		if id == ch1 {
			found1 = true
		}
		if id == ch2 {
			found2 = true
		}
	}
	if !found1 || !found2 {
		t.Fatalf("Enqueue calls = %v, want оба канала [%d, %d]", fe.calls, ch1, ch2)
	}
}
