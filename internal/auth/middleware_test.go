package auth_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

func TestRequireUser(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := auth.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	uid, err := svc.Register(ctx, "mw@example.com", "hunter2hunter2")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	token, err := svc.CreateSession(ctx, uid)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	var gotUID int64
	var gotOK bool
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUID, gotOK = auth.UserID(r.Context())
		w.WriteHeader(http.StatusOK)
	})
	h := svc.RequireUser(inner)

	// Без cookie → редирект на /login.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/issues", nil))
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login" {
		t.Fatalf("anonymous: code=%d location=%q", rec.Code, rec.Header().Get("Location"))
	}

	// С валидной cookie → внутренний хендлер видит userID.
	rec = httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/issues", nil)
	req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: token})
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !gotOK || gotUID != uid {
		t.Fatalf("authenticated: code=%d uid=%d ok=%v", rec.Code, gotUID, gotOK)
	}

	// С поддельной cookie → редирект.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/issues", nil)
	req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: "forged"})
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("forged cookie: code=%d, want 303", rec.Code)
	}
}

func TestRequireUserDBOutage(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := auth.NewService(pool)
	pool.Close() // имитируем недоступность БД

	h := svc.RequireUser(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("inner handler must not be called")
	}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/issues", nil)
	req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: "sometoken"})
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("db outage: code=%d, want 500", rec.Code)
	}
	if len(rec.Result().Cookies()) != 0 {
		t.Fatalf("db outage must not clear the user's cookie: %+v", rec.Result().Cookies())
	}
}

func TestSessionCookieFlags(t *testing.T) {
	rec := httptest.NewRecorder()
	auth.SetSessionCookie(rec, "tok", false)
	c := rec.Result().Cookies()[0]
	if !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.Path != "/" || c.Secure {
		t.Fatalf("cookie flags (insecure base url): %+v", c)
	}
	rec = httptest.NewRecorder()
	auth.SetSessionCookie(rec, "tok", true)
	if c := rec.Result().Cookies()[0]; !c.Secure {
		t.Fatalf("secure=true not applied: %+v", c)
	}
	rec = httptest.NewRecorder()
	auth.ClearSessionCookie(rec)
	c = rec.Result().Cookies()[0]
	if c.MaxAge != -1 {
		t.Fatalf("clear cookie: MaxAge=%d, want -1", c.MaxAge)
	}
}

// На HTTPS cookie должна писаться под префиксным именем __Host-, соблюдающим
// требования браузера (Path=/, Secure, без Domain).
func TestSessionCookieHostPrefixOnHTTPS(t *testing.T) {
	rec := httptest.NewRecorder()
	auth.SetSessionCookie(rec, "tok", true)
	c := rec.Result().Cookies()[0]
	if c.Name != "__Host-gotcha_session" {
		t.Fatalf("secure cookie name = %q, want __Host-gotcha_session", c.Name)
	}
	if !c.Secure || c.Path != "/" || c.Domain != "" {
		t.Fatalf("__Host- prefix requires Secure+Path=/+no Domain: %+v", c)
	}

	// На plain-http — обычное имя (иначе логин на self-hosted http сломается).
	rec = httptest.NewRecorder()
	auth.SetSessionCookie(rec, "tok", false)
	if c := rec.Result().Cookies()[0]; c.Name != auth.CookieName {
		t.Fatalf("insecure cookie name = %q, want %q", c.Name, auth.CookieName)
	}
}

// ClearSessionCookie должна стирать оба имени (и http-, и https-вариант),
// чтобы logout работал после смены схемы.
func TestClearSessionCookieBothNames(t *testing.T) {
	rec := httptest.NewRecorder()
	auth.ClearSessionCookie(rec)
	cookies := rec.Result().Cookies()
	names := map[string]bool{}
	for _, c := range cookies {
		if c.MaxAge != -1 {
			t.Fatalf("clear cookie %q: MaxAge=%d, want -1", c.Name, c.MaxAge)
		}
		names[c.Name] = true
	}
	if !names[auth.CookieName] || !names["__Host-gotcha_session"] {
		t.Fatalf("clear must reset both names, got %v", names)
	}
}

