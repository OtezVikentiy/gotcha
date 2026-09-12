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

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/alert"
	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/metric"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
	"gitflic.ru/otezvikentiy/gotcha/internal/web"
)

type shellOperateStack struct {
	pool *pgxpool.Pool
	srv  *httptest.Server
	org  *org.Service
	auth *auth.Service
}

func newShellOperateStack(t *testing.T) *shellOperateStack {
	t.Helper()
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)

	authSvc := auth.NewService(pool)
	orgSvc := org.NewService(pool, 1_000_000)
	uptimeSvc := uptime.NewService(pool)
	alertSvc := alert.NewService(pool)
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

	h := web.New(authSvc, orgSvc, nil, nil, srv.URL)
	h.Uptime = uptimeSvc
	h.UptimeWriter = writer
	h.UptimeQuery = uptime.NewQuery(ch)
	h.Alerts = alertSvc
	h.Metrics = metric.NewQuery(ch)
	h.Register(mux)

	return &shellOperateStack{pool: pool, srv: srv, org: orgSvc, auth: authSvc}
}

// только здесь регрессия в withShell (забытое присваивание CanOperate) уронит тест через HTTP.
// безкомандный участник получает 404, не 200 без операторских ссылок: условия сегодня совпадают.
func TestWebShellCanOperateSidebar(t *testing.T) {
	s := newShellOperateStack(t)
	operatorID, operatorCookie := orgSettingsRegister(t, s.auth, "shellop-operator@example.com")
	teamlessID, teamlessCookie := orgSettingsRegister(t, s.auth, "shellop-teamless@example.com")

	o, err := s.org.CreateOrg(context.Background(), "shellop-co", "ShellOp Co", operatorID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	if err := s.org.AddMember(context.Background(), o.ID, teamlessID, org.RoleMember); err != nil {
		t.Fatalf("add teamless member: %v", err)
	}
	proj, err := s.org.CreateProject(context.Background(), o.ID, "shellop-proj", "ShellOp Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	addTeamAccess(t, s.org, o.ID, proj.ID, operatorID, "shellop-team")

	projID := strconv.FormatInt(proj.ID, 10)

	// Ключи карты — страница-источник набора пунктов: ctxNav рендерит подразделы ТЕКУЩЕЙ
	// области, не общий рейл.
	operatorHrefs := map[string][]string{
		"/projects/" + projID + "/alerts": {
			"/projects/" + projID + "/alerts",
			"/projects/" + projID + "/metrics/alerts",
			"/projects/" + projID + "/hosts/settings",
			"/projects/" + projID + "/slos",
			"/projects/" + projID + "/maintenance",
			"/projects/" + projID + "/alert-suppression",
			"/projects/" + projID + "/escalations",
			"/projects/" + projID + "/alerts/deliveries",
		},
		"/projects/" + projID + "/statuspages": {
			"/projects/" + projID + "/statuspages",
		},
	}

	for path, hrefs := range operatorHrefs {
		resp := getWithCookie(t, s.srv, path, operatorCookie)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s (operator) status = %d, want 200: %s", path, resp.StatusCode, body)
		}
		for _, href := range hrefs {
			if !strings.Contains(string(body), `href="`+href+`"`) {
				t.Fatalf("GET %s (operator) sidebar missing operator href %q", path, href)
			}
		}
	}

	for path := range operatorHrefs {
		resp := getWithCookie(t, s.srv, path, teamlessCookie)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("GET %s (teamless) status = %d, want 404: %s", path, resp.StatusCode, body)
		}
		for _, hrefs := range operatorHrefs {
			for _, href := range hrefs {
				if strings.Contains(string(body), `href="`+href+`"`) {
					t.Fatalf("GET %s (teamless) 404 body unexpectedly contains operator href %q", path, href)
				}
			}
		}
	}
}
