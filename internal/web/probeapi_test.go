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

// probeStack — стенд lease-протокола: Handler с Uptime + UptimeWriter +
// UptimeIngestor (фейковый OnResult вместо реального детектора — проверяем,
// что центр действительно прогоняет присланный пробой результат через
// детекцию).
type probeStack struct {
	pool    *pgxpool.Pool
	srv     *httptest.Server
	uptime  *uptime.Service
	writer  *uptime.ResultWriter
	results chan uptime.State
}

func newProbeStack(t *testing.T) *probeStack {
	t.Helper()
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)

	authSvc := auth.NewService(pool)
	orgSvc := org.NewService(pool, 1_000_000)
	issueSvc := issue.NewService(pool)
	var events *event.Query

	uptimeSvc := uptime.NewService(pool)
	writer := uptime.NewResultWriter(ch)
	go writer.Run()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = writer.Close(ctx)
	})

	results := make(chan uptime.State, 8)

	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	h := web.New(authSvc, orgSvc, issueSvc, events, srv.URL)
	h.Uptime = uptimeSvc
	h.UptimeWriter = writer
	h.UptimeIngestor = &uptime.Ingestor{
		Svc:    uptimeSvc,
		Writer: writer,
		OnResult: func(_ context.Context, _ uptime.Monitor, _ string, _ uptime.Result, st uptime.State) {
			select {
			case results <- st:
			default:
			}
		},
	}
	h.Register(mux)

	return &probeStack{pool: pool, srv: srv, uptime: uptimeSvc, writer: writer, results: results}
}

var probeOrgSeq atomic.Int64

// newOrgProject заводит организацию и проект прямыми вставками (как
// newProject в heartbeat_test.go), но возвращает и org_id — пробы висят на
// организации.
func newOrgProject(t *testing.T, pool *pgxpool.Pool) (orgID, projectID int64) {
	t.Helper()
	ctx := context.Background()
	n := probeOrgSeq.Add(1)
	var userID int64
	if err := pool.QueryRow(ctx,
		"INSERT INTO users (email, password_hash) VALUES ($1,'x') RETURNING id",
		fmt.Sprintf("probe-u%d@example.com", n)).Scan(&userID); err != nil {
		t.Fatalf("user: %v", err)
	}
	if err := pool.QueryRow(ctx,
		"INSERT INTO organizations (slug, name, event_quota) VALUES ($1,'Up',1000000) RETURNING id",
		fmt.Sprintf("probe-%d", n)).Scan(&orgID); err != nil {
		t.Fatalf("org: %v", err)
	}
	if err := pool.QueryRow(ctx,
		"INSERT INTO projects (org_id, slug, name) VALUES ($1,'api','API') RETURNING id", orgID).Scan(&projectID); err != nil {
		t.Fatalf("project: %v", err)
	}
	return orgID, projectID
}

func probeHTTPMonitor(t *testing.T, projectID int64) uptime.Monitor {
	t.Helper()
	raw, err := json.Marshal(uptime.HTTPConfig{Method: "GET", URL: "https://example.com/health"})
	if err != nil {
		t.Fatalf("marshal http config: %v", err)
	}
	return uptime.Monitor{
		ProjectID:          projectID,
		Name:               "API health",
		Kind:               uptime.KindHTTP,
		Enabled:            true,
		IntervalSeconds:    60,
		TimeoutSeconds:     10,
		FailThreshold:      1,
		RecoveryThreshold:  1,
		Consensus:          uptime.ConsensusMajority,
		RemindEveryMinutes: 0,
		SSLAlertDays:       14,
		Config:             raw,
	}
}

// probePost шлёт машинный POST на эндпойнт lease-протокола. token == "" —
// запрос без заголовка Authorization.
func probePost(t *testing.T, s *probeStack, path, token string, body any) *http.Response {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode body: %v", err)
		}
	}
	req, err := http.NewRequest(http.MethodPost, s.srv.URL+path, &buf)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	return resp
}

func decodeJSON(t *testing.T, resp *http.Response, v any) {
	t.Helper()
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatalf("decode response: %v", err)
	}
}

func drain(resp *http.Response) {
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
}

func TestProbeLeaseWithoutTokenReturns401(t *testing.T) {
	s := newProbeStack(t)

	resp := probePost(t, s, "/probe/lease", "", uptime.LeaseRequest{Limit: 10})
	defer drain(resp)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

func TestProbeLeaseWithUnknownOrRevokedTokenReturns401(t *testing.T) {
	s := newProbeStack(t)
	ctx := context.Background()
	orgID, _ := newOrgProject(t, s.pool)

	probe, token, err := s.uptime.CreateProbe(ctx, orgID, "eu-west", "Probe 1")
	if err != nil {
		t.Fatalf("CreateProbe: %v", err)
	}

	resp := probePost(t, s, "/probe/lease", "not-a-real-token", nil)
	drain(resp)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unknown token: status = %d, want 401", resp.StatusCode)
	}

	if err := s.uptime.RevokeProbe(ctx, probe.ID); err != nil {
		t.Fatalf("RevokeProbe: %v", err)
	}
	resp2 := probePost(t, s, "/probe/lease", token, nil)
	drain(resp2)
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revoked token: status = %d, want 401", resp2.StatusCode)
	}
}

