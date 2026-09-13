package alert

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/notify"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

type fakeEnqueuer struct {
	failFor map[int64]error
	calls   []int64
}

func (f *fakeEnqueuer) EnqueueIdempotent(ctx context.Context, channelID int64, payload map[string]any, key string) (bool, error) {
	f.calls = append(f.calls, channelID)
	if err, ok := f.failFor[channelID]; ok {
		return false, err
	}
	return true, nil
}

// Whitebox-сид (package alert, недоступны хелперы alert_test) — тот же приём,
// что rewrap_secrets_internal_test.go.
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

// Вставляет issue напрямую — тот же приём, что newEvalIssue в alert_test
// (недоступен отсюда: этот файл — package alert, whitebox).
func newDigestIssue(t *testing.T, pool *pgxpool.Pool, projectID int64, fingerprint string) int64 {
	t.Helper()
	ctx := context.Background()
	var id int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO issues (project_id, fingerprint, title, culprit, level, first_seen, last_seen)
		VALUES ($1, $2, 'boom', 'app.x', 'error', now(), now()) RETURNING id`,
		projectID, fingerprint).Scan(&id); err != nil {
		t.Fatalf("issue: %v", err)
	}
	return id
}

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

// ClaimSuppressed необратимо обнуляет счётчик в момент забора — без возврата
// провал send() терял бы сводку навсегда.
func TestDigesterTickRestoresSuppressedOnSendFailure(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	svc := NewService(pool)
	svc.SetBudget(time.Second, 1)
	ctx := context.Background()
	pid := newDigestProject(t, pool)

	ch, err := svc.CreateChannel(ctx, Channel{ProjectID: pid, Kind: ChannelWebhook, Enabled: true, Target: "https://example.com/hook"})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	if _, err := svc.UpsertRule(ctx, Rule{ProjectID: pid, Kind: KindNewIssue, Enabled: true}); err != nil {
		t.Fatalf("UpsertRule: %v", err)
	}

	ob := notify.NewOutbox(pool)
	e := &Evaluator{Svc: svc, Outbox: ob, BaseURL: "https://gotcha.example", Details: NewDetailPolicy("", nil, true)}
	for i := 0; i < 4; i++ {
		issueID := newDigestIssue(t, pool, pid, fmt.Sprintf("fp-restore-%d", i))
		e.OnIssue(ctx, Event{ProjectID: pid, IssueID: issueID, Kind: KindNewIssue, Title: "boom"})
	}
	if _, err := ob.Claim(ctx, 100); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	time.Sleep(1200 * time.Millisecond) // окно бюджета должно закрыться

	fe := &fakeEnqueuer{failFor: map[int64]error{ch: errors.New("hook unreachable")}}
	d := &Digester{Svc: svc, Outbox: fe, BaseURL: "https://gotcha.example", Details: NewDetailPolicy("", nil, true)}
	d.Tick(ctx)

	if got := d.LostSuppressed(); got != 0 {
		t.Errorf("LostSuppressed() = %d, want 0 — restore должен был вернуть счётчик, а не потерять его", got)
	}

	// Тот же забор, что сделал бы следующий тик: сводка обязана появиться
	// снова, а не остаться потерянной вместе с обнулённым claim'ом.
	again, err := svc.ClaimSuppressed(ctx, 10)
	if err != nil {
		t.Fatalf("ClaimSuppressed после провала: %v", err)
	}
	var got int
	found := false
	for _, b := range again {
		if b.ProjectID == pid {
			got = b.Suppressed
			found = true
		}
	}
	if !found {
		t.Fatalf("после провала send() сводка проекта %d не восстановилась в счётчике", pid)
	}
	if got != 3 {
		t.Errorf("восстановленных подавленных %d, want 3 (4 события − потолок 1)", got)
	}
}

// send() ретраит тот же батч (тот же b.Since) через RestoreSuppressed — без
// идемпотентного ключа канал, уже получивший сводку, получил бы вторую копию.
func TestDigesterSendIsIdempotentPerWindow(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	svc := NewService(pool)
	ctx := context.Background()
	pid := newDigestProject(t, pool)

	ch, err := svc.CreateChannel(ctx, Channel{ProjectID: pid, Kind: ChannelWebhook, Enabled: true, Target: "https://example.com/hook"})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}

	ob := notify.NewOutbox(pool)
	d := &Digester{Svc: svc, Outbox: ob, BaseURL: "https://gotcha.example", Details: NewDetailPolicy("", nil, true)}
	b := SuppressedBatch{ProjectID: pid, Suppressed: 5, Since: time.Now().Add(-time.Hour)}

	if err := d.send(ctx, b); err != nil {
		t.Fatalf("send (первая попытка): %v", err)
	}
	if err := d.send(ctx, b); err != nil {
		t.Fatalf("send (повтор того же окна): %v", err)
	}

	var count int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM notification_outbox WHERE channel_id = $1", ch).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Errorf("в outbox %d задач для канала %d, want 1 — повтор того же окна задвоил сводку", count, ch)
	}

	// Другое окно того же канала — самостоятельная сводка, ключ не смеет
	// схлопнуть её с предыдущей: иначе вторая просто теряется.
	b2 := SuppressedBatch{ProjectID: pid, Suppressed: 2, Since: time.Now().Add(-2 * time.Hour)}
	if err := d.send(ctx, b2); err != nil {
		t.Fatalf("send (другое окно): %v", err)
	}
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM notification_outbox WHERE channel_id = $1", ch).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 2 {
		t.Errorf("в outbox %d задач для канала %d, want 2 — сводка другого окна схлопнута с предыдущей", count, ch)
	}
}

func TestDigesterPublishesTickLiveness(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := NewService(pool)
	d := &Digester{Svc: svc, Outbox: notify.NewOutbox(pool), Interval: time.Hour}

	if got := d.LastTickUnix(); got != 0 {
		t.Fatalf("LastTickUnix до первого тика = %d, want 0", got)
	}
	before := time.Now().Unix()
	d.Tick(context.Background())

	if got := d.LastTickUnix(); got < before {
		t.Errorf("LastTickUnix = %d, want >= %d (момент завершения тика)", got, before)
	}
	if got := d.LastTickSeconds(); got < 0 || got > 5 {
		t.Errorf("LastTickSeconds = %v, want положительную длительность в разумных пределах", got)
	}
}

type slowEnqueuer struct {
	delay time.Duration
	calls atomic.Int32
}

func (f *slowEnqueuer) EnqueueIdempotent(ctx context.Context, channelID int64, payload map[string]any, key string) (bool, error) {
	f.calls.Add(1)
	time.Sleep(f.delay)
	return true, nil
}

// Без ctx.Err() в цикле по батчам Tick доставил бы все три сводки, пусть и
// вдвое дольше бюджета — здесь третья обязана остаться нетронутой.
func TestDigesterTickBudgetSkipsRemainingBatches(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	svc := NewService(pool)
	ctx := context.Background()

	pids := make([]int64, 3)
	for i := 0; i < 3; i++ {
		var orgID, pid int64
		slug := fmt.Sprintf("digest-budget-%d", i)
		if err := pool.QueryRow(ctx, "INSERT INTO organizations (slug, name, event_quota) VALUES ($1,$1,1000000) RETURNING id",
			slug).Scan(&orgID); err != nil {
			t.Fatalf("insert org: %v", err)
		}
		if err := pool.QueryRow(ctx, "INSERT INTO projects (org_id, slug, name) VALUES ($1,$2,$2) RETURNING id",
			orgID, slug).Scan(&pid); err != nil {
			t.Fatalf("insert project: %v", err)
		}
		if _, err := svc.CreateChannel(ctx, Channel{ProjectID: pid, Kind: ChannelWebhook, Enabled: true, Target: "https://example.com/hook"}); err != nil {
			t.Fatalf("CreateChannel: %v", err)
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO alert_project_budget (project_id, window_start, suppressed) VALUES ($1, now() - interval '2 hours', 3)`,
			pid); err != nil {
			t.Fatalf("seed budget: %v", err)
		}
		pids[i] = pid
	}

	// Interval=1s держит tickBudget() на полу (10с); каждый батч съедает 6с —
	// третий батч стартует уже за пределами бюджета.
	fe := &slowEnqueuer{delay: 6 * time.Second}
	d := &Digester{Svc: svc, Outbox: fe, BaseURL: "https://gotcha.example", Details: NewDetailPolicy("", nil, true), Interval: time.Second}

	var logBuf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	d.Tick(ctx)

	if got := fe.calls.Load(); got != 2 {
		t.Errorf("EnqueueIdempotent вызван %d раз, want 2 — третий батч обязан быть пропущен по бюджету тика", got)
	}
	if got := d.LastTickUnix(); got != 0 {
		t.Errorf("LastTickUnix = %d после оборванного по бюджету тика, want 0", got)
	}
	// Без ctx.Err() в цикле третий батч упал бы сам внутри send() на просроченном
	// контексте, не увеличив fe.calls — разницу видно только по этому счётчику.
	if got := d.LastTickSkippedBatches(); got != 1 {
		t.Errorf("LastTickSkippedBatches() = %d, want 1 (третий батч пропущен по бюджету)", got)
	}
	// Пропуск обязан восстановить счётчик подавленных, обнулённый claim'ом —
	// иначе пропуск по бюджету означал бы такую же потерю, что и провал send().
	if got := d.LostSuppressed(); got != 0 {
		t.Errorf("LostSuppressed() = %d, want 0 — пропущенный батч обязан быть восстановлен, а не потерян", got)
	}
	var restored int
	if err := pool.QueryRow(ctx, "SELECT suppressed FROM alert_project_budget WHERE project_id = $1", pids[2]).Scan(&restored); err != nil {
		t.Fatalf("select suppressed: %v", err)
	}
	if restored != 3 {
		t.Errorf("suppressed третьего проекта после пропуска = %d, want 3 (восстановлен, не потерян)", restored)
	}
	// Первые два батча прошли send() штатно — их счётчик восстановлению не
	// подлежит, restoreSkipped не должен был их коснуться.
	for i := 0; i < 2; i++ {
		var sent int
		if err := pool.QueryRow(ctx, "SELECT suppressed FROM alert_project_budget WHERE project_id = $1", pids[i]).Scan(&sent); err != nil {
			t.Fatalf("select suppressed: %v", err)
		}
		if sent != 0 {
			t.Errorf("suppressed отправленного проекта %d = %d, want 0 (не задвоено restoreSkipped)", i, sent)
		}
	}
	logs := logBuf.String()
	if !strings.Contains(logs, "tick budget exhausted, remaining batches skipped") {
		t.Errorf("лог не содержит явного пропуска по бюджету: %s", logs)
	}
	if strings.Contains(logs, "suppressed count restored for retry") || strings.Contains(logs, "tick budget exhausted, suppressed count lost") {
		t.Errorf("третий батч дошёл до send() или не смог восстановиться вместо чистого пропуска по бюджету: %s", logs)
	}

	// Восстановленное обязано дойти до следующего тика, не только осесть в
	// столбце — второй прогон обязан забрать и отправить третий проект.
	fe2 := &fakeEnqueuer{}
	d2 := &Digester{Svc: svc, Outbox: fe2, BaseURL: "https://gotcha.example", Details: NewDetailPolicy("", nil, true), Interval: time.Second}
	d2.Tick(context.Background())
	if len(fe2.calls) != 1 {
		t.Fatalf("второй тик вызвал EnqueueIdempotent %d раз, want 1 (переотправка восстановленного третьего проекта)", len(fe2.calls))
	}
	if got := d2.LastTickSkippedBatches(); got != 0 {
		t.Errorf("второй тик LastTickSkippedBatches() = %d, want 0", got)
	}
	var afterSecondTick int
	if err := pool.QueryRow(ctx, "SELECT suppressed FROM alert_project_budget WHERE project_id = $1", pids[2]).Scan(&afterSecondTick); err != nil {
		t.Fatalf("select suppressed: %v", err)
	}
	if afterSecondTick != 0 {
		t.Errorf("suppressed третьего проекта после второго тика = %d, want 0 (сводка ушла)", afterSecondTick)
	}
}

