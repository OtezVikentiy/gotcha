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
	"gitflic.ru/otezvikentiy/gotcha/internal/trace"
	"gitflic.ru/otezvikentiy/gotcha/internal/web"
)

type regressionsStack struct {
	pool   *pgxpool.Pool
	srv    *httptest.Server
	org    *org.Service
	auth   *auth.Service
	reg    *trace.RegressionService
	deploy *deploy.Store
}

func newRegressionsStack(t *testing.T, wireReg bool) *regressionsStack {
	t.Helper()
	pool := testenv.MigratedPG(t)

	authSvc := auth.NewService(pool)
	orgSvc := org.NewService(pool, 1_000_000)
	regSvc := trace.NewRegressionService(pool)
	deploySvc := deploy.NewStore(pool)

	mux := http.NewServeMux()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)

	h := web.New(authSvc, orgSvc, nil, nil, srv.URL)
	if wireReg {
		h.Regressions = regSvc
		h.Deploy = deploySvc
	}
	h.Register(mux)

	return &regressionsStack{pool: pool, srv: srv, org: orgSvc, auth: authSvc, reg: regSvc, deploy: deploySvc}
}

func TestWebRegressionsList(t *testing.T) {
	s := newRegressionsStack(t, true)
	ctx := context.Background()

	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, "reg-list-owner@example.com")
	o, err := s.org.CreateOrg(ctx, "reg-list-co", "Reg List Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	project, err := s.org.CreateProject(ctx, o.ID, "reg-list-proj", "Reg List Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	if _, _, err := s.reg.Open(ctx, project.ID, "endpoint_p95", "GET /orders", "duration", 100, 150, false); err != nil {
		t.Fatalf("open endpoint regression: %v", err)
	}
	// Рост % считается от ПИКА, а не от current — иначе у закрытой строки было бы +5%, а не +100%.
	wv, _, err := s.reg.Open(ctx, project.ID, "webvital_p75", "/checkout", "lcp", 2000, 4000, false)
	if err != nil {
		t.Fatalf("open webvital regression: %v", err)
	}
	if _, err := s.reg.Resolve(ctx, wv.ID, 2100); err != nil {
		t.Fatalf("resolve webvital regression: %v", err)
	}

	listPath := "/projects/" + strconv.FormatInt(project.ID, 10) + "/regressions"

	resp := getWithCookie(t, s.srv, listPath, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200: %s", listPath, resp.StatusCode, body)
	}
	bs := string(body)
	if !strings.Contains(bs, "GET /orders") {
		t.Fatalf("list missing endpoint target: %s", bs)
	}
	if !strings.Contains(bs, "p95 длительности") {
		t.Fatalf("list missing human-readable metric: %s", bs)
	}
	if !strings.Contains(bs, "+50%") {
		t.Fatalf("list missing increase pct: %s", bs)
	}
	if !strings.Contains(bs, "идёт") {
		t.Fatalf("open regression must show ongoing: %s", bs)
	}
	if strings.Contains(bs, "/checkout") {
		t.Fatalf("default (open) filter leaked resolved regression: %s", bs)
	}

	resp = getWithCookie(t, s.srv, listPath+"?status=resolved", ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s?status=resolved status = %d, want 200", listPath, resp.StatusCode)
	}
	bs = string(body)
	if !strings.Contains(bs, "/checkout") {
		t.Fatalf("?status=resolved missing resolved regression: %s", bs)
	}
	if !strings.Contains(bs, "LCP p75") {
		t.Fatalf("?status=resolved missing vital metric label: %s", bs)
	}
	if !strings.Contains(bs, "+100%") {
		t.Fatalf("?status=resolved missing increase pct: %s", bs)
	}
	if strings.Contains(bs, "GET /orders") {
		t.Fatalf("?status=resolved leaked open regression: %s", bs)
	}
	if strings.Contains(bs, "идёт") {
		t.Fatalf("resolved regression must show duration, not ongoing: %s", bs)
	}

	resp = getWithCookie(t, s.srv, listPath+"?status=all", ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	bs = string(body)
	if !strings.Contains(bs, "GET /orders") || !strings.Contains(bs, "/checkout") {
		t.Fatalf("?status=all missing one of the regressions: %s", bs)
	}

	_, outsiderCookie := orgSettingsRegister(t, s.auth, "reg-list-outsider@example.com")
	resp = getWithCookie(t, s.srv, listPath, outsiderCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET %s (outsider) status = %d, want 404", listPath, resp.StatusCode)
	}
}

func TestWebRegressionsDeployAttribution(t *testing.T) {
	s := newRegressionsStack(t, true)
	ctx := context.Background()

	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, "reg-deploy-owner@example.com")
	o, err := s.org.CreateOrg(ctx, "reg-deploy-co", "Reg Deploy Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	project, err := s.org.CreateProject(ctx, o.ID, "reg-deploy-proj", "Reg Deploy Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	if _, err := s.deploy.Record(ctx, project.ID, deploy.Deployment{Version: "v9.9.9-recent", Environment: "prod", DeployedAt: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatalf("record recent deploy: %v", err)
	}
	if _, err := s.deploy.Record(ctx, project.ID, deploy.Deployment{Version: "v0.0.0-stale", Environment: "prod", DeployedAt: time.Now().Add(-8 * 24 * time.Hour)}); err != nil {
		t.Fatalf("record stale deploy: %v", err)
	}

	if _, _, err := s.reg.Open(ctx, project.ID, "endpoint_p95", "GET /orders", "duration", 100, 150, false); err != nil {
		t.Fatalf("open regression: %v", err)
	}

	listPath := "/projects/" + strconv.FormatInt(project.ID, 10) + "/regressions"
	resp := getWithCookie(t, s.srv, listPath, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200: %s", listPath, resp.StatusCode, body)
	}
	bs := string(body)
	if !strings.Contains(bs, "v9.9.9-recent") {
		t.Fatalf("regression must be attributed to preceding deploy in window: %s", bs)
	}
	if !strings.Contains(bs, "после деплоя") {
		t.Fatalf("attribution missing localized prefix: %s", bs)
	}
	if strings.Contains(bs, "v0.0.0-stale") {
		t.Fatalf("deploy older than window must not attribute: %s", bs)
	}
}

func TestWebRegressionsListEmpty(t *testing.T) {
	s := newRegressionsStack(t, true)
	ctx := context.Background()

	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, "reg-empty-owner@example.com")
	o, err := s.org.CreateOrg(ctx, "reg-empty-co", "Reg Empty Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	project, err := s.org.CreateProject(ctx, o.ID, "reg-empty-proj", "Reg Empty Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	listPath := "/projects/" + strconv.FormatInt(project.ID, 10) + "/regressions"
	resp := getWithCookie(t, s.srv, listPath, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200", listPath, resp.StatusCode)
	}
	if !strings.Contains(string(body), "Регрессий нет") {
		t.Fatalf("empty list missing placeholder: %s", body)
	}
}

func TestWebRegressionsNilService(t *testing.T) {
	s := newRegressionsStack(t, false)
	ctx := context.Background()

	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, "reg-nil-owner@example.com")
	o, err := s.org.CreateOrg(ctx, "reg-nil-co", "Reg Nil Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	project, err := s.org.CreateProject(ctx, o.ID, "reg-nil-proj", "Reg Nil Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	listPath := "/projects/" + strconv.FormatInt(project.ID, 10) + "/regressions"
	resp := getWithCookie(t, s.srv, listPath, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET %s (nil Regressions) status = %d, want 404", listPath, resp.StatusCode)
	}
}