func TestProbeLeaseReturnsOwnRegionJobsAndTouchesProbe(t *testing.T) {
	s := newProbeStack(t)
	ctx := context.Background()
	orgID, pid := newOrgProject(t, s.pool)

	probe, token, err := s.uptime.CreateProbe(ctx, orgID, "eu-west", "Probe 1")
	if err != nil {
		t.Fatalf("CreateProbe: %v", err)
	}

	// Монитор в двух регионах: проба обязана получить только своё задание.
	created, err := s.uptime.Create(ctx, probeHTTPMonitor(t, pid), []string{"local", "eu-west"}, nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := s.uptime.Schedule(ctx); err != nil {
		t.Fatalf("Schedule: %v", err)
	}

	resp := probePost(t, s, "/probe/lease", token, uptime.LeaseRequest{})
	if resp.StatusCode != http.StatusOK {
		drain(resp)
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var lease uptime.LeaseResponse
	decodeJSON(t, resp, &lease)

	if lease.ProbeID != probe.ID || lease.Region != "eu-west" {
		t.Fatalf("lease = probe %d region %q, want probe %d region eu-west", lease.ProbeID, lease.Region, probe.ID)
	}
	if len(lease.Jobs) != 1 {
		t.Fatalf("lease.Jobs = %d, want 1 (only region eu-west)", len(lease.Jobs))
	}
	job := lease.Jobs[0]
	if job.MonitorID != created.ID || job.Kind != uptime.KindHTTP || job.TimeoutSeconds != 10 {
		t.Fatalf("job = %+v, want monitor %d kind http timeout 10", job, created.ID)
	}
	if len(job.Config) == 0 {
		t.Fatalf("job.Config is empty, want the monitor's config")
	}

	var leasedBy *int64
	if err := s.pool.QueryRow(ctx, "SELECT leased_by FROM check_queue WHERE id = $1", job.QueueID).Scan(&leasedBy); err != nil {
		t.Fatalf("select leased_by: %v", err)
	}
	if leasedBy == nil || *leasedBy != probe.ID {
		t.Fatalf("leased_by = %v, want %d", leasedBy, probe.ID)
	}

	probes, err := s.uptime.Probes(ctx, orgID)
	if err != nil {
		t.Fatalf("Probes: %v", err)
	}
	if len(probes) != 1 || probes[0].LastSeenAt == nil {
		t.Fatalf("probes = %+v, want last_seen_at set", probes)
	}
}

func TestProbeResultsAcceptsResultForOwnLeasedJob(t *testing.T) {
	s := newProbeStack(t)
	ctx := context.Background()
	orgID, pid := newOrgProject(t, s.pool)

	_, token, err := s.uptime.CreateProbe(ctx, orgID, "eu-west", "Probe 1")
	if err != nil {
		t.Fatalf("CreateProbe: %v", err)
	}
	created, err := s.uptime.Create(ctx, probeHTTPMonitor(t, pid), []string{"eu-west"}, nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := s.uptime.Schedule(ctx); err != nil {
		t.Fatalf("Schedule: %v", err)
	}

	resp := probePost(t, s, "/probe/lease", token, nil)
	var lease uptime.LeaseResponse
	decodeJSON(t, resp, &lease)
	if len(lease.Jobs) != 1 {
		t.Fatalf("lease.Jobs = %d, want 1", len(lease.Jobs))
	}

	req := uptime.ResultsRequest{Results: []uptime.ResultDTO{{
		QueueID:    lease.Jobs[0].QueueID,
		OK:         false,
		StatusCode: 500,
		Error:      "500 Internal Server Error",
		Timings:    uptime.Timings{DNS: 1, Connect: 2, TLS: 3, TTFB: 4, Total: 9},
		BodySize:   123,
	}}}
	rresp := probePost(t, s, "/probe/results", token, req)
	if rresp.StatusCode != http.StatusOK {
		drain(rresp)
		t.Fatalf("status = %d, want 200", rresp.StatusCode)
	}
	var out uptime.ResultsResponse
	decodeJSON(t, rresp, &out)
	if out.Accepted != 1 || out.Rejected != 0 {
		t.Fatalf("results = %+v, want accepted 1 rejected 0", out)
	}

	pending, err := s.uptime.PendingCount(ctx)
	if err != nil {
		t.Fatalf("PendingCount: %v", err)
	}
	if pending != 0 {
		t.Fatalf("PendingCount() = %d, want 0 (job must be completed)", pending)
	}

	states, err := s.uptime.States(ctx, created.ID)
	if err != nil {
		t.Fatalf("States: %v", err)
	}
	if len(states) != 1 || states[0].Status != "down" || states[0].Region != "eu-west" {
		t.Fatalf("states = %+v, want single down state in eu-west", states)
	}

	select {
	case st := <-s.results:
		if st.Status != "down" {
			t.Fatalf("OnResult state = %+v, want down", st)
		}
	default:
		t.Fatal("OnResult (detector) was not called")
	}
}

func TestProbeResultsRejectsJobLeasedByAnotherProbe(t *testing.T) {
	s := newProbeStack(t)
	ctx := context.Background()
	orgID, pid := newOrgProject(t, s.pool)

	_, tokenA, err := s.uptime.CreateProbe(ctx, orgID, "eu-west", "Probe A")
	if err != nil {
		t.Fatalf("CreateProbe A: %v", err)
	}
	_, tokenB, err := s.uptime.CreateProbe(ctx, orgID, "eu-west", "Probe B")
	if err != nil {
		t.Fatalf("CreateProbe B: %v", err)
	}
	if _, err := s.uptime.Create(ctx, probeHTTPMonitor(t, pid), []string{"eu-west"}, nil); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := s.uptime.Schedule(ctx); err != nil {
		t.Fatalf("Schedule: %v", err)
	}

	resp := probePost(t, s, "/probe/lease", tokenA, nil)
	var lease uptime.LeaseResponse
	decodeJSON(t, resp, &lease)
	if len(lease.Jobs) != 1 {
		t.Fatalf("lease.Jobs = %d, want 1", len(lease.Jobs))
	}

	// Проба B шлёт результат по заданию пробы A — центр не верит пробе.
	rresp := probePost(t, s, "/probe/results", tokenB, uptime.ResultsRequest{
		Results: []uptime.ResultDTO{{QueueID: lease.Jobs[0].QueueID, OK: true}},
	})
	if rresp.StatusCode != http.StatusOK {
		drain(rresp)
		t.Fatalf("status = %d, want 200", rresp.StatusCode)
	}
	var out uptime.ResultsResponse
	decodeJSON(t, rresp, &out)
	if out.Accepted != 0 || out.Rejected != 1 {
		t.Fatalf("results = %+v, want accepted 0 rejected 1", out)
	}

	pending, err := s.uptime.PendingCount(ctx)
	if err != nil {
		t.Fatalf("PendingCount: %v", err)
	}
	if pending != 1 {
		t.Fatalf("PendingCount() = %d, want 1 (foreign result must not complete the job)", pending)
	}
}

func TestProbeResultsRejectsExpiredLease(t *testing.T) {
	s := newProbeStack(t)
	ctx := context.Background()
	orgID, pid := newOrgProject(t, s.pool)

	_, token, err := s.uptime.CreateProbe(ctx, orgID, "eu-west", "Probe 1")
	if err != nil {
		t.Fatalf("CreateProbe: %v", err)
	}
	if _, err := s.uptime.Create(ctx, probeHTTPMonitor(t, pid), []string{"eu-west"}, nil); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := s.uptime.Schedule(ctx); err != nil {
		t.Fatalf("Schedule: %v", err)
	}

	resp := probePost(t, s, "/probe/lease", token, nil)
	var lease uptime.LeaseResponse
	decodeJSON(t, resp, &lease)
	if len(lease.Jobs) != 1 {
		t.Fatalf("lease.Jobs = %d, want 1", len(lease.Jobs))
	}
	queueID := lease.Jobs[0].QueueID

	if _, err := s.pool.Exec(ctx,
		"UPDATE check_queue SET lease_until = now() - interval '1 minute' WHERE id = $1", queueID); err != nil {
		t.Fatalf("expire lease: %v", err)
	}

	rresp := probePost(t, s, "/probe/results", token, uptime.ResultsRequest{
		Results: []uptime.ResultDTO{{QueueID: queueID, OK: true}},
	})
	if rresp.StatusCode != http.StatusOK {
		drain(rresp)
		t.Fatalf("status = %d, want 200", rresp.StatusCode)
	}
	var out uptime.ResultsResponse
	decodeJSON(t, rresp, &out)
	if out.Accepted != 0 || out.Rejected != 1 {
		t.Fatalf("results = %+v, want accepted 0 rejected 1 (lease expired)", out)
	}

	pending, err := s.uptime.PendingCount(ctx)
	if err != nil {
		t.Fatalf("PendingCount: %v", err)
	}
	if pending != 1 {
		t.Fatalf("PendingCount() = %d, want 1 (expired-lease result must not complete the job)", pending)
	}
}

func TestProbeResultsTooManyResultsReturns400(t *testing.T) {
	s := newProbeStack(t)
	ctx := context.Background()
	orgID, _ := newOrgProject(t, s.pool)

	_, token, err := s.uptime.CreateProbe(ctx, orgID, "eu-west", "Probe 1")
	if err != nil {
		t.Fatalf("CreateProbe: %v", err)
	}

	results := make([]uptime.ResultDTO, 101)
	for i := range results {
		results[i] = uptime.ResultDTO{QueueID: int64(i + 1), OK: true}
	}
	resp := probePost(t, s, "/probe/results", token, uptime.ResultsRequest{Results: results})
	drain(resp)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (over the 100-result cap)", resp.StatusCode)
	}
}

func TestProbeResultsWithoutTokenReturns401(t *testing.T) {
	s := newProbeStack(t)

	resp := probePost(t, s, "/probe/results", "", uptime.ResultsRequest{
		Results: []uptime.ResultDTO{{QueueID: 1, OK: true}},
	})
	drain(resp)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}
