package web_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/event"
	"gitflic.ru/otezvikentiy/gotcha/internal/issue"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/profile"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"gitflic.ru/otezvikentiy/gotcha/internal/trace"
	"gitflic.ru/otezvikentiy/gotcha/internal/web"
)

type traceStack struct {
	pool    *pgxpool.Pool
	ch      driver.Conn
	srv     *httptest.Server
	org     *org.Service
	auth    *auth.Service
	issues  *issue.Service
	spans   *trace.SpanWriter
	batcher *event.Batcher
	// нужен тестам, которые настраивают SpanRetentionDays явно, вместо дефолта.
	h *web.Handler
}

func newTraceStack(t *testing.T) *traceStack {
	t.Helper()
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)

	authSvc := auth.NewService(pool)
	orgSvc := org.NewService(pool, 1_000_000)
	issueSvc := issue.NewService(pool)
	eventsQuery := event.NewQuery(ch)

	batcher := event.NewBatcher(ch)
	go batcher.Run()
	spans := trace.NewSpanWriter(ch)
	go spans.Run()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = batcher.Close(ctx)
		_ = spans.Close(ctx)
	})

	mux := http.NewServeMux()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)

	h := web.New(authSvc, orgSvc, issueSvc, eventsQuery, srv.URL)
	h.Trace = trace.NewQuery(ch)
	h.Profiles = profile.NewQuery(ch)
	h.Register(mux)

	return &traceStack{pool: pool, ch: ch, srv: srv, org: orgSvc, auth: authSvc, issues: issueSvc, spans: spans, batcher: batcher, h: h}
}

// Close идемпотентен — повторный вызов из Cleanup безопасен.
func (s *traceStack) flush(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := s.spans.Close(ctx); err != nil {
		t.Fatalf("flush spans: %v", err)
	}
	if err := s.batcher.Close(ctx); err != nil {
		t.Fatalf("flush events: %v", err)
	}
}

func TestWebTraceWaterfall(t *testing.T) {
	s := newTraceStack(t)
	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, "trace-owner@example.com")
	_, outsiderCookie := orgSettingsRegister(t, s.auth, "trace-outsider@example.com")

	o, err := s.org.CreateOrg(context.Background(), "trace-co", "Trace Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	proj, err := s.org.CreateProject(context.Background(), o.ID, "trace-proj", "Trace Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	now := time.Now().UTC()
	iss, err := s.issues.Upsert(context.Background(), proj.ID, "fp-trace", "DBError", "GET /api/checkout", "error", "", now)
	if err != nil {
		t.Fatalf("upsert issue: %v", err)
	}

	const traceID = "wf-trace-01"
	const rootSpan = "wf-span-root"
	const dbSpan = "wf-span-db"
	const httpSpan = "wf-span-http"
	start := now.Add(-5 * time.Minute)

	s.spans.Add(proj.ID, proj.ID, trace.Transaction{
		TraceID:     traceID,
		SpanID:      rootSpan,
		Name:        "GET /api/checkout",
		Op:          "http.server",
		Status:      "ok",
		Start:       start,
		End:         start.Add(300 * time.Millisecond),
		Environment: "production",
		Spans: []trace.Span{
			{
				SpanID:       dbSpan,
				ParentSpanID: rootSpan,
				Op:           "db.query",
				Description:  "SELECT * FROM orders",
				Start:        start.Add(20 * time.Millisecond),
				End:          start.Add(120 * time.Millisecond),
				Status:       "internal_error",
			},
			{
				SpanID:       httpSpan,
				ParentSpanID: rootSpan,
				Op:           "http.client",
				Description:  "POST payments",
				Start:        start.Add(150 * time.Millisecond),
				End:          start.Add(280 * time.Millisecond),
				Status:       "ok",
			},
		},
	})

	s.batcher.Add(event.Event{
		ID:        uuid.NewString(),
		ProjectID: proj.ID,
		IssueID:   iss.IssueID,
		Timestamp: start.Add(120 * time.Millisecond),
		Level:     "error",
		Message:   "db boom",
		TraceID:   traceID,
		SpanID:    dbSpan,
		Tags:      map[string]string{},
	})
	s.flush(t)

	tracePath := "/traces/" + traceID

	resp := getWithCookie(t, s.srv, tracePath, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s (owner) status = %d, want 200: %s", tracePath, resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "<svg") {
		t.Fatalf("GET %s missing <svg: %s", tracePath, body)
	}
	if n := strings.Count(string(body), "<rect"); n < 3 {
		t.Fatalf("GET %s has %d <rect, want >= 3: %s", tracePath, n, body)
	}
	issueLink := "/issues/" + strconv.FormatInt(iss.IssueID, 10)
	if !strings.Contains(string(body), issueLink) {
		t.Fatalf("GET %s missing error marker link %q: %s", tracePath, issueLink, body)
	}
	if !strings.Contains(string(body), "GET /api/checkout") {
		t.Fatalf("GET %s missing transaction name: %s", tracePath, body)
	}

	resp = getWithCookie(t, s.srv, tracePath, outsiderCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET %s (outsider) status = %d, want 404", tracePath, resp.StatusCode)
	}

	// в БД trace_id лоуркейснут — тот же id в другом регистре должен
	// резолвиться в тот же трейс, а не давать ложный 404.
	upperPath := "/traces/" + strings.ToUpper(traceID)
	resp = getWithCookie(t, s.srv, upperPath, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s (uppercase trace_id) status = %d, want 200: %s", upperPath, resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "<svg") {
		t.Fatalf("GET %s (uppercase trace_id) missing <svg: %s", upperPath, body)
	}

	resp = getWithCookie(t, s.srv, "/traces/nope-nope", ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET /traces/nope-nope status = %d, want 404", resp.StatusCode)
	}
}

