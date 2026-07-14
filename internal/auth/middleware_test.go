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
