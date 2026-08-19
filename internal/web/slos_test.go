package web_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/slo"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"gitflic.ru/otezvikentiy/gotcha/internal/web"
	"gitflic.ru/otezvikentiy/gotcha/internal/web/templates"
)

type sloStack struct {
	pool *pgxpool.Pool
	srv  *httptest.Server
	org  *org.Service
	auth *auth.Service
	h    *web.Handler
	slo  *slo.Store
}

func newSLOStack(t *testing.T, wire bool) *sloStack {
	t.Helper()
	pool := testenv.MigratedPG(t)
	authSvc := auth.NewService(pool)
	orgSvc := org.NewService(pool, 1_000_000)
	store := slo.NewStore(pool)

	mux := http.NewServeMux()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)

	h := web.New(authSvc, orgSvc, nil, nil, srv.URL)
	if wire {
		h.SLO = store
		// SLOProviders намеренно nil: без ClickHouse ряды good/total не
		// посчитать, страница списка при этом обязана рендериться (HasData=false,
		// прочерк вместо %), а не падать. Расчёт достижения по провайдерам
		// проверяется на уровне пакета slo (provider_test.go).
	}
	h.Register(mux)
	return &sloStack{pool: pool, srv: srv, org: orgSvc, auth: authSvc, h: h, slo: store}
}

func TestWebSLOsList(t *testing.T) {
	s := newSLOStack(t, true)
	ctx := context.Background()
	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, "slo-owner@example.com")
	o, err := s.org.CreateOrg(ctx, "slo-co", "SLO Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	project, err := s.org.CreateProject(ctx, o.ID, "slo-proj", "SLO Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	if _, err := s.slo.Create(ctx, slo.SLO{
		ProjectID: project.ID, Name: "checkout availability", Kind: slo.SLIAvailability,
		Target: 0.99, WindowDays: 30, BurnThreshold: 14.4, BurnLongMin: 60, BurnShortMin: 5, Enabled: true,
	}); err != nil {
		t.Fatalf("seed slo: %v", err)
	}

	base := "/projects/" + strconv.FormatInt(project.ID, 10) + "/slos"

	// Список показывает засеянное SLO.
	resp := getWithCookie(t, s.srv, base, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "checkout availability") {
		t.Fatalf("page missing SLO (status %d): %s", resp.StatusCode, body)
	}

	// Создание валидного availability-SLO (target в %) → 303, запись в сторе.
	form := url.Values{
		"name": {"api latency"}, "sli_kind": {"latency"}, "target": {"95"},
		"window_days": {"7"}, "threshold_ms": {"300"}, "burn_threshold": {"14.4"},
	}
	resp = postForm(t, s.srv, base, form, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("create status = %d, want 303", resp.StatusCode)
	}
	list, _ := s.slo.List(ctx, project.ID)
	if len(list) != 2 {
		t.Fatalf("slos = %d, want 2", len(list))
	}

	// Невалидный target (>100%) → 422 с формой.
	bad := url.Values{"name": {"bad"}, "sli_kind": {"availability"}, "target": {"150"}, "window_days": {"30"}}
	resp = postForm(t, s.srv, base, bad, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("bad target status = %d, want 422", resp.StatusCode)
	}

	// latency без порога → 422 (kind-специфичная валидация).
	badLat := url.Values{"name": {"lat"}, "sli_kind": {"latency"}, "target": {"99"}, "window_days": {"30"}, "threshold_ms": {"0"}}
	resp = postForm(t, s.srv, base, badLat, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("latency no-threshold status = %d, want 422", resp.StatusCode)
	}

	// latency с порогом выше потолка (1 час в мс) → 422, а не 500 (int4-overflow).
	bigLat := url.Values{"name": {"big"}, "sli_kind": {"latency"}, "target": {"99"}, "window_days": {"30"}, "threshold_ms": {"3600001"}}
	resp = postForm(t, s.srv, base, bigLat, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("latency over-max status = %d, want 422", resp.StatusCode)
	}

	// uptime без монитора → 422 (kind-специфичная валидация).
	badUp := url.Values{"name": {"up"}, "sli_kind": {"uptime"}, "target": {"99"}, "window_days": {"30"}}
	resp = postForm(t, s.srv, base, badUp, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("uptime no-monitor status = %d, want 422", resp.StatusCode)
	}

	// Удаление SLO (двухшаговое подтверждение: confirmed=yes).
	del := url.Values{"confirmed": {"yes"}, "slo_id": {strconv.FormatInt(list[0].ID, 10)}}
	resp = postForm(t, s.srv, base+"/"+strconv.FormatInt(list[0].ID, 10)+"/delete", del, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("delete status = %d, want 303", resp.StatusCode)
	}
	if list2, _ := s.slo.List(ctx, project.ID); len(list2) != 1 {
		t.Fatalf("after delete slos = %d, want 1", len(list2))
	}

	// Член организации без команды на проекте → 404 (requireProjectOperator).
	memberID, memberCookie := orgSettingsRegister(t, s.auth, "slo-member@example.com")
	if err := s.org.AddMember(ctx, o.ID, memberID, org.RoleMember); err != nil {
		t.Fatalf("add member: %v", err)
	}
	resp = getWithCookie(t, s.srv, base, memberCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("member (no team) status = %d, want 404", resp.StatusCode)
	}

	// Кап-на-проект на web-слое: добиваем до 100 через стор, 101-й POST → 422
	// (ErrTooManySLOs транслируется в err.slo.too_many, а не 500).
	cur, _ := s.slo.List(ctx, project.ID)
	for i := len(cur); i < 100; i++ {
		if _, err := s.slo.Create(ctx, slo.SLO{
			ProjectID: project.ID, Name: "fill", Kind: slo.SLIAvailability, Target: 0.99,
			WindowDays: 30, BurnThreshold: 14.4, BurnLongMin: 60, BurnShortMin: 5, Enabled: true,
		}); err != nil {
			t.Fatalf("fill #%d: %v", i, err)
		}
	}
	over := url.Values{"name": {"over"}, "sli_kind": {"availability"}, "target": {"99"}, "window_days": {"30"}}
	resp = postForm(t, s.srv, base, over, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("over-cap create status = %d, want 422", resp.StatusCode)
	}
}

func TestWebSLOsNilService(t *testing.T) {
	s := newSLOStack(t, false)
	ctx := context.Background()
	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, "slo-nil-owner@example.com")
	o, _ := s.org.CreateOrg(ctx, "slo-nil-co", "SLO Nil Co", ownerID)
	project, _ := s.org.CreateProject(ctx, o.ID, "slo-nil-proj", "SLO Nil Proj", "go")
	base := "/projects/" + strconv.FormatInt(project.ID, 10) + "/slos"
	resp := getWithCookie(t, s.srv, base, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("nil service status = %d, want 404", resp.StatusCode)
	}
}

// TestSLOsPathHelper фиксирует адрес раздела для nav/детали (T7).
func TestSLOsPathHelper(t *testing.T) {
	if got := templates.SLOsPath(7); got != "/projects/7/slos" {
		t.Fatalf("SLOsPath(7) = %q, want /projects/7/slos", got)
	}
}
