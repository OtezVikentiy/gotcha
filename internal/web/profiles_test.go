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
	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/profile"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"gitflic.ru/otezvikentiy/gotcha/internal/web"
)

type profilesStack struct {
	pool *pgxpool.Pool
	ch   driver.Conn
	srv  *httptest.Server
	org  *org.Service
	auth *auth.Service
}

func newProfilesStack(t *testing.T, wire bool) *profilesStack {
	t.Helper()
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)
	authSvc := auth.NewService(pool)
	orgSvc := org.NewService(pool, 1_000_000)

	mux := http.NewServeMux()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)

	h := web.New(authSvc, orgSvc, nil, nil, srv.URL)
	if wire {
		h.Profiles = profile.NewQuery(ch)
	}
	h.Register(mux)
	return &profilesStack{pool: pool, ch: ch, srv: srv, org: orgSvc, auth: authSvc}
}

func TestWebProfiles(t *testing.T) {
	s := newProfilesStack(t, true)
	ctx := context.Background()
	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, "prof-owner@example.com")
	o, err := s.org.CreateOrg(ctx, "prof-co", "Prof Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	project, err := s.org.CreateProject(ctx, o.ID, "prof-proj", "Prof Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	// Засеять пару стеков.
	seedTS := time.Now().UTC().Add(-time.Minute)
	for _, st := range [][]string{{"root", "a"}, {"root", "b"}} {
		if err := s.ch.Exec(ctx, `INSERT INTO profile_samples
			(project_id,profile_type,service,environment,transaction,platform,ts,stack,value)
			VALUES (?,'cpu','api','','GET /x','go',?,?,?)`, project.ID, seedTS, st, uint64(5)); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	base := "/projects/" + strconv.FormatInt(project.ID, 10) + "/profiles"

	// Список групп.
	resp := getWithCookie(t, s.srv, base, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "api") {
		t.Fatalf("list status=%d body=%s", resp.StatusCode, body)
	}

	// Flamegraph.
	flame := base + "/flame?service=api&type=cpu&period=24h"
	resp = getWithCookie(t, s.srv, flame, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "<svg") {
		t.Fatalf("flame status=%d body=%s", resp.StatusCode, body)
	}

	// Чужой → 404.
	_, outsider := orgSettingsRegister(t, s.auth, "prof-outsider@example.com")
	resp = getWithCookie(t, s.srv, base, outsider)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("outsider status = %d, want 404", resp.StatusCode)
	}
}

func TestWebProfilesNilService(t *testing.T) {
	s := newProfilesStack(t, false)
	ctx := context.Background()
	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, "prof-nil-owner@example.com")
	o, _ := s.org.CreateOrg(ctx, "prof-nil-co", "Prof Nil Co", ownerID)
	project, _ := s.org.CreateProject(ctx, o.ID, "prof-nil-proj", "Prof Nil Proj", "go")
	base := "/projects/" + strconv.FormatInt(project.ID, 10) + "/profiles"
	resp := getWithCookie(t, s.srv, base, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("nil Profiles status = %d, want 404", resp.StatusCode)
	}}
