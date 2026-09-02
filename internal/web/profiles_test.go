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
	if strings.Contains(string(body), "flame-ancestor") || !strings.Contains(string(body), "Клик по сегменту") {
		t.Fatalf("flame without focus must render the root without ancestors and with the hint: %s", body)
	}

	// Зум: ?focus=root&focus=a — предки «all» и «root» строками-предками,
	// ссылки детей сохраняют фильтры и несут путь.
	resp = getWithCookie(t, s.srv, flame+"&focus=root&focus=a", ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || strings.Count(string(body), "flame-ancestor") != 2 {
		t.Fatalf("focused flame status=%d, want 200 with two ancestor rows: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), `href="`+base+`/flame?focus=root&amp;period=24h&amp;service=api&amp;type=cpu"`) {
		t.Fatalf("ancestor link must keep filters and carry its own path: %s", body)
	}
	if !strings.Contains(string(body), `href="`+base+`/flame?period=24h&amp;service=api&amp;type=cpu"`) {
		t.Fatalf("root link must reset focus and keep filters: %s", body)
	}
	// Пустой профиль (нет такого сервиса) — плейсхолдер без подсказки про зум.
	resp = getWithCookie(t, s.srv, base+"/flame?service=nope&type=cpu&period=24h", ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "нет данных профиля") || strings.Contains(string(body), "Клик по сегменту") {
		t.Fatalf("empty flame status=%d, want placeholder without the zoom hint: %s", resp.StatusCode, body)
	}

	// Оборванный путь (нет такого кадра) — полный профиль, не ошибка.
	resp = getWithCookie(t, s.srv, flame+"&focus=nope", ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || strings.Contains(string(body), "flame-ancestor") {
		t.Fatalf("broken focus status=%d, want 200 without ancestors: %s", resp.StatusCode, body)
	}

	// Произвольный диапазон на списке и flamegraph — селектор в режиме custom
	// (parseTimeRange custom, ссылки flame несут period=custom).
	custList := base + "?period=custom&start=2026-07-01T00:00&end=2026-07-10T00:00"
	resp = getWithCookie(t, s.srv, custList, ownerCookie)
	cbody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(cbody), `value="custom" selected`) {
		t.Fatalf("custom list status=%d body=%s", resp.StatusCode, cbody)
	}
	custFlame := base + "/flame?service=api&type=cpu&period=custom&start=2026-07-01T00:00&end=2026-07-10T00:00"
	resp = getWithCookie(t, s.srv, custFlame, ownerCookie)
	cbody, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(cbody), `value="custom" selected`) {
		t.Fatalf("custom flame status=%d body=%s", resp.StatusCode, cbody)
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
	}
}
