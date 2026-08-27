package web_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/host"
	"gitflic.ru/otezvikentiy/gotcha/internal/metric"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/profile"
	"gitflic.ru/otezvikentiy/gotcha/internal/slo"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"gitflic.ru/otezvikentiy/gotcha/internal/trace"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
	"gitflic.ru/otezvikentiy/gotcha/internal/web"
)

// ackStack — стенд ack-эндпоинта (B4, T10): только PG, все 5 инцидент-
// сторов проведены на одном Handler — incidentAck диспатчит на них по
// {source}, не нуждаясь в ClickHouse (в отличие от hostsStack, которому CH
// нужен ради Metrics на самой карточке хоста).
type ackStack struct {
	pool    *pgxpool.Pool
	srv     *httptest.Server
	h       *web.Handler
	org     *org.Service
	auth    *auth.Service
	hosts   *host.Store
	hostInc *host.IncidentService
	metInc  *metric.IncidentService
	traceR  *trace.RegressionService
	profR   *profile.RegressionService
	sloSt   *slo.Store
	uptimeS *uptime.Service
}

// seedMonitor — заводит монитор напрямую SQL (не через uptime.Service.Create):
// ack-стенду не нужны ни валидные HTTP/DNS/TCP-настройки, ни шифрование
// заголовков — только строка в monitors, на которую можно повесить инцидент
// через uptime.Service.OpenIncident (FK monitor_id).
func (s *ackStack) seedMonitor(t *testing.T, projectID int64, name string) int64 {
	t.Helper()
	var id int64
	if err := s.pool.QueryRow(context.Background(),
		`INSERT INTO monitors (project_id, name, kind, interval_seconds) VALUES ($1,$2,'heartbeat',60) RETURNING id`,
		projectID, name).Scan(&id); err != nil {
		t.Fatalf("seed monitor: %v", err)
	}
	return id
}

func newAckStack(t *testing.T) *ackStack {
	t.Helper()
	pool := testenv.MigratedPG(t)
	authSvc := auth.NewService(pool)
	orgSvc := org.NewService(pool, 1_000_000)

	mux := http.NewServeMux()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)

	h := web.New(authSvc, orgSvc, nil, nil, srv.URL)
	hostsStore := host.NewStore(pool)
	hostInc := host.NewIncidentService(pool)
	metInc := metric.NewIncidentService(pool)
	traceR := trace.NewRegressionService(pool)
	profR := profile.NewRegressionService(pool)
	sloSt := slo.NewStore(pool)
	uptimeS := uptime.NewService(pool)
	h.Hosts = hostsStore
	h.HostIncidents = hostInc
	h.MetricIncidents = metInc
	h.Regressions = traceR
	h.ProfileRegressions = profR
	h.SLO = sloSt
	h.Uptime = uptimeS
	h.Register(mux)

	return &ackStack{pool: pool, srv: srv, h: h, org: orgSvc, auth: authSvc,
		hosts: hostsStore, hostInc: hostInc, metInc: metInc, traceR: traceR, profR: profR, sloSt: sloSt, uptimeS: uptimeS}
}

// seedHost — заводит хост через host.Store.Upsert (как остальные web-тесты
// хостов), а не руками через INSERT — не завязываемся на набор колонок
// таблицы hosts помимо контракта Store.
func (s *ackStack) seedHost(t *testing.T, projectID int64, name string) int64 {
	t.Helper()
	ctx := context.Background()
	if _, err := s.hosts.Upsert(ctx, projectID, []host.TouchEntry{{Name: name}}); err != nil {
		t.Fatalf("upsert host: %v", err)
	}
	hst, found, err := s.hosts.Get(ctx, projectID, name)
	if err != nil || !found {
		t.Fatalf("get host: found=%v err=%v", found, err)
	}
	return hst.ID
}

func ackPath(projectID int64, source string, incidentID int64) string {
	return "/projects/" + strconv.FormatInt(projectID, 10) +
		"/incidents/" + source + "/" + strconv.FormatInt(incidentID, 10) + "/ack"
}