func TestWebTraceWaterfallLogsLink(t *testing.T) {
	s := newTraceStack(t)
	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, "trace-logs-owner@example.com")

	o, err := s.org.CreateOrg(context.Background(), "trace-logs-co", "Trace Logs Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	proj, err := s.org.CreateProject(context.Background(), o.ID, "trace-logs-proj", "Trace Logs Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	const traceID = "logs-link-trace-01"
	start := time.Now().UTC().Add(-10 * time.Minute)
	s.spans.Add(proj.ID, proj.ID, trace.Transaction{
		TraceID:     traceID,
		SpanID:      "logs-link-root",
		Name:        "GET /api/logs-link",
		Op:          "http.server",
		Status:      "ok",
		Start:       start,
		End:         start.Add(100 * time.Millisecond),
		Environment: "production",
	})
	s.flush(t)

	tracePath := "/traces/" + traceID
	resp := getWithCookie(t, s.srv, tracePath, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200: %s", tracePath, resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "/logs?") {
		t.Fatalf("GET %s missing logs link: %s", tracePath, body)
	}
	if !strings.Contains(string(body), "trace_id="+traceID) {
		t.Fatalf("GET %s logs link missing trace_id=%s: %s", tracePath, traceID, body)
	}
	if !hasLogsLinkWindow(string(body)) {
		t.Fatalf("GET %s logs link missing start/end window: %s", tracePath, body)
	}
	// ссылка на логи не завязана на HasProfile, в отличие от ссылки на flamegraph.
	if strings.Contains(string(body), "Смотреть flamegraph") {
		t.Fatalf("GET %s must not show flamegraph link without a profile: %s", tracePath, body)
	}
}