// Если восстановление пропущенного само не укладывается в дедлайн, потеря
// обязана попасть в LostSuppressed; два батча проверяют, что дедлайн общий, не поштучный.
func TestDigesterTickBudgetSkipRestoreFails(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	svc := NewService(pool)
	ctx := context.Background()

	pids := make([]int64, 4)
	for i := 0; i < 4; i++ {
		var orgID, pid int64
		slug := fmt.Sprintf("digest-restorefail-%d", i)
		if err := pool.QueryRow(ctx, "INSERT INTO organizations (slug, name, event_quota) VALUES ($1,$1,1000000) RETURNING id",
			slug).Scan(&orgID); err != nil {
			t.Fatalf("insert org: %v", err)
		}
		if err := pool.QueryRow(ctx, "INSERT INTO projects (org_id, slug, name) VALUES ($1,$2,$2) RETURNING id",
			orgID, slug).Scan(&pid); err != nil {
			t.Fatalf("insert project: %v", err)
		}
		if _, err := svc.CreateChannel(ctx, Channel{ProjectID: pid, Kind: ChannelWebhook, Enabled: true, Target: "https://example.com/hook"}); err != nil {
			t.Fatalf("CreateChannel: %v", err)
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO alert_project_budget (project_id, window_start, suppressed) VALUES ($1, now() - interval '2 hours', 3)`,
			pid); err != nil {
			t.Fatalf("seed budget: %v", err)
		}
		pids[i] = pid
	}

	// Точечный лок по строкам, не по таблице — ClaimSuppressed (FOR UPDATE
	// SKIP LOCKED) не должен их пропустить, лочить нужно уже ПОСЛЕ claim.
	lockConn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer lockConn.Release()
	tx, err := lockConn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)

	fe := &slowEnqueuer{delay: 6 * time.Second}
	d := &Digester{Svc: svc, Outbox: fe, BaseURL: "https://gotcha.example", Details: NewDetailPolicy("", nil, true), Interval: time.Second}

	var logBuf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	done := make(chan struct{})
	started := time.Now()
	go func() {
		d.Tick(ctx)
		close(done)
	}()

	// Ждём по значению, не по паузе: лок обязан лечь строго после
	// ClaimSuppressed, иначе SKIP LOCKED тихо исключил бы строку из клейма.
	claimDeadline := time.Now().Add(5 * time.Second)
	for {
		var suppressed int
		if err := pool.QueryRow(ctx, "SELECT suppressed FROM alert_project_budget WHERE project_id = $1", pids[3]).Scan(&suppressed); err != nil {
			t.Fatalf("poll suppressed: %v", err)
		}
		if suppressed == 0 {
			break
		}
		if time.Now().After(claimDeadline) {
			t.Fatalf("ClaimSuppressed не забрал проект %d за 5s", pids[3])
		}
		time.Sleep(10 * time.Millisecond)
	}
	for _, pid := range []int64{pids[2], pids[3]} {
		if _, err := tx.Exec(ctx, "SELECT suppressed FROM alert_project_budget WHERE project_id = $1 FOR UPDATE", pid); err != nil {
			t.Fatalf("lock row: %v", err)
		}
	}

	select {
	case <-done:
	case <-time.After(45 * time.Second):
		t.Fatal("Tick не вернулся за 45s — restoreSkipped не ограничен собственным бюджетом")
	}
	// ~12с на два отправленных батча + один общий restoreSkippedTimeout (10с)
	// на оба заблокированных: по 10с на каждый заблокированный дал бы ~32с.
	if dur := time.Since(started); dur > 28*time.Second {
		t.Errorf("Tick вернулся через %v, want < 28s — дедлайн restoreSkipped общий на все пропущенные, не по одному на каждый", dur)
	}

	if got := d.LastTickSkippedBatches(); got != 2 {
		t.Errorf("LastTickSkippedBatches() = %d, want 2", got)
	}
	if got := d.LostSuppressed(); got != 6 {
		t.Errorf("LostSuppressed() = %d, want 6 — оба восстановления упёрлись в дедлайн и обязаны считаться потерей", got)
	}
	logs := logBuf.String()
	if !strings.Contains(logs, "tick budget exhausted, suppressed count lost") {
		t.Errorf("лог не содержит потери при неудачном восстановлении: %s", logs)
	}
}

func TestDigesterTickBudget(t *testing.T) {
	tests := []struct {
		name     string
		interval time.Duration
		want     time.Duration
	}{
		{"ниже пола — берём пол", time.Second, minDigestTickBudget},
		{"выше пола — доля Interval", 60 * time.Second, 48 * time.Second},
		{"нулевой Interval — доля дефолта, как у Run", 0, time.Duration(float64(digestInterval) * digestTickBudgetShare)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := &Digester{Interval: tt.interval}
			if got := d.tickBudget(); got != tt.want {
				t.Errorf("tickBudget() при Interval=%v = %v, want %v", tt.interval, got, tt.want)
			}
		})
	}
}
