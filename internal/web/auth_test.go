package web_test

import (
	"fmt"
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
	"gitflic.ru/otezvikentiy/gotcha/internal/ingestsignal"
	"gitflic.ru/otezvikentiy/gotcha/internal/issue"
	"gitflic.ru/otezvikentiy/gotcha/internal/notify"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"gitflic.ru/otezvikentiy/gotcha/internal/web"
)

type stack struct {
	pool *pgxpool.Pool
	srv  *httptest.Server
	h    *web.Handler
	mux  *http.ServeMux
}

func newStack(t *testing.T) *stack {
	t.Helper()
	pool := testenv.MigratedPG(t)

	authSvc := auth.NewService(pool)
	orgSvc := org.NewService(pool, 1_000_000)
	issueSvc := issue.NewService(pool)
	var events *event.Query

	mux := http.NewServeMux()
	var h *web.Handler
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)

	h = web.New(authSvc, orgSvc, issueSvc, events, srv.URL)
	h.Alerts = alert.NewService(pool)
	h.Outbox = notify.NewOutbox(pool)
	h.Signals = ingestsignal.NewStore(pool)
	h.Register(mux)

	return &stack{pool: pool, srv: srv, h: h, mux: mux}
}

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

func TestRegisterExistingEmailNeutralMessage(t *testing.T) {
	s := newStack(t)

	form := url.Values{
		"email":     {"dup@example.com"},
		"password":  {"correct-horse-battery"},
		"password2": {"correct-horse-battery"},
	}

	resp := postForm(t, s.srv, "/register", form, s.srv.URL, nil)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("first register status = %d, want 303", resp.StatusCode)
	}

	resp = postForm(t, s.srv, "/register", form, s.srv.URL, nil)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("duplicate register status = %d, want 422", resp.StatusCode)
	}
	if strings.Contains(string(body), "уже зарегистрирован") {
		t.Fatalf("duplicate register body leaks account existence: %s", body)
	}
}

func TestLoginPerIPRateLimit(t *testing.T) {
	s := newStack(t)

	var last *http.Response
	for i := 0; i < 21; i++ {
		form := url.Values{
			"email":    {fmt.Sprintf("user%d@example.com", i)},
			"password": {"whatever-password"},
		}
		last = postForm(t, s.srv, "/login", form, s.srv.URL, nil)
		io.Copy(io.Discard, last.Body)
		last.Body.Close()
	}
	if last.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("21st login from same IP status = %d, want 429 (per-IP limit)", last.StatusCode)
	}
}

func TestLoginBadCredentialsPreservesEmail(t *testing.T) {
	s := newStack(t)

	form := url.Values{
		"email":    {"typed-user@example.com"},
		"password": {"wrong-password"},
	}
	resp := postForm(t, s.srv, "/login", form, s.srv.URL, nil)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("bad credentials status = %d, want 422", resp.StatusCode)
	}
	if !strings.Contains(string(body), `value="typed-user@example.com"`) {
		t.Fatalf("login form after bad credentials должен вернуть введённый email: %s", body)
	}
	if strings.Contains(string(body), "wrong-password") {
		t.Fatalf("login form after bad credentials не должен возвращать пароль: %s", body)
	}
}

func TestRegisterPasswordMismatchPreservesEmail(t *testing.T) {
	s := newStack(t)

	form := url.Values{
		"email":     {"typed-register@example.com"},
		"password":  {"correct-horse-battery"},
		"password2": {"does-not-match"},
	}
	resp := postForm(t, s.srv, "/register", form, s.srv.URL, nil)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("password mismatch status = %d, want 422", resp.StatusCode)
	}
	if !strings.Contains(string(body), `value="typed-register@example.com"`) {
		t.Fatalf("register form after password mismatch должен вернуть введённый email: %s", body)
	}
	if strings.Contains(string(body), "correct-horse-battery") || strings.Contains(string(body), "does-not-match") {
		t.Fatalf("register form после ошибки не должен возвращать пароль: %s", body)
	}
}

// email не проходит проверку формата до возврата в форму (password-mismatch наступает раньше) —
// доказываем, что возврат в value= держит экранирование движка шаблонов, а не ручную санитизацию.
func TestRegisterPasswordMismatchEscapesEmail(t *testing.T) {
	s := newStack(t)

	payload := `"><script>alert(1)</script>`
	form := url.Values{
		"email":     {payload},
		"password":  {"correct-horse-battery"},
		"password2": {"does-not-match"},
	}
	resp := postForm(t, s.srv, "/register", form, s.srv.URL, nil)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("password mismatch status = %d, want 422", resp.StatusCode)
	}
	if strings.Contains(string(body), "<script>alert(1)</script>") {
		t.Fatalf("email из формы пробил разметку: %s", body)
	}
	if strings.Contains(string(body), `"><script>`) {
		t.Fatalf("email не экранирован в атрибуте value=: %s", body)
	}
}

func TestWebAuthFlow(t *testing.T) {
	s := newStack(t)

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

	loginForm := url.Values{"email": {"web-user@example.com"}, "password": {"wrong-password"}}
	resp = postForm(t, s.srv, "/login", loginForm, "", nil)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST /login (no origin) status = %d, want 403", resp.StatusCode)
	}

	resp = postForm(t, s.srv, "/login", loginForm, s.srv.URL, nil)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST /login (wrong password) status = %d, want 422", resp.StatusCode)
	}
	if !strings.Contains(string(body), "еверный email или пароль") {
		t.Fatalf("POST /login (wrong password) body missing error text: %s", body)
	}

	var last *http.Response
	for i := 0; i < 5; i++ {
		last = postForm(t, s.srv, "/login", loginForm, s.srv.URL, nil)
		io.Copy(io.Discard, last.Body)
		last.Body.Close()
	}
	if last.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("6th POST /login status = %d, want 429", last.StatusCode)
	}

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
	req.AddCookie(cookie)
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