func TestWebIssueDetailTraceLink(t *testing.T) {
	s := newTraceStack(t)
	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, "tracelink-owner@example.com")
	o, err := s.org.CreateOrg(context.Background(), "tracelink-co", "TraceLink Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	proj, err := s.org.CreateProject(context.Background(), o.ID, "tracelink-proj", "TraceLink Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	now := time.Now().UTC()
	iss, err := s.issues.Upsert(context.Background(), proj.ID, "fp-tracelink", "Boom", "svc.go:1", "error", "", now)
	if err != nil {
		t.Fatalf("upsert issue: %v", err)
	}

	const traceID = "link-trace-01"
	const unsampledTrace = "unsampled-trace-99"
	withTraceID := uuid.NewString()
	noTraceID := uuid.NewString()
	unsampledID := uuid.NewString()
	s.batcher.Add(event.Event{
		ID:        withTraceID,
		ProjectID: proj.ID,
		IssueID:   iss.IssueID,
		Timestamp: now.Add(-2 * time.Minute),
		Level:     "error",
		Message:   "with trace",
		TraceID:   traceID,
		Tags:      map[string]string{},
	})
	s.batcher.Add(event.Event{
		ID:        noTraceID,
		ProjectID: proj.ID,
		IssueID:   iss.IssueID,
		Timestamp: now.Add(-1 * time.Minute),
		Level:     "error",
		Message:   "no trace",
		Tags:      map[string]string{},
	})
	// Событие с trace_id, но БЕЗ записанной транзакции (сэмплирование): ссылки
	// быть не должно — иначе клик ведёт на 404-й трейс.
	s.batcher.Add(event.Event{
		ID:        unsampledID,
		ProjectID: proj.ID,
		IssueID:   iss.IssueID,
		Timestamp: now.Add(-3 * time.Minute),
		Level:     "error",
		Message:   "unsampled trace",
		TraceID:   unsampledTrace,
		Tags:      map[string]string{},
	})
	// Для traceID записываем транзакцию — только тогда ссылка «Смотреть трейс»
	// осмысленна (страница трейса что-то покажет), иначе traceWaterfall даёт 404.
	s.spans.Add(proj.ID, proj.ID, trace.Transaction{
		TraceID:     traceID,
		SpanID:      "root-link-span",
		Name:        "GET /linked",
		Op:          "http.server",
		Status:      "ok",
		Start:       now.Add(-2 * time.Minute),
		End:         now.Add(-2 * time.Minute).Add(50 * time.Millisecond),
		Environment: "production",
	})
	s.flush(t)

	issuePath := "/issues/" + strconv.FormatInt(iss.IssueID, 10)

	resp := getWithCookie(t, s.srv, issuePath+"?event="+withTraceID, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s?event=%s status = %d, want 200: %s", issuePath, withTraceID, resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "Смотреть трейс") {
		t.Fatalf("GET %s (event with trace) missing 'Смотреть трейс': %s", issuePath, body)
	}
	if !strings.Contains(string(body), "/traces/"+traceID) {
		t.Fatalf("GET %s (event with trace) missing trace link: %s", issuePath, body)
	}

	resp = getWithCookie(t, s.srv, issuePath+"?event="+noTraceID, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s?event=%s status = %d, want 200: %s", issuePath, noTraceID, resp.StatusCode, body)
	}
	if strings.Contains(string(body), "Смотреть трейс") {
		t.Fatalf("GET %s (event without trace) must not show 'Смотреть трейс': %s", issuePath, body)
	}

	// Событие с trace_id, но без записанной транзакции → ссылки тоже нет: клик
	// вёл бы на 404 (traceWaterfall не находит трейс в transactions).
	resp = getWithCookie(t, s.srv, issuePath+"?event="+unsampledID, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.Contains(string(body), "Смотреть трейс") {
		t.Fatalf("GET %s (trace без транзакции) must not show 'Смотреть трейс': %s", issuePath, body)
	}
}

func TestWebTraceProfilingInContext(t *testing.T) {
	s := newTraceStack(t)
	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, "pic-owner@example.com")
	_, outsiderCookie := orgSettingsRegister(t, s.auth, "pic-outsider@example.com")
	ctx := context.Background()
	o, _ := s.org.CreateOrg(ctx, "pic-co", "PIC Co", ownerID)
	proj, _ := s.org.CreateProject(ctx, o.ID, "pic-proj", "PIC Proj", "go")

	const traceID = "pic-trace-01"
	start := time.Now().UTC().Add(-2 * time.Minute)
	s.spans.Add(proj.ID, proj.ID, trace.Transaction{
		TraceID: traceID, SpanID: "pic-root", Name: "GET /pic", Op: "http.server",
		Status: "ok", Start: start, End: start.Add(100 * time.Millisecond), Environment: "prod",
	})
	s.flush(t)

	resp := getWithCookie(t, s.srv, "/traces/"+traceID, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.Contains(string(body), "Смотреть flamegraph") {
		t.Fatalf("flamegraph link shown without a profile")
	}

	resp = getWithCookie(t, s.srv, "/traces/"+traceID+"/flame", ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "нет данных профиля") {
		t.Fatalf("flame without profile status=%d body=%s", resp.StatusCode, body)
	}
	if strings.Contains(string(body), "Клик по сегменту") {
		t.Fatalf("zoom hint must not be shown under the empty placeholder: %s", body)
	}

	if err := s.ch.Exec(ctx, `INSERT INTO profile_samples
		(project_id,profile_type,service,environment,transaction,platform,ts,stack,value,trace_id)
		VALUES (?,'cpu','api','prod','GET /pic','go',?,?,?,?)`,
		proj.ID, start, []string{"root", "handler"}, uint64(10), traceID); err != nil {
		t.Fatalf("seed profile: %v", err)
	}

	resp = getWithCookie(t, s.srv, "/traces/"+traceID, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "Смотреть flamegraph") {
		t.Fatalf("flamegraph link missing with a profile: %s", body)
	}

	resp = getWithCookie(t, s.srv, "/traces/"+traceID+"/flame", ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "<svg") {
		t.Fatalf("flame page status=%d body=%s", resp.StatusCode, body)
	}
	if strings.Contains(string(body), "flame-ancestor") || !strings.Contains(string(body), "Клик по сегменту") {
		t.Fatalf("flame without focus must render the root without ancestors and with the hint: %s", body)
	}

	resp = getWithCookie(t, s.srv, "/traces/"+traceID+"/flame?focus=root&focus=handler", ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || strings.Count(string(body), "flame-ancestor") != 2 {
		t.Fatalf("focused flame status=%d, want 200 with two ancestor rows: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), `href="/traces/`+traceID+`/flame"><svg class="flame-ancestor"`) {
		t.Fatalf("root row must link to the flame page without focus: %s", body)
	}

	resp = getWithCookie(t, s.srv, "/traces/"+traceID+"/flame", outsiderCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("outsider flame status = %d, want 404", resp.StatusCode)
	}
}

func TestWebTraceCrossOrgStranger(t *testing.T) {
	s := newTraceStack(t)

	victimID, victimCookie := orgSettingsRegister(t, s.auth, "trace-victim@example.com")
	victimOrg, err := s.org.CreateOrg(context.Background(), "trace-victim-co", "Trace Victim Co", victimID)
	if err != nil {
		t.Fatalf("create victim org: %v", err)
	}
	proj, err := s.org.CreateProject(context.Background(), victimOrg.ID, "trace-victim-proj", "Trace Victim Proj", "go")
	if err != nil {
		t.Fatalf("create victim project: %v", err)
	}

	attackerID, attackerCookie := orgSettingsRegister(t, s.auth, "trace-attacker@example.com")
	if _, err := s.org.CreateOrg(context.Background(), "trace-attacker-co", "Trace Attacker Co", attackerID); err != nil {
		t.Fatalf("create attacker org: %v", err)
	}

	const traceID = "xorg-trace-01"
	start := time.Now().UTC().Add(-5 * time.Minute)
	s.spans.Add(proj.ID, proj.ID, trace.Transaction{
		TraceID:     traceID,
		SpanID:      "xorg-span-root",
		Name:        "GET /api/secret",
		Op:          "http.server",
		Status:      "ok",
		Start:       start,
		End:         start.Add(100 * time.Millisecond),
		Environment: "production",
	})
	s.flush(t)

	// санити: иначе 404 ниже был бы ложноположительным («не нашли, потому что не завели»).
	resp := getWithCookie(t, s.srv, "/traces/"+traceID, victimCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("victim owner trace status = %d, want 200 (трейс должен существовать)", resp.StatusCode)
	}

	for _, path := range []string{"/traces/" + traceID, "/traces/" + traceID + "/flame", "/traces/xorg-nope"} {
		resp := getWithCookie(t, s.srv, path, attackerCookie)
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("stranger GET %s status = %d, want 404", path, resp.StatusCode)
		}
	}
}

// Атакующий шлёт транзакцию с тем же trace_id в свой проект — резолв обязан
// найти проект жертвы среди кандидатов, а не первую строку по trace_id.
func TestWebTraceCollisionOwnerStillResolves(t *testing.T) {
	s := newTraceStack(t)

	victimID, victimCookie := orgSettingsRegister(t, s.auth, "trace-collision-victim@example.com")
	victimOrg, err := s.org.CreateOrg(context.Background(), "trace-collision-victim-co", "Trace Collision Victim Co", victimID)
	if err != nil {
		t.Fatalf("create victim org: %v", err)
	}
	victimProj, err := s.org.CreateProject(context.Background(), victimOrg.ID, "trace-collision-victim-proj", "Trace Collision Victim Proj", "go")
	if err != nil {
		t.Fatalf("create victim project: %v", err)
	}

	attackerID, _ := orgSettingsRegister(t, s.auth, "trace-collision-attacker@example.com")
	attackerOrg, err := s.org.CreateOrg(context.Background(), "trace-collision-attacker-co", "Trace Collision Attacker Co", attackerID)
	if err != nil {
		t.Fatalf("create attacker org: %v", err)
	}
	attackerProj, err := s.org.CreateProject(context.Background(), attackerOrg.ID, "trace-collision-attacker-proj", "Trace Collision Attacker Proj", "go")
	if err != nil {
		t.Fatalf("create attacker project: %v", err)
	}

	const traceID = "collision-trace-01"
	start := time.Now().UTC().Add(-5 * time.Minute)
	s.spans.Add(victimProj.ID, victimProj.ID, trace.Transaction{
		TraceID: traceID, SpanID: "victim-root", Name: "GET /api/mine", Op: "http.server",
		Status: "ok", Start: start, End: start.Add(100 * time.Millisecond), Environment: "production",
	})
	// та же строка id — не совпадение, а наведённая атакующим коллизия.
	s.spans.Add(attackerProj.ID, attackerProj.ID, trace.Transaction{
		TraceID: traceID, SpanID: "attacker-root", Name: "GET /api/other", Op: "http.server",
		Status: "ok", Start: start, End: start.Add(50 * time.Millisecond), Environment: "production",
	})
	s.flush(t)

	for _, path := range []string{"/traces/" + traceID, "/traces/" + traceID + "/flame"} {
		resp := getWithCookie(t, s.srv, path, victimCookie)
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("victim GET %s status = %d, want 200 (коллизия в чужом проекте не должна прятать свой трейс)", path, resp.StatusCode)
		}
	}
}

// Пользователю доступны ОБА проекта (два своих проекта в одной организации) —
// выбор не произволен: показывается проект с самой свежей транзакцией по этому
// trace_id, а не первая строка в порядке хранения ClickHouse.
func TestWebTraceCollisionBothAccessibleShowsMostRecent(t *testing.T) {
	s := newTraceStack(t)

	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, "trace-collision-owner@example.com")
	ownerOrg, err := s.org.CreateOrg(context.Background(), "trace-collision-owner-co", "Trace Collision Owner Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	olderProj, err := s.org.CreateProject(context.Background(), ownerOrg.ID, "trace-collision-older-proj", "Trace Collision Older Proj", "go")
	if err != nil {
		t.Fatalf("create older project: %v", err)
	}
	recentProj, err := s.org.CreateProject(context.Background(), ownerOrg.ID, "trace-collision-recent-proj", "Trace Collision Recent Proj", "go")
	if err != nil {
		t.Fatalf("create recent project: %v", err)
	}

	const traceID = "collision-trace-02"
	older := time.Now().UTC().Add(-2 * time.Hour)
	recent := time.Now().UTC().Add(-5 * time.Minute)
	s.spans.Add(olderProj.ID, olderProj.ID, trace.Transaction{
		TraceID: traceID, SpanID: "older-root", Name: "GET /older-project", Op: "http.server",
		Status: "ok", Start: older, End: older.Add(100 * time.Millisecond), Environment: "production",
	})
	s.spans.Add(recentProj.ID, recentProj.ID, trace.Transaction{
		TraceID: traceID, SpanID: "recent-root", Name: "GET /recent-project", Op: "http.server",
		Status: "ok", Start: recent, End: recent.Add(100 * time.Millisecond), Environment: "production",
	})
	s.flush(t)

	resp := getWithCookie(t, s.srv, "/traces/"+traceID, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "GET /recent-project") {
		t.Fatalf("страница не показывает более свежий проект (GET /recent-project):\n%s", body)
	}
	if strings.Contains(string(body), "GET /older-project") {
		t.Fatalf("страница показала более старый проект вместо свежего:\n%s", body)
	}
}

func TestWebTraceWaterfallExpiredSpans(t *testing.T) {
	s := newTraceStack(t)
	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, "trace-expired-owner@example.com")
	_, outsiderCookie := orgSettingsRegister(t, s.auth, "trace-expired-outsider@example.com")

	o, err := s.org.CreateOrg(context.Background(), "trace-expired-co", "Trace Expired Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	proj, err := s.org.CreateProject(context.Background(), o.ID, "trace-expired-proj", "Trace Expired Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	const traceID = "expired-trace-01"
	start := time.Now().UTC().Add(-60 * 24 * time.Hour)
	s.spans.Add(proj.ID, proj.ID, trace.Transaction{
		TraceID:     traceID,
		SpanID:      "expired-root",
		Name:        "GET /api/old",
		Op:          "http.server",
		Status:      "ok",
		Start:       start,
		End:         start.Add(100 * time.Millisecond),
		Environment: "production",
	})
	s.flush(t)

	// реальный TTL ждать дни нельзя — спаны трейса удаляем синхронной мутацией.
	if err := s.ch.Exec(context.Background(),
		"ALTER TABLE spans DELETE WHERE trace_id = ? SETTINGS mutations_sync = 2", traceID); err != nil {
		t.Fatalf("simulate span TTL: %v", err)
	}

	tracePath := "/traces/" + traceID

	// проверяем именно склонённую форму (i18n.Tn), а не «30 дней» буквально.
	s.h.SpanRetentionDays = 30
	resp := getWithCookie(t, s.srv, tracePath, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET %s status = %d, want 404: %s", tracePath, resp.StatusCode, body)
	}
	if !strings.Contains(string(body), traceID) {
		t.Fatalf("GET %s missing trace_id in expired state: %s", tracePath, body)
	}
	if !strings.Contains(string(body), "хранятся 30 дней") {
		t.Fatalf("GET %s missing the configured-retention wording (h.SpanRetentionDays=30): %s", tracePath, body)
	}
	if strings.Contains(string(body), "Страница не найдена") {
		t.Fatalf("GET %s must not fall back to the generic 404 page: %s", tracePath, body)
	}

	// при SpanRetentionDays=0 текст говорит «удалены вручную», не «истёк TTL».
	s.h.SpanRetentionDays = 0
	resp = getWithCookie(t, s.srv, tracePath, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET %s (retention=0) status = %d, want 404: %s", tracePath, resp.StatusCode, body)
	}
	if strings.Contains(string(body), "хранятся") {
		t.Fatalf("GET %s (retention=0) must not claim a retention period: %s", tracePath, body)
	}
	if !strings.Contains(string(body), "были удалены") {
		t.Fatalf("GET %s (retention=0) missing the purge wording: %s", tracePath, body)
	}

	s.h.SpanRetentionDays = 30

	resp = getWithCookie(t, s.srv, tracePath, outsiderCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET %s (outsider) status = %d, want 404", tracePath, resp.StatusCode)
	}

	// обычный notFound, не trace.expired: не путаем «нет спанов» с «нет трейса».
	resp = getWithCookie(t, s.srv, "/traces/never-existed", ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET /traces/never-existed status = %d, want 404", resp.StatusCode)
	}
	if strings.Contains(string(body), "хранятся") || strings.Contains(string(body), "были удалены") {
		t.Fatalf("GET /traces/never-existed must show the generic 404, not trace.expired: %s", body)
	}
}

// транзакция свежая (далеко внутри окна хранения спанов), но её спаны пропали —
// это потеря на буфере писателя, не срок хранения; экран не должен путать их.
func TestWebTraceWaterfallSpansLostNotExpired(t *testing.T) {
	s := newTraceStack(t)
	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, "trace-lost-owner@example.com")

	o, err := s.org.CreateOrg(context.Background(), "trace-lost-co", "Trace Lost Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	proj, err := s.org.CreateProject(context.Background(), o.ID, "trace-lost-proj", "Trace Lost Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	const traceID = "lost-trace-01"
	start := time.Now().UTC().Add(-time.Hour)
	s.spans.Add(proj.ID, proj.ID, trace.Transaction{
		TraceID:     traceID,
		SpanID:      "lost-root",
		Name:        "GET /api/fresh",
		Op:          "http.server",
		Status:      "ok",
		Start:       start,
		End:         start.Add(50 * time.Millisecond),
		Environment: "production",
	})
	s.flush(t)

	// реальный дроп на буфере воспроизводить недетерминированной гонкой не
	// станем — имитируем его результат: у свежей транзакции нет ни одного спана.
	if err := s.ch.Exec(context.Background(),
		"ALTER TABLE spans DELETE WHERE trace_id = ? SETTINGS mutations_sync = 2", traceID); err != nil {
		t.Fatalf("simulate buffer loss: %v", err)
	}

	tracePath := "/traces/" + traceID

	s.h.SpanRetentionDays = 30
	resp := getWithCookie(t, s.srv, tracePath, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET %s status = %d, want 404: %s", tracePath, resp.StatusCode, body)
	}
	if strings.Contains(string(body), "хранятся") || strings.Contains(string(body), "были удалены") {
		t.Fatalf("GET %s must not blame retention/purge for a fresh transaction: %s", tracePath, body)
	}
	if !strings.Contains(string(body), "не доехали") {
		t.Fatalf("GET %s missing the buffer-loss wording: %s", tracePath, body)
	}
}
