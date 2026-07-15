package web_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"gitflic.ru/otezvikentiy/gotcha/internal/trace"
	"gitflic.ru/otezvikentiy/gotcha/internal/web"
)

// regressionsStack — стенд страницы регрессий: только PG (страница читает
// trace.RegressionService, CH здесь не нужен).
type regressionsStack struct {
	pool *pgxpool.Pool
	srv  *httptest.Server
	org  *org.Service
	auth *auth.Service
	reg  *trace.RegressionService
}

func newRegressionsStack(t *testing.T, wireReg bool) *regressionsStack {
	t.Helper()
	pool := testenv.MigratedPG(t)

	authSvc := auth.NewService(pool)
	orgSvc := org.NewService(pool, 1_000_000)
	regSvc := trace.NewRegressionService(pool)

	mux := http.NewServeMux()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)

	h := web.New(authSvc, orgSvc, nil, nil, srv.URL)
	if wireReg {
		h.Regressions = regSvc
	}
	h.Register(mux)

	return &regressionsStack{pool: pool, srv: srv, org: orgSvc, auth: authSvc, reg: regSvc}
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

	// Открытая регрессия эндпойнта: p95 длительности 100ms → 150ms (+50%).
	if _, _, err := s.reg.Open(ctx, project.ID, "endpoint_p95", "GET /orders", "duration", 100, 150); err != nil {
		t.Fatalf("open endpoint regression: %v", err)
	}
	// Закрытая регрессия web-vital: LCP p75 2000ms → пик 4000ms (+100%), затем
	// закрыта значением восстановления 2100 (около базы). Рост % считается от
	// ПИКА, а не от current: иначе у закрытой строки было бы +5%, а не +100%.
	wv, _, err := s.reg.Open(ctx, project.ID, "webvital_p75", "/checkout", "lcp", 2000, 4000)
	if err != nil {
		t.Fatalf("open webvital regression: %v", err)
	}
	if _, err := s.reg.Resolve(ctx, wv.ID, 2100); err != nil {
		t.Fatalf("resolve webvital regression: %v", err)
	}

	listPath := "/projects/" + strconv.FormatInt(project.ID, 10) + "/regressions"

	// Дефолт (open) показывает только открытую регрессию эндпойнта с ростом %,
	// человекочитаемой метрикой, статусом и «ongoing».
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
	if !strings.Contains(bs, "ongoing") {
		t.Fatalf("open regression must show ongoing: %s", bs)
	}
	if strings.Contains(bs, "/checkout") {
		t.Fatalf("default (open) filter leaked resolved regression: %s", bs)
	}

	// ?status=resolved показывает закрытую web-vital регрессию с длительностью
	// (не «ongoing») и метрикой LCP p75.
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
	if strings.Contains(bs, "ongoing") {
		t.Fatalf("resolved regression must show duration, not ongoing: %s", bs)
	}

	// ?status=all показывает обе.
	resp = getWithCookie(t, s.srv, listPath+"?status=all", ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	bs = string(body)
	if !strings.Contains(bs, "GET /orders") || !strings.Contains(bs, "/checkout") {
		t.Fatalf("?status=all missing one of the regressions: %s", bs)
	}

	// Чужой юзер (не участник) → 404, не палим существование проекта.
	_, outsiderCookie := orgSettingsRegister(t, s.auth, "reg-list-outsider@example.com")
	resp = getWithCookie(t, s.srv, listPath, outsiderCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET %s (outsider) status = %d, want 404", listPath, resp.StatusCode)
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
	if !strings.Contains(string(body), "no regressions detected") {
		t.Fatalf("empty list missing placeholder: %s", body)
	}
}

func TestWebRegressionsNilService(t *testing.T) {
	// h.Regressions не проставлен → 404 (nil-guard, как h.PerfIssues).
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
