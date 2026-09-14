package web

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/web/flashctx"
)

func TestFlashRoundTrip(t *testing.T) {
	h := &Handler{BaseURL: "https://gotcha.example", Secure: true}

	rec := httptest.NewRecorder()
	h.flashOK(rec, "flash.saved", 0)
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != flashCookie {
		t.Fatalf("cookie не поставлена: %+v", cookies)
	}
	if !cookies[0].HttpOnly || !cookies[0].Secure {
		t.Errorf("cookie должна быть HttpOnly и Secure на https-инстансе: %+v", cookies[0])
	}

	var seen *flashctx.Flash
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen = flashctx.FromContext(r.Context())
	})
	req := httptest.NewRequest(http.MethodGet, "/projects/1/issues", nil)
	req.AddCookie(cookies[0])
	rec2 := httptest.NewRecorder()
	h.withFlash(next).ServeHTTP(rec2, req)

	if seen == nil || seen.Key != "flash.saved" || seen.Kind != "ok" {
		t.Fatalf("сообщение не доехало до обработчика: %+v", seen)
	}
	var cleared bool
	for _, c := range rec2.Result().Cookies() {
		if c.Name == flashCookie && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Error("cookie не погашена — сообщение показалось бы повторно при F5")
	}
}

func TestFlashRejectsForgedCookie(t *testing.T) {
	forged := []string{
		"ok|Ваш+пароль+истёк,+введите+его+заново",
		"ok|flash.unknown_key",
		"evil|flash.saved",
		"flash.saved",
		"",
		"ok|flash.saved|not-a-number",
	}
	for _, v := range forged {
		f := parseFlash(v)
		switch v {
		case "ok|flash.saved|not-a-number":
			if f == nil || f.N != 0 {
				t.Errorf("валидный ключ с мусорным числом: %+v", f)
			}
		default:
			if f != nil {
				t.Errorf("подделка %q принята: %+v", v, f)
			}
		}
	}
}

func TestFlashPluralCarriesCount(t *testing.T) {
	rec := httptest.NewRecorder()
	h := &Handler{BaseURL: "http://localhost:59080"}
	h.flashOK(rec, "flash.issues_resolved", 5)

	c := rec.Result().Cookies()[0]
	if c.Secure {
		t.Error("на http-инстансе Secure не ставится")
	}
	f := parseFlash(c.Value)
	if f == nil || f.N != 5 {
		t.Fatalf("число не доехало: %+v", f)
	}
	// Отрицательное число отбрасывается: форм множественного числа для него нет.
	if f := parseFlash("ok|flash.issues_resolved|-3"); f == nil || f.N != 0 {
		t.Errorf("отрицательное число должно игнорироваться: %+v", f)
	}
}

func TestFlashPairCarriesBothCounts(t *testing.T) {
	rec := httptest.NewRecorder()
	h := &Handler{BaseURL: "http://localhost:59080"}
	h.flashOKPair(rec, "flash.recipes_applied", 3, 2)

	c := rec.Result().Cookies()[0]
	f := parseFlash(c.Value)
	if f == nil || !f.Pair || f.N != 3 || f.M != 2 || f.Kind != "ok" {
		t.Fatalf("парный флеш не доехал: %+v", f)
	}

	rec = httptest.NewRecorder()
	h.flashOKPair(rec, "flash.recipes_applied", 0, 3)
	f = parseFlash(rec.Result().Cookies()[0].Value)
	if f == nil || !f.Pair || f.N != 0 || f.M != 3 {
		t.Fatalf("нулевой created сдвинул счётчики: %+v", f)
	}

	if f := parseFlash("ok|flash.recipes_applied|2|-7"); f == nil || f.M != 0 || f.N != 2 {
		t.Errorf("отрицательный M должен игнорироваться: %+v", f)
	}

	if f := parseFlash("ok|flash.issues_resolved|5"); f == nil || f.Pair || f.N != 5 || f.M != 0 {
		t.Errorf("старый формат сломан или ошибочно помечен Pair: %+v", f)
	}
}

// h.Secure — единственный источник истины для флага Secure у cookie; пересчёт из
// BaseURL заново дал бы второй источник, способный разойтись с первым.
func TestFlashSecureFollowsHandlerSecureField(t *testing.T) {
	h := &Handler{BaseURL: "https://gotcha.example", Secure: false}

	rec := httptest.NewRecorder()
	h.flashOK(rec, "flash.saved", 0)
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("cookie не поставлена: %+v", cookies)
	}
	if cookies[0].Secure {
		t.Errorf("cookie.Secure = true при h.Secure=false: %+v", cookies[0])
	}
}

func TestFlashSkipsStatic(t *testing.T) {
	h := &Handler{BaseURL: "https://gotcha.example"}
	req := httptest.NewRequest(http.MethodGet, "/static/app.css", nil)
	req.AddCookie(&http.Cookie{Name: flashCookie, Value: "ok%7Cflash.saved"})
	rec := httptest.NewRecorder()

	called := false
	h.withFlash(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		called = true
		if flashctx.FromContext(r.Context()) != nil {
			t.Error("на статике сообщение читать не нужно")
		}
	})).ServeHTTP(rec, req)

	if !called {
		t.Fatal("запрос не пропущен дальше")
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == flashCookie && c.MaxAge < 0 {
			t.Error("статика погасила сообщение до того, как его показала страница")
		}
	}
}

func TestFlashUnknownKeyNotSet(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelError})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	rec := httptest.NewRecorder()
	(&Handler{}).flashOK(rec, "flash.typo", 0)
	if n := len(rec.Result().Cookies()); n != 0 {
		t.Errorf("неизвестный ключ поставил cookie (%d)", n)
	}
	if strings.Contains(rec.Header().Get("Set-Cookie"), flashCookie) {
		t.Error("неизвестный ключ не должен ставить cookie")
	}
}

func TestSetFlashUnknownKeyIsLoud(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelError})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	rec := httptest.NewRecorder()
	(&Handler{}).flashOK(rec, "flash.definitely_not_in_the_list", 0)

	if buf.Len() == 0 {
		t.Fatal("неизвестный ключ прошёл молча: забытый в списке ключ никак " +
			"не отличить от несработавшей формы")
	}
	if len(rec.Result().Cookies()) != 0 {
		t.Fatal("неизвестный ключ всё-таки уехал в cookie")
	}
}
