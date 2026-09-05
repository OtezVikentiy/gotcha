package web_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/depsuppress"
	"gitflic.ru/otezvikentiy/gotcha/internal/event"
	"gitflic.ru/otezvikentiy/gotcha/internal/export"
	"gitflic.ru/otezvikentiy/gotcha/internal/issue"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
	"gitflic.ru/otezvikentiy/gotcha/internal/web"
)

// confirmLimitStack — фикс-раунд 2/5 T4, находка «дыра покрытия»: один общий
// стенд со ВСЕМИ сущностями, нужными четырнадцати маршрутам, которые в
// фикс-раунде 1 получили h.parseForm(w, r). testenv.MigratedPG/MigratedCH
// поднимают контейнеры один раз на весь прогон пакета (sync.Once), так что
// один такой стенд не дороже одного из соседних *_test.go, а не в 14 раз
// дороже.
type confirmLimitStack struct {
	pool *pgxpool.Pool
	srv  *httptest.Server
	h    *web.Handler

	// adminUID — первый и единственный зарегистрированный на этом стенде
	// пользователь: PROD-B1 делает первого зарегистрированного
	// инстанс-админом, что и нужно profileInstanceAdminTransfer/
	// orgSettingsSSODelete. Он же владелец org/project/team ниже —
	// requireProjectOwner/requireProjectOperator/requireOrgOwner/
	// requireTeamRole все проходят по одной и той же роли owner.
	adminUID    int64
	adminCookie *http.Cookie

	// otherUID — НЕ инстанс-админ и не владелец никакого орга: нужен
	// profileDelete отдельно от adminUID, у которого SoleOwnedOrgNames
	// вернул бы non-empty (409 раньше, чем дело дойдёт до parseForm).
	otherUID    int64
	otherCookie *http.Cookie

	orgID     int64
	projectID int64
	teamID    int64

	httpMonitorID      int64
	heartbeatMonitorID int64
	exportJobID        int64
	statusPageID       int64
}