// ReadSessionToken на plain-http (secure=false) понимает оба имени —
// и префиксное (https), и обычное — ради смены схемы без разлогина.
func TestReadSessionToken(t *testing.T) {
	// Под префиксным именем.
	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(&http.Cookie{Name: "__Host-gotcha_session", Value: "hosttok"})
	if tok, ok := auth.ReadSessionToken(req, false); !ok || tok != "hosttok" {
		t.Fatalf("read __Host- cookie: tok=%q ok=%v", tok, ok)
	}

	// Под обычным именем.
	req = httptest.NewRequest("GET", "/", nil)
	req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: "plaintok"})
	if tok, ok := auth.ReadSessionToken(req, false); !ok || tok != "plaintok" {
		t.Fatalf("read plain cookie: tok=%q ok=%v", tok, ok)
	}

	// Оба присутствуют → приоритет у префиксного.
	req = httptest.NewRequest("GET", "/", nil)
	req.AddCookie(&http.Cookie{Name: "__Host-gotcha_session", Value: "hosttok"})
	req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: "plaintok"})
	if tok, ok := auth.ReadSessionToken(req, false); !ok || tok != "hosttok" {
		t.Fatalf("both cookies: tok=%q ok=%v, want hosttok", tok, ok)
	}

	// Ни одного → ok=false.
	req = httptest.NewRequest("GET", "/", nil)
	if _, ok := auth.ReadSessionToken(req, false); ok {
		t.Fatalf("no cookie must return ok=false")
	}
}

// RA-L1: на HTTPS (secure=true) ReadSessionToken читает ТОЛЬКО префиксный
// __Host-; непрефиксный gotcha_session игнорируется — иначе поддомен/MITM на
// plain-http мог бы навязать pre-login session-fixation через непрефиксную cookie.
func TestReadSessionTokenSecureOnlyHostPrefix(t *testing.T) {
	// Только непрефиксная cookie на HTTPS → не читаем.
	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: "plaintok"})
	if tok, ok := auth.ReadSessionToken(req, true); ok {
		t.Fatalf("secure=true must ignore non-__Host- cookie: tok=%q ok=%v", tok, ok)
	}

	// Префиксная cookie на HTTPS → читаем.
	req = httptest.NewRequest("GET", "/", nil)
	req.AddCookie(&http.Cookie{Name: "__Host-gotcha_session", Value: "hosttok"})
	if tok, ok := auth.ReadSessionToken(req, true); !ok || tok != "hosttok" {
		t.Fatalf("secure=true must read __Host- cookie: tok=%q ok=%v", tok, ok)
	}

	// Обе на HTTPS → берём префиксную, непрефиксную не подхватываем.
	req = httptest.NewRequest("GET", "/", nil)
	req.AddCookie(&http.Cookie{Name: "__Host-gotcha_session", Value: "hosttok"})
	req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: "plaintok"})
	if tok, ok := auth.ReadSessionToken(req, true); !ok || tok != "hosttok" {
		t.Fatalf("secure=true both cookies: tok=%q ok=%v, want hosttok", tok, ok)
	}
}

// RA-L1: RequireUser на secure-инстансе (Service.Secure=true) не должен
// принимать непрефиксную cookie.
func TestRequireUserSecureIgnoresPlainCookie(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := auth.NewService(pool)
	svc.Secure = true
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	uid, err := svc.Register(ctx, "mwsec@example.com", "hunter2hunter2")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	token, err := svc.CreateSession(ctx, uid)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h := svc.RequireUser(inner)

	// Валидный токен, но под непрефиксным именем → на secure отвергаем.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/issues", nil)
	req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: token})
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("secure + plain cookie: code=%d, want 303 redirect", rec.Code)
	}

	// Тот же токен под префиксным именем → пропускаем.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/issues", nil)
	req.AddCookie(&http.Cookie{Name: "__Host-gotcha_session", Value: token})
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("secure + __Host- cookie: code=%d, want 200", rec.Code)
	}
}
