package web_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/event"
	"gitflic.ru/otezvikentiy/gotcha/internal/issue"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
	"gitflic.ru/otezvikentiy/gotcha/internal/web"
)

// uptimeStack — свой стенд (не newStack/issuesStack): нужен и h.Uptime, и
// h.UptimeWriter (heartbeat пишет и в PG monitor_state/last_beat_at, и в CH
// check_results), которых нет в общих стендах остальных web-тестов.
type uptimeStack struct {
	pool   *pgxpool.Pool
	srv    *httptest.Server
	uptime *uptime.Service
	writer *uptime.ResultWriter
}

func newUptimeStack(t *testing.T) *uptimeStack {
	t.Helper()
	return newUptimeStackInRegion(t, "")
}

// newUptimeStackInRegion — стенд с явно заданным именем встроенного региона
// (GOTCHA_LOCAL_REGION в проде): его получают и Handler, и uptime.Service, ровно
// как в cmd/gotcha/main.go. Пустая строка — дефолт (uptime.DefaultRegion).
func newUptimeStackInRegion(t *testing.T, localRegion string) *uptimeStack {
	t.Helper()
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)

	authSvc := auth.NewService(pool)
	orgSvc := org.NewService(pool, 1_000_000)
	issueSvc := issue.NewService(pool)
	var events *event.Query

	uptimeSvc := uptime.NewService(pool)
	uptimeSvc.LocalRegion = localRegion
	writer := uptime.NewResultWriter(ch)
	go writer.Run()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = writer.Close(ctx)
	})

	mux := http.NewServeMux()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)

	h := web.New(authSvc, orgSvc, issueSvc, events, srv.URL)
	h.Uptime = uptimeSvc
	h.UptimeWriter = writer
	h.LocalRegion = localRegion
	h.Register(mux)

	return &uptimeStack{pool: pool, srv: srv, uptime: uptimeSvc, writer: writer}
}

var heartbeatProjectSeq atomic.Int64

// newProject — прямые вставки в обход org.Service, зеркалит одноимённый
// хелпер internal/uptime/monitor_test.go: heartbeat-тестам не нужен
// зарегистрированный юзер/сессия (эндпойнт публичный), только project_id для
// монитора.
func newProject(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	ctx := context.Background()
	n := heartbeatProjectSeq.Add(1)
	var userID, orgID, projectID int64
	if err := pool.QueryRow(ctx,
		"INSERT INTO users (email, password_hash) VALUES ($1,'x') RETURNING id",
		fmt.Sprintf("hb-u%d@example.com", n)).Scan(&userID); err != nil {
		t.Fatalf("user: %v", err)
	}
	if err := pool.QueryRow(ctx,
		"INSERT INTO organizations (slug, name, event_quota) VALUES ($1,'Up',1000000) RETURNING id",
		fmt.Sprintf("hb-%d", n)).Scan(&orgID); err != nil {
		t.Fatalf("org: %v", err)
	}
	if err := pool.QueryRow(ctx,
		"INSERT INTO projects (org_id, slug, name) VALUES ($1,'api','API') RETURNING id", orgID).Scan(&projectID); err != nil {
		t.Fatalf("project: %v", err)
	}
	return projectID
}

func heartbeatConfigJSON(t *testing.T, cfg uptime.HeartbeatConfig) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal heartbeat config: %v", err)
	}
	return raw
}

func TestHeartbeatValidTokenTouchesAndAppliesUp(t *testing.T) {
	s := newUptimeStack(t)
	pid := newProject(t, s.pool)

	m := uptime.Monitor{
		ProjectID:          pid,
		Name:               "Cron job",
		Kind:               uptime.KindHeartbeat,
		Enabled:            true,
		IntervalSeconds:    60,
		TimeoutSeconds:     10,
		FailThreshold:      3,
		RecoveryThreshold:  1,
		Consensus:          uptime.ConsensusMajority,
		RemindEveryMinutes: 0,
		SSLAlertDays:       14,
		Config:             heartbeatConfigJSON(t, uptime.HeartbeatConfig{GraceSeconds: 60}),
	}
	created, err := s.uptime.Create(context.Background(), m, []string{"local"}, nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.HeartbeatToken == "" {
		t.Fatalf("HeartbeatToken is empty, want generated token")
	}

	resp, err := http.Post(s.srv.URL+"/uptime/hb/"+created.HeartbeatToken, "", nil)
	if err != nil {
		t.Fatalf("POST heartbeat: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, body)
	}

	got, err := s.uptime.Get(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.LastBeatAt == nil {
		t.Fatalf("LastBeatAt is nil, want set")
	}

	states, err := s.uptime.States(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("States: %v", err)
	}
	if len(states) != 1 || states[0].Status != "up" {
		t.Fatalf("states = %+v, want single up state", states)
	}

	// GET also works, not just POST.
	resp2, err := http.Get(s.srv.URL + "/uptime/hb/" + created.HeartbeatToken)
	if err != nil {
		t.Fatalf("GET heartbeat: %v", err)
	}
	io.Copy(io.Discard, resp2.Body)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("GET status = %d, want 200", resp2.StatusCode)
	}
}

// TestHeartbeatOversizedBodyReturns413 verifies that the heartbeat
// endpoint's 1 KB body cap (heartbeatMaxBodyBytes) is actually enforced. The
// handler wraps r.Body in http.MaxBytesReader but must also READ the body
// for that cap to trigger — the stdlib server does not itself drain the
// unread body against a MaxBytesReader's limit.
func TestHeartbeatOversizedBodyReturns413(t *testing.T) {
	s := newUptimeStack(t)
	pid := newProject(t, s.pool)

	m := uptime.Monitor{
		ProjectID:          pid,
		Name:               "Cron job",
		Kind:               uptime.KindHeartbeat,
		Enabled:            true,
		IntervalSeconds:    60,
		TimeoutSeconds:     10,
		FailThreshold:      3,
		RecoveryThreshold:  1,
		Consensus:          uptime.ConsensusMajority,
		RemindEveryMinutes: 0,
		SSLAlertDays:       14,
		Config:             heartbeatConfigJSON(t, uptime.HeartbeatConfig{GraceSeconds: 60}),
	}
	created, err := s.uptime.Create(context.Background(), m, []string{"local"}, nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	body := bytes.Repeat([]byte("x"), 2<<10) // 2 KB, over the 1 KB cap
	resp, err := http.Post(s.srv.URL+"/uptime/hb/"+created.HeartbeatToken, "application/octet-stream", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST heartbeat: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", resp.StatusCode)
	}
}

func TestHeartbeatUnknownTokenReturns404(t *testing.T) {
	s := newUptimeStack(t)

	resp, err := http.Post(s.srv.URL+"/uptime/hb/does-not-exist", "", nil)
	if err != nil {
		t.Fatalf("POST heartbeat: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}