func newConfirmLimitStack(t *testing.T) *confirmLimitStack {
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

	mux := http.NewServeMux()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)

	h := web.New(authSvc, orgSvc, issueSvc, events, srv.URL)
	h.Uptime = uptimeSvc
	h.UptimeWriter = writer
	h.UptimeQuery = uptime.NewQuery(ch)
	h.Exports = export.NewStore(pool)
	h.ExportDir = t.TempDir()
	h.AlertDeps = depsuppress.NewStore(pool)
	h.Register(mux)

	ctx := context.Background()
	adminUID, err := authSvc.Register(ctx, "confirm-limit-admin@example.com", "correct-horse-battery")
	if err != nil {
		t.Fatalf("register admin: %v", err)
	}
	adminToken, err := authSvc.CreateSession(ctx, adminUID)
	if err != nil {
		t.Fatalf("create admin session: %v", err)
	}
	otherUID, err := authSvc.Register(ctx, "confirm-limit-other@example.com", "correct-horse-battery")
	if err != nil {
		t.Fatalf("register other: %v", err)
	}
	otherToken, err := authSvc.CreateSession(ctx, otherUID)
	if err != nil {
		t.Fatalf("create other session: %v", err)
	}

	o, err := orgSvc.CreateOrg(ctx, "confirm-limit-org", "Confirm Limit", adminUID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	proj, err := orgSvc.CreateProject(ctx, o.ID, "confirm-limit-proj", "Confirm Limit Proj", "other")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	team, err := orgSvc.CreateTeam(ctx, o.ID, "confirm-limit-team", "Confirm Limit Team")
	if err != nil {
		t.Fatalf("create team: %v", err)
	}

	httpM := baseMonitor(proj.ID, "confirm-limit http")
	httpM.Config = monHTTPConfig(t, "https://example.invalid/health")
	createdHTTP, err := uptimeSvc.Create(ctx, httpM, []string{"local"}, nil)
	if err != nil {
		t.Fatalf("create http monitor: %v", err)
	}

	hbM := uptime.Monitor{
		ProjectID:          proj.ID,
		Name:               "confirm-limit heartbeat",
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
	createdHB, err := uptimeSvc.Create(ctx, hbM, []string{"local"}, nil)
	if err != nil {
		t.Fatalf("create heartbeat monitor: %v", err)
	}

	jobID, err := h.Exports.Enqueue(ctx, export.Job{
		ProjectID: proj.ID,
		CreatedBy: adminUID,
		Kind:      export.KindIssues,
		Format:    export.FormatCSV,
		Params:    export.Params{Since: time.Now().Add(-24 * time.Hour), Until: time.Now()},
	})
	if err != nil {
		t.Fatalf("enqueue export job: %v", err)
	}
	// Терминальный статус — тем же приёмом, что exportsStack.markDone
	// (exports_test.go): exportsDelete требует job.Status.Terminal() ДО
	// h.parseForm, значит для теста этого маршрута заявка обязана быть done.
	if _, err := pool.Exec(ctx,
		`UPDATE export_jobs SET status='done', finished_at=now(), expires_at=now()+interval '7 days' WHERE id=$1`,
		jobID); err != nil {
		t.Fatalf("mark export job done: %v", err)
	}

	sp, err := uptimeSvc.CreateStatusPage(ctx, uptime.StatusPage{
		ProjectID: proj.ID,
		Title:     "Confirm Limit Status",
		Enabled:   false, // Enabled=false — оператора достаточно, не нужен CanManage
	}, nil)
	if err != nil {
		t.Fatalf("create status page: %v", err)
	}

	return &confirmLimitStack{
		pool: pool, srv: srv, h: h,
		adminUID:    adminUID,
		adminCookie: &http.Cookie{Name: auth.CookieName, Value: adminToken},
		otherUID:    otherUID,
		otherCookie: &http.Cookie{Name: auth.CookieName, Value: otherToken},
		orgID:       o.ID, projectID: proj.ID, teamID: team.ID,
		httpMonitorID: createdHTTP.ID, heartbeatMonitorID: createdHB.ID,
		exportJobID: jobID, statusPageID: sp.ID,
	}
}

// TestConfirmAndSwitchHandlersOversizedBodyReturns413 — фикс-раунд 2/5 T4:
// таблица по ВСЕМ четырнадцати маршрутам фикс-раунда 1 (12 confirm-хендлеров
// + themeSwitch/localeSwitch), а не по одному profileDelete-представителю —
// тест реально ХОДИТ по каждой строке своим HTTP-запросом на свой маршрут
// со своим предзаведённым в БД объектом, а не просто перечисляет имена.
// Проверка одна и та же для каждой строки: тело сверх formBodyMaxBytes
// (поле pad) отвечает 413, а не 200 (confirm-страница)/303 (редирект)/500.
func TestConfirmAndSwitchHandlersOversizedBodyReturns413(t *testing.T) {
	s := newConfirmLimitStack(t)
	huge := strings.Repeat("x", 70_000) // > formBodyMaxBytes (64 КиБ)

	cases := []struct {
		name   string
		path   string
		cookie *http.Cookie // nil — без сессии (публичный маршрут)
	}{
		{"monitorDelete", fmt.Sprintf("/monitors/%d/delete", s.httpMonitorID), s.adminCookie},
		{"monitorHeartbeatRegenerate", fmt.Sprintf("/monitors/%d/heartbeat/regenerate", s.heartbeatMonitorID), s.adminCookie},
		{"exportsDelete", fmt.Sprintf("/projects/%d/exports/%d/delete", s.projectID, s.exportJobID), s.adminCookie},
		{"projectSettingsDelete", fmt.Sprintf("/projects/%d/settings/delete", s.projectID), s.adminCookie},
		{"teamDelete", fmt.Sprintf("/teams/%d/delete", s.teamID), s.adminCookie},
		{"profileDelete", "/profile/delete", s.otherCookie}, // otherUID: не sole owner, иначе 409 раньше parseForm
		{"profileInstanceAdminTransfer", "/profile/instance-admin/transfer", s.adminCookie},
		{"orgSettingsDelete", fmt.Sprintf("/orgs/%d/settings/delete", s.orgID), s.adminCookie},
		{"orgSettingsLeave", fmt.Sprintf("/orgs/%d/settings/leave", s.orgID), s.adminCookie},
		{"orgSettingsSSODelete", fmt.Sprintf("/orgs/%d/settings/sso/delete", s.orgID), s.adminCookie},
		{"statusPagesDelete", fmt.Sprintf("/statuspages/%d/delete", s.statusPageID), s.adminCookie},
		{"alertSuppressionDelete", fmt.Sprintf("/projects/%d/alert-suppression/1/delete", s.projectID), s.adminCookie},
		{"themeSwitch", "/settings/theme", nil},
		{"localeSwitch", "/settings/locale", nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := postForm(t, s.srv, tc.path, url.Values{"pad": {huge}, "confirmed": {"yes"}}, s.srv.URL, tc.cookie)
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusRequestEntityTooLarge {
				t.Fatalf("%s: status = %d, want 413: %s", tc.path, resp.StatusCode, body)
			}
		})
	}
}
