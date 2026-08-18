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

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/deploy"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"gitflic.ru/otezvikentiy/gotcha/internal/web"
)

// deployStack — стенд экрана деплоев: только PG (экран читает deploy.Store).
type deployStack struct {
	pool   *pgxpool.Pool
	srv    *httptest.Server
	org    *org.Service
	auth   *auth.Service
	deploy *deploy.Store
}

func newDeployStack(t *testing.T, wireDeploy bool) *deployStack {
	t.Helper()
	pool := testenv.MigratedPG(t)

	authSvc := auth.NewService(pool)
	orgSvc := org.NewService(pool, 1_000_000)
	depSvc := deploy.NewStore(pool)

	mux := http.NewServeMux()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)

	h := web.New(authSvc, orgSvc, nil, nil, srv.URL)
	if wireDeploy {
		h.Deploy = depSvc
	}
	h.Register(mux)

	return &deployStack{pool: pool, srv: srv, org: orgSvc, auth: authSvc, deploy: depSvc}
}

func TestWebDeploymentsScreen(t *testing.T) {
	s := newDeployStack(t, true)
	ctx := context.Background()

	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, "deploy-list-owner@example.com")
	o, err := s.org.CreateOrg(ctx, "deploy-list-co", "Deploy List Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	project, err := s.org.CreateProject(ctx, o.ID, "deploy-list-proj", "Deploy List Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	now := time.Now().UTC()
	if _, err := s.deploy.Record(ctx, project.ID, deploy.Deployment{
		Version:     "v3.1.0",
		Environment: "prod",
		URL:         "https://ci.example/run/9",
		Changelog:   "fix cart\nspeed up checkout",
		DeployedAt:  now.Add(-2 * time.Hour),
	}); err != nil {
		t.Fatalf("record deploy: %v", err)
	}
	// Деплой с небезопасной схемой URL — показывается текстом, не ссылкой.
	if _, err := s.deploy.Record(ctx, project.ID, deploy.Deployment{
		Version:     "v3.0.0",
		Environment: "prod",
		URL:         "javascript:alert(1)",
		DeployedAt:  now.Add(-3 * time.Hour),
	}); err != nil {
		t.Fatalf("record deploy 2: %v", err)
	}

	listPath := "/projects/" + strconv.FormatInt(project.ID, 10) + "/deployments"
	resp := getWithCookie(t, s.srv, listPath, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200: %s", listPath, resp.StatusCode, body)
	}
	bs := string(body)
	if !strings.Contains(bs, "v3.1.0") {
		t.Fatalf("list missing version: %s", bs)
	}
	if !strings.Contains(bs, "prod") {
		t.Fatalf("list missing environment: %s", bs)
	}
	if !strings.Contains(bs, "https://ci.example/run/9") {
		t.Fatalf("list missing deploy url link: %s", bs)
	}
	if !strings.Contains(bs, "fix cart") {
		t.Fatalf("list missing changelog: %s", bs)
	}
	// Небезопасная схема не должна попасть в href.
	if strings.Contains(bs, "href=\"javascript:") || strings.Contains(bs, "href='javascript:") {
		t.Fatalf("unsafe url scheme leaked into href: %s", bs)
	}
}

func TestWebDeploymentsEmpty(t *testing.T) {
	s := newDeployStack(t, true)
	ctx := context.Background()

	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, "deploy-empty-owner@example.com")
	o, err := s.org.CreateOrg(ctx, "deploy-empty-co", "Deploy Empty Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	project, err := s.org.CreateProject(ctx, o.ID, "deploy-empty-proj", "Deploy Empty Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	listPath := "/projects/" + strconv.FormatInt(project.ID, 10) + "/deployments"
	resp := getWithCookie(t, s.srv, listPath, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200", listPath, resp.StatusCode)
	}
	if !strings.Contains(string(body), "Деплоев пока нет") {
		t.Fatalf("empty list missing placeholder: %s", body)
	}
}

func TestWebDeploymentsOutsider404(t *testing.T) {
	s := newDeployStack(t, true)
	ctx := context.Background()

	ownerID, _ := orgSettingsRegister(t, s.auth, "deploy-out-owner@example.com")
	o, err := s.org.CreateOrg(ctx, "deploy-out-co", "Deploy Out Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	project, err := s.org.CreateProject(ctx, o.ID, "deploy-out-proj", "Deploy Out Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	listPath := "/projects/" + strconv.FormatInt(project.ID, 10) + "/deployments"
	_, outsiderCookie := orgSettingsRegister(t, s.auth, "deploy-out-outsider@example.com")
	resp := getWithCookie(t, s.srv, listPath, outsiderCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET %s (outsider) status = %d, want 404", listPath, resp.StatusCode)
	}
}

func TestWebDeploymentsNilStore(t *testing.T) {
	// h.Deploy не проставлен → 404 (nil-guard, как h.Regressions).
	s := newDeployStack(t, false)
	ctx := context.Background()

	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, "deploy-nil-owner@example.com")
	o, err := s.org.CreateOrg(ctx, "deploy-nil-co", "Deploy Nil Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	project, err := s.org.CreateProject(ctx, o.ID, "deploy-nil-proj", "Deploy Nil Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	listPath := "/projects/" + strconv.FormatInt(project.ID, 10) + "/deployments"
	resp := getWithCookie(t, s.srv, listPath, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET %s (nil Deploy) status = %d, want 404", listPath, resp.StatusCode)
	}
}
