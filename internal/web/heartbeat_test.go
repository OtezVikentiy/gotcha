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
	h      *web.Handler
}

func newUptimeStack(t *testing.T) *uptimeStack {
	t.Helper()
	return newUptimeStackInRegion(t, "")
}

// newUptimeStackInRegion — стенд с явно заданным именем встроенного региона
// (GOTCHA_UPTIME_LOCAL_REGION в проде): его получают и Handler, и uptime.Service, ровно
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

	return &uptimeStack{pool: pool, srv: srv, uptime: uptimeSvc, writer: writer, h: h}
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

// TestHeartbeatPrefetchHeaderStillCapsOversizedBody — регресс на находку
// ревью T9: отсев префетча/превью раньше стоял ПЕРЕД капом тела
// (http.MaxBytesReader), а заголовки Sec-Purpose/Purpose/X-Purpose/X-Moz и
// User-Agent — то, что клиент заявляет о себе сам, подделать их тривиально.
// Итог был: любой аноним, добавив один такой заголовок, заливал
// неограниченное тело в публичный неаутентифицированный
// POST /uptime/hb/{token} — до БД запрос всё равно не доходил, но кап,
// объявленный десятью строками выше как обязательный для ВСЕХ запросов,
// переставал действовать именно для помеченных как «отсев». Тело больше
// heartbeatMaxBodyBytes с Sec-Purpose: prefetch обязано быть отвергнуто по
// размеру (413), а не прочитано целиком с последующим 204.
func TestHeartbeatPrefetchHeaderStillCapsOversizedBody(t *testing.T) {
	s := newUptimeStack(t)
	pid := newProject(t, s.pool)
	created := newHeartbeatMonitor(t, s, pid)
	before := web.HeartbeatIgnoredBy(web.HeartbeatIgnorePrefetchHeader)

	body := bytes.Repeat([]byte("x"), 2<<10) // 2 KB, over the 1 KB cap
	req, err := http.NewRequest(http.MethodPost, s.srv.URL+"/uptime/hb/"+created.HeartbeatToken, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Sec-Purpose", "prefetch")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413 — тело поверх лимита с Sec-Purpose: prefetch обязано быть отвергнуто капом ДО ветвления на игнор, а не пройти как 204", resp.StatusCode)
	}

	got, err := s.uptime.Get(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.LastBeatAt != nil {
		t.Fatalf("монитор не отмечен живым: LastBeatAt = %v, want nil", got.LastBeatAt)
	}

	// Запрос отвергнут капом раньше ветвления на игнор — счётчик отсева расти
	// не должен: это не «мы распознали и вежливо проигнорировали префетч», а
	// «мы вообще не добрались до этой классификации».
	if got := web.HeartbeatIgnoredBy(web.HeartbeatIgnorePrefetchHeader); got != before {
		t.Errorf("HeartbeatIgnoredBy(prefetch_header) = %d, want %d — счётчик игнора расти не должен, запрос отвергнут раньше классификации", got, before)
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

// TestHeartbeatFeedsDetector фиксирует P0: успешный пинг применял результат к
// состоянию, но НЕ звал детектор. Инцидент по heartbeat открывает watchdog
// (пропущенный удар), а закрыть его больше некому — у heartbeat нет ни очереди
// заданий, ни пробы, этот эндпойнт и есть единственный сигнал «жив». Без
// вызова монитор в UI зеленел, а инцидент оставался открытым навсегда: ни
// уведомления о восстановлении, ни конца напоминаниям «всё ещё DOWN».
func TestHeartbeatFeedsDetector(t *testing.T) {
	s := newUptimeStack(t)
	pid := newProject(t, s.pool)

	created, err := s.uptime.Create(context.Background(), uptime.Monitor{
		ProjectID: pid, Name: "Cron job", Kind: uptime.KindHeartbeat, Enabled: true,
		IntervalSeconds: 60, TimeoutSeconds: 10, FailThreshold: 3, RecoveryThreshold: 1,
		Consensus: uptime.ConsensusMajority, SSLAlertDays: 14,
		Config: heartbeatConfigJSON(t, uptime.HeartbeatConfig{GraceSeconds: 60}),
	}, []string{"local"}, nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	type call struct {
		region string
		ok     bool
		status string
	}
	var got []call
	s.h.UptimeIngestor = &uptime.Ingestor{
		OnResult: func(_ context.Context, m uptime.Monitor, region string, r uptime.Result, st uptime.State) {
			got = append(got, call{region: region, ok: r.OK, status: st.Status})
		},
	}

	resp, err := http.Post(s.srv.URL+"/uptime/hb/"+created.HeartbeatToken, "", nil)
	if err != nil {
		t.Fatalf("POST heartbeat: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	if len(got) != 1 {
		t.Fatalf("OnResult вызван %d раз, want 1 — без него инцидент heartbeat не закроется никогда", len(got))
	}
	if !got[0].ok || got[0].status != "up" {
		t.Fatalf("OnResult получил %+v, want ok=true status=up", got[0])
	}
}

// newHeartbeatMonitor создаёт heartbeat-монитор с настройками, идентичными
// остальным тестам этого файла — общий хелпер для тестов отсева
// префетча/предпросмотра ниже.
func newHeartbeatMonitor(t *testing.T, s *uptimeStack, pid int64) uptime.Monitor {
	t.Helper()
	created, err := s.uptime.Create(context.Background(), uptime.Monitor{
		ProjectID: pid, Name: "Cron job", Kind: uptime.KindHeartbeat, Enabled: true,
		IntervalSeconds: 60, TimeoutSeconds: 10, FailThreshold: 3, RecoveryThreshold: 1,
		Consensus: uptime.ConsensusMajority, SSLAlertDays: 14,
		Config: heartbeatConfigJSON(t, uptime.HeartbeatConfig{GraceSeconds: 60}),
	}, []string{"local"}, nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return created
}

// TestHeartbeatPrefetchAndPreviewIgnored — P0 (C3/T9): ссылка heartbeat
// регулярно дёргается не человеком — unfurl-бот мессенджера, антивирусный
// прокси, префетч браузера, — и такой запрос НЕ обязан засчитываться как
// «сервис жив», иначе он гасит настоящую тревогу watchdog'а. Каждый признак
// (протокольный заголовок или известный User-Agent) обязан отдавать 204 без
// тела и не трогать вообще ничего: ни last_seen монитора, ни состояние.
func TestHeartbeatPrefetchAndPreviewIgnored(t *testing.T) {
	cases := []struct {
		name       string
		setHeaders func(r *http.Request)
		reason     web.HeartbeatIgnoreReason
	}{
		{"sec-purpose-prefetch", func(r *http.Request) { r.Header.Set("Sec-Purpose", "prefetch") }, web.HeartbeatIgnorePrefetchHeader},
		// Значение составное ("prefetch;prerender" и подобное) — сверка идёт
		// префиксом, не равенством (см. heartbeatIgnoreReason).
		{"sec-purpose-prefetch-prerender", func(r *http.Request) { r.Header.Set("Sec-Purpose", "prefetch;prerender") }, web.HeartbeatIgnorePrefetchHeader},
		{"purpose-prefetch", func(r *http.Request) { r.Header.Set("Purpose", "prefetch") }, web.HeartbeatIgnorePrefetchHeader},
		{"x-purpose-preview", func(r *http.Request) { r.Header.Set("X-Purpose", "preview") }, web.HeartbeatIgnorePrefetchHeader},
		{"x-moz-prefetch", func(r *http.Request) { r.Header.Set("X-Moz", "prefetch") }, web.HeartbeatIgnorePrefetchHeader},
		{"slackbot-user-agent", func(r *http.Request) {
			r.Header.Set("User-Agent", "Slackbot-LinkExpanding 1.0 (+https://api.slack.com/robots)")
		}, web.HeartbeatIgnoreBotUserAgent},
		{"telegram-user-agent", func(r *http.Request) {
			r.Header.Set("User-Agent", "TelegramBot (like TwitterBot)")
		}, web.HeartbeatIgnoreBotUserAgent},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newUptimeStack(t)
			pid := newProject(t, s.pool)
			created := newHeartbeatMonitor(t, s, pid)
			before := web.HeartbeatIgnoredBy(c.reason)

			req, err := http.NewRequest(http.MethodGet, s.srv.URL+"/uptime/hb/"+created.HeartbeatToken, nil)
			if err != nil {
				t.Fatalf("NewRequest: %v", err)
			}
			c.setHeaders(req)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("do request: %v", err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusNoContent {
				t.Fatalf("status = %d, want 204: %s", resp.StatusCode, body)
			}
			if len(body) != 0 {
				t.Errorf("204-ответ содержит тело %q, want пусто", body)
			}

			got, err := s.uptime.Get(context.Background(), created.ID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if got.LastBeatAt != nil {
				t.Fatalf("монитор не отмечен живым: LastBeatAt = %v, want nil", got.LastBeatAt)
			}

			states, err := s.uptime.States(context.Background(), created.ID)
			if err != nil {
				t.Fatalf("States: %v", err)
			}
			if len(states) != 0 {
				t.Fatalf("states = %+v, want пусто — отклонённый пинг не создаёт состояние", states)
			}

			if got := web.HeartbeatIgnoredBy(c.reason); got != before+1 {
				t.Errorf("HeartbeatIgnoredBy(%s) = %d, want %d", c.reason, got, before+1)
			}
		})
	}
}

// TestHeartbeatCurlLikeClientsNotIgnored — регресс: обычный curl/wget-подобный
// клиент (без протокольных заголовков префетча, с типичным User-Agent) обязан
// засчитываться как раньше, и GET, и POST. Отсев не должен быть шире, чем
// нужно.
func TestHeartbeatCurlLikeClientsNotIgnored(t *testing.T) {
	cases := []struct {
		name   string
		method string
		ua     string
	}{
		{"curl-get", http.MethodGet, "curl/8.4.0"},
		{"curl-post", http.MethodPost, "curl/8.4.0"},
		{"wget-get", http.MethodGet, "Wget/1.21.3"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newUptimeStack(t)
			pid := newProject(t, s.pool)
			created := newHeartbeatMonitor(t, s, pid)

			req, err := http.NewRequest(c.method, s.srv.URL+"/uptime/hb/"+created.HeartbeatToken, nil)
			if err != nil {
				t.Fatalf("NewRequest: %v", err)
			}
			req.Header.Set("User-Agent", c.ua)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("do request: %v", err)
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}

			got, err := s.uptime.Get(context.Background(), created.ID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if got.LastBeatAt == nil {
				t.Fatalf("LastBeatAt is nil, want set — обычный %s не должен отклоняться", c.ua)
			}
		})
	}
}
