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
	"gitflic.ru/otezvikentiy/gotcha/internal/profile"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"gitflic.ru/otezvikentiy/gotcha/internal/web"
)

type profRegStack struct {
	pool *pgxpool.Pool
	srv  *httptest.Server
	org  *org.Service
	auth *auth.Service
	reg  *profile.RegressionService
}

func newProfRegStack(t *testing.T, wire bool) *profRegStack {
	t.Helper()
	pool := testenv.MigratedPG(t)
	authSvc := auth.NewService(pool)
	orgSvc := org.NewService(pool, 1_000_000)
	reg := profile.NewRegressionService(pool)

	mux := http.NewServeMux()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)

	h := web.New(authSvc, orgSvc, nil, nil, srv.URL)
	if wire {
		h.ProfileRegressions = reg
	}
	h.Register(mux)
	return &profRegStack{pool: pool, srv: srv, org: orgSvc, auth: authSvc, reg: reg}
}

func TestWebProfileRegressions(t *testing.T) {
	s := newProfRegStack(t, true)
	ctx := context.Background()
	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, "preg-owner@example.com")
	o, _ := s.org.CreateOrg(ctx, "preg-co", "PReg Co", ownerID)
	proj, _ := s.org.CreateProject(ctx, o.ID, "preg-proj", "PReg Proj", "go")

	// Открытая регрессия функции compress; закрытая — decode.
	if _, _, err := s.reg.Open(ctx, proj.ID, "api", "cpu", "compress", 0.1, 0.3); err != nil {
		t.Fatalf("open compress: %v", err)
	}
	dec, _, err := s.reg.Open(ctx, proj.ID, "api", "cpu", "decode", 0.1, 0.25)
	if err != nil {
		t.Fatalf("open decode: %v", err)
	}
	if _, err := s.reg.Resolve(ctx, dec.ID, 0.11); err != nil {
		t.Fatalf("resolve decode: %v", err)
	}

	base := "/projects/" + strconv.FormatInt(proj.ID, 10) + "/profile-regressions"

	// Дефолт (open): compress виден, decode нет.
	resp := getWithCookie(t, s.srv, base, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "compress") {
		t.Fatalf("open list status=%d body=%s", resp.StatusCode, body)
	}
	if strings.Contains(string(body), "decode") {
		t.Fatalf("resolved decode leaked into open filter")
	}

	// ?status=resolved: decode виден, compress нет.
	resp = getWithCookie(t, s.srv, base+"?status=resolved", ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "decode") || strings.Contains(string(body), "compress") {
		t.Fatalf("resolved filter wrong: %s", body)
	}

	// ?status=all: обе.
	resp = getWithCookie(t, s.srv, base+"?status=all", ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "compress") || !strings.Contains(string(body), "decode") {
		t.Fatalf("all filter missing one: %s", body)
	}

	// Чужой → 404.
	_, outsider := orgSettingsRegister(t, s.auth, "preg-outsider@example.com")
	resp = getWithCookie(t, s.srv, base, outsider)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("outsider status = %d, want 404", resp.StatusCode)
	}
}

func TestWebProfileRegressionsNil(t *testing.T) {
	s := newProfRegStack(t, false)
	ctx := context.Background()
	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, "preg-nil-owner@example.com")
	o, _ := s.org.CreateOrg(ctx, "preg-nil-co", "PReg Nil Co", ownerID)
	proj, _ := s.org.CreateProject(ctx, o.ID, "preg-nil-proj", "PReg Nil Proj", "go")
	base := "/projects/" + strconv.FormatInt(proj.ID, 10) + "/profile-regressions"
	resp := getWithCookie(t, s.srv, base, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("nil service status = %d, want 404", resp.StatusCode)
	}
}
