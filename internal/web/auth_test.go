package web_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/alert"
	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/event"
	"gitflic.ru/otezvikentiy/gotcha/internal/issue"
	"gitflic.ru/otezvikentiy/gotcha/internal/notify"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"gitflic.ru/otezvikentiy/gotcha/internal/web"
)

type stack struct {
	pool *pgxpool.Pool
	srv  *httptest.Server
}

func newStack(t *testing.T) *stack {
	t.Helper()
	pool := testenv.MigratedPG(t)

	authSvc := auth.NewService(pool)
	orgSvc := org.NewService(pool, 1_000_000)
	issueSvc := issue.NewService(pool)
	var events *event.Query // не трогается в задаче 4

	mux := http.NewServeMux()
	var h *web.Handler
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)

	h = web.New(authSvc, orgSvc, issueSvc, events, srv.URL)
	// Alerts (план 6, задача 5): онбординг вызывает EnsureDefaultRules при
	// создании проекта, а /projects/{id}/alerts нужен во всех сценариях,
	// использующих этот общий стенд (alerts_test.go, orgsettings_test.go,
	// projsettings_test.go, onboarding_test.go) — заводим сервис здесь один
	// раз, а не в каждом тесте отдельно.
	h.Alerts = alert.NewService(pool)
	// Outbox (план 6, задача 5, spec §7): страница /projects/{id}/alerts
	// показывает failed-доставки — тот же принцип, что и Alerts выше, заводим
	// один раз на весь стенд, а не в каждом тесте.
	h.Outbox = notify.NewOutbox(pool)
	h.Register(mux)

	return &stack{pool: pool, srv: srv}
}

// noRedirectClient не следует за редиректами, чтобы можно было проверить
// статус и Location самостоятельно.
func noRedirectClient() *http.Client {
	return &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func postForm(t *testing.T, srv *httptest.Server, path string, form url.Values, origin string, cookie *http.Cookie) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL+path, strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	resp, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	return resp
}

func sessionCookie(resp *http.Response) *http.Cookie {
	for _, c := range resp.Cookies() {
		if c.Name == auth.CookieName {
			return c
		}
	}
	return nil
}

func TestWebAuthFlow(t *testing.T) {
	s := newStack(t)

	// GET /login → 200 + форма.
	resp, err := http.Get(s.srv.URL + "/login")
	if err != nil {
		t.Fatalf("get /login: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /login status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(string(body), "<form") {
		t.Fatalf("GET /login body has no <form: %s", body)
	}

	// POST /register (валидная форма, верный Origin) → 303 на /, cookie выставлена.
	form := url.Values{
		"email":     {"web-user@example.com"},
		"password":  {"correct-horse-battery"},
		"password2": {"correct-horse-battery"},
	}
	resp = postForm(t, s.srv, "/register", form, s.srv.URL, nil)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST /register status = %d, want 303", resp.StatusCode)
	}
	if got := resp.Header.Get("Location"); got != "/" {
		t.Fatalf("POST /register Location = %q, want /", got)
	}
	cookie := sessionCookie(resp)
	if cookie == nil || cookie.Value == "" {
		t.Fatalf("POST /register did not set session cookie")
	}

	// GET / с cookie → 303 /onboarding (организаций нет).
	req, _ := http.NewRequest(http.MethodGet, s.srv.URL+"/", nil)
	req.AddCookie(cookie)
	resp, err = noRedirectClient().Do(req)
	if err != nil {
		t.Fatalf("get /: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("GET / status = %d, want 303", resp.StatusCode)
	}
	if got := resp.Header.Get("Location"); got != "/onboarding" {
		t.Fatalf("GET / Location = %q, want /onboarding", got)
	}

	// GET / с HX-Request: true, без cookie → 200 + HX-Redirect.
	req, _ = http.NewRequest(http.MethodGet, s.srv.URL+"/", nil)
	req.Header.Set("HX-Request", "true")
	resp, err = noRedirectClient().Do(req)
	if err != nil {
		t.Fatalf("get / htmx: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET / (htmx) status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("HX-Redirect"); got != "/login" {
		t.Fatalf("GET / (htmx) HX-Redirect = %q, want /login", got)
	}

	// POST /login без Origin → 403.
	loginForm := url.Values{"email": {"web-user@example.com"}, "password": {"wrong-password"}}
	resp = postForm(t, s.srv, "/login", loginForm, "", nil)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST /login (no origin) status = %d, want 403", resp.StatusCode)
	}

	// POST /login с неверным паролем → 422 и текст ошибки.
	resp = postForm(t, s.srv, "/login", loginForm, s.srv.URL, nil)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST /login (wrong password) status = %d, want 422", resp.StatusCode)
	}
	if !strings.Contains(string(body), "неверный") {
		t.Fatalf("POST /login (wrong password) body missing error text: %s", body)
	}

	// POST /login 6 раз подряд (тот же ip|email) → шестой 429.
	// Первая попытка уже израсходована выше — используем оставшиеся 5 слотов.
	var last *http.Response
	for i := 0; i < 5; i++ {
		last = postForm(t, s.srv, "/login", loginForm, s.srv.URL, nil)
		io.Copy(io.Discard, last.Body)
		last.Body.Close()
	}
	if last.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("6th POST /login status = %d, want 429", last.StatusCode)
	}

	// POST /logout → cookie очищена, GET / → 303 /login.
	resp = postForm(t, s.srv, "/logout", url.Values{}, s.srv.URL, cookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST /logout status = %d, want 303", resp.StatusCode)
	}
	cleared := sessionCookie(resp)
	if cleared == nil || cleared.MaxAge >= 0 && cleared.Value != "" {
		t.Fatalf("POST /logout did not clear cookie: %+v", cleared)
	}

	req, _ = http.NewRequest(http.MethodGet, s.srv.URL+"/", nil)
	req.AddCookie(cookie) // старая cookie, сессия уже уничтожена на сервере
	resp, err = noRedirectClient().Do(req)
	if err != nil {
		t.Fatalf("get / after logout: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("GET / after logout status = %d, want 303", resp.StatusCode)
	}
	if got := resp.Header.Get("Location"); got != "/login" {
		t.Fatalf("GET / after logout Location = %q, want /login", got)
	}
}