// TestWebIncidentAckDispatch — один POST на каждый из 5 источников
// подтверждает СВОЙ инцидент (диспатч по {source} на верный стор) и
// редиректит 303 на Referer.
func TestWebIncidentAckDispatch(t *testing.T) {
	s := newAckStack(t)
	ctx := context.Background()

	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, "ack-owner@example.com")
	o, err := s.org.CreateOrg(ctx, "ack-co", "Ack Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	project, err := s.org.CreateProject(ctx, o.ID, "ack-proj", "Ack Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	// host
	hostID := s.seedHost(t, project.ID, "h1")
	hostInc, _, err := s.hostInc.Open(ctx, project.ID, hostID, "disk", 0.95, "/var", false)
	if err != nil {
		t.Fatalf("open host incident: %v", err)
	}

	// metric
	rule, err := metric.NewRuleService(s.pool).Create(ctx, metric.Rule{
		ProjectID: project.ID, MetricName: "cpu", Aggregation: "avg", Comparator: "gt",
		Threshold: 90, WindowSeconds: 300, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create rule: %v", err)
	}
	metInc, _, err := s.metInc.Open(ctx, rule.ID, project.ID, 95, false, "")
	if err != nil {
		t.Fatalf("open metric incident: %v", err)
	}

	// trace
	traceReg, _, err := s.traceR.Open(ctx, project.ID, "endpoint_p95", "GET /x", "duration", 100, 300, false)
	if err != nil {
		t.Fatalf("open trace regression: %v", err)
	}

	// profile
	profReg, _, err := s.profR.Open(ctx, project.ID, "api", "cpu", "hot()", 0.1, 0.3, false)
	if err != nil {
		t.Fatalf("open profile regression: %v", err)
	}

	// slo
	sloDef, err := s.sloSt.Create(ctx, slo.SLO{
		ProjectID: project.ID, Name: "ack-slo", Kind: slo.SLIAvailability, Target: 0.99,
		WindowDays: 30, BurnThreshold: 14.4, BurnLongMin: 60, BurnShortMin: 5, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create slo: %v", err)
	}
	rem := 0.5
	sloInc, _, err := s.sloSt.OpenIncident(ctx, sloDef.ID, project.ID, 20.0, &rem, false)
	if err != nil {
		t.Fatalf("open slo incident: %v", err)
	}

	// uptime (W2-C находка 2)
	monID := s.seedMonitor(t, project.ID, "m1")
	uptimeInc, _, err := s.uptimeS.OpenIncident(ctx, monID, "connection refused", []string{"eu"}, false)
	if err != nil {
		t.Fatalf("open uptime incident: %v", err)
	}

	cases := []struct {
		source string
		id     int64
	}{
		{"host", hostInc.ID},
		{"metric", metInc.ID},
		{"trace", traceReg.ID},
		{"profile", profReg.ID},
		{"slo", sloInc.ID},
		{"uptime", uptimeInc.ID},
	}
	for _, c := range cases {
		form := url.Values{}
		resp := postForm(t, s.srv, ackPath(project.ID, c.source, c.id), form, s.srv.URL, ownerCookie)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("ack %s: status = %d, want 303: %s", c.source, resp.StatusCode, body)
		}
	}

	if got, _, err := s.hostInc.GetByID(ctx, hostInc.ID); err != nil || got.AcknowledgedAt == nil {
		t.Errorf("host incident not acked: %+v err=%v", got, err)
	}
	if got, _, err := s.metInc.GetByID(ctx, metInc.ID); err != nil || got.AcknowledgedAt == nil {
		t.Errorf("metric incident not acked: %+v err=%v", got, err)
	}
	if got, _, err := s.traceR.GetByID(ctx, traceReg.ID); err != nil || got.AcknowledgedAt == nil {
		t.Errorf("trace regression not acked: %+v err=%v", got, err)
	}
	if got, _, err := s.profR.GetByID(ctx, profReg.ID); err != nil || got.AcknowledgedAt == nil {
		t.Errorf("profile regression not acked: %+v err=%v", got, err)
	}
	if got, _, err := s.sloSt.GetIncidentByID(ctx, sloInc.ID); err != nil || got.AcknowledgedAt == nil {
		t.Errorf("slo incident not acked: %+v err=%v", got, err)
	}
	if got, _, err := s.uptimeS.IncidentByID(ctx, uptimeInc.ID); err != nil || got.AcknowledgedAt == nil {
		t.Errorf("uptime incident not acked: %+v err=%v", got, err)
	}

	// Повторный ack — идемпотентно, тот же редирект, без ошибки (T10 §1).
	resp := postForm(t, s.srv, ackPath(project.ID, "host", hostInc.ID), url.Values{}, s.srv.URL, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("повторный ack: status = %d, want 303: %s", resp.StatusCode, body)
	}
}

// TestWebIncidentAckUnknownSource — {source} вне пяти известных → 404.
func TestWebIncidentAckUnknownSource(t *testing.T) {
	s := newAckStack(t)
	ctx := context.Background()
	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, "ack-unknown@example.com")
	o, err := s.org.CreateOrg(ctx, "ack-unk-co", "Ack Unk Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	project, err := s.org.CreateProject(ctx, o.ID, "ack-unk-proj", "Ack Unk Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	resp := postForm(t, s.srv, ackPath(project.ID, "bogus", 1), url.Values{}, s.srv.URL, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("bogus source: status = %d, want 404: %s", resp.StatusCode, body)
	}
}

// TestWebIncidentAckCrossOrigin — POST без совпадающего Origin → 403, а не
// диспатч на стор (sameOrigin — первая проверка хендлера).
func TestWebIncidentAckCrossOrigin(t *testing.T) {
	s := newAckStack(t)
	ctx := context.Background()
	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, "ack-xorigin@example.com")
	o, err := s.org.CreateOrg(ctx, "ack-xo-co", "Ack XO Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	project, err := s.org.CreateProject(ctx, o.ID, "ack-xo-proj", "Ack XO Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	hostID := s.seedHost(t, project.ID, "h1")
	inc, _, err := s.hostInc.Open(ctx, project.ID, hostID, "disk", 0.95, "/var", false)
	if err != nil {
		t.Fatalf("open incident: %v", err)
	}

	resp := postForm(t, s.srv, ackPath(project.ID, "host", inc.ID), url.Values{}, "https://evil.example", ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin ack: status = %d, want 403: %s", resp.StatusCode, body)
	}
	if got, _, err := s.hostInc.GetByID(ctx, inc.ID); err != nil || got.AcknowledgedAt != nil {
		t.Errorf("cross-origin ack acked the incident anyway: %+v err=%v", got, err)
	}
}

// TestWebIncidentAckCrossTenant — оператор проекта B не подтверждает
// инцидент проекта A подобранным id (project_id в WHERE Acknowledge,
// defense-in-depth, зеркало uptime.DeleteWindow B3): путь несёт {id}=B,
// инцидент принадлежит A → Acknowledge WHERE project_id=B не найдёт строку,
// ok=false, редирект успешный (идемпотентно), но инцидент A остаётся
// неподтверждённым.
func TestWebIncidentAckCrossTenant(t *testing.T) {
	s := newAckStack(t)
	ctx := context.Background()

	ownerAID, _ := orgSettingsRegister(t, s.auth, "ack-tenant-a@example.com")
	orgA, err := s.org.CreateOrg(ctx, "ack-tenant-a-co", "Ack Tenant A Co", ownerAID)
	if err != nil {
		t.Fatalf("create org A: %v", err)
	}
	projectA, err := s.org.CreateProject(ctx, orgA.ID, "ack-tenant-a-proj", "Ack Tenant A Proj", "go")
	if err != nil {
		t.Fatalf("create project A: %v", err)
	}
	hostID := s.seedHost(t, projectA.ID, "h1")
	inc, _, err := s.hostInc.Open(ctx, projectA.ID, hostID, "disk", 0.95, "/var", false)
	if err != nil {
		t.Fatalf("open incident: %v", err)
	}

	ownerBID, ownerBCookie := orgSettingsRegister(t, s.auth, "ack-tenant-b@example.com")
	orgB, err := s.org.CreateOrg(ctx, "ack-tenant-b-co", "Ack Tenant B Co", ownerBID)
	if err != nil {
		t.Fatalf("create org B: %v", err)
	}
	projectB, err := s.org.CreateProject(ctx, orgB.ID, "ack-tenant-b-proj", "Ack Tenant B Proj", "go")
	if err != nil {
		t.Fatalf("create project B: %v", err)
	}

	// Оператор B, путь несёт {id}=B, incident_id — инцидент проекта A.
	resp := postForm(t, s.srv, ackPath(projectB.ID, "host", inc.ID), url.Values{}, s.srv.URL, ownerBCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	// Оператор B авторизован на своём проекте B — гейт requireProjectOperator
	// пропускает (это его проект), а cross-tenant отсекает Acknowledge внутри
	// (project_id в WHERE) — идемпотентный редирект, не ошибка.
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("cross-tenant ack: status = %d, want 303: %s", resp.StatusCode, body)
	}
	if got, _, err := s.hostInc.GetByID(ctx, inc.ID); err != nil || got.AcknowledgedAt != nil {
		t.Errorf("cross-tenant ack acked project A's incident via project B's path: %+v err=%v", got, err)
	}
}
