package web_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/issue"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"gitflic.ru/otezvikentiy/gotcha/internal/web"
)

// TestSecurityHeaders — любой ответ Handler'а несёт базовые security-заголовки
// (securityHeaders оборачивает весь mux в Register).
func TestSecurityHeaders(t *testing.T) {
	s := newStack(t)

	resp, err := http.Get(s.srv.URL + "/login")
	if err != nil {
		t.Fatalf("get /login: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if got := resp.Header.Get("X-Frame-Options"); got != "DENY" {
		t.Errorf("X-Frame-Options = %q, want DENY", got)
	}
	if got := resp.Header.Get("Referrer-Policy"); got != "same-origin" {
		t.Errorf("Referrer-Policy = %q, want same-origin", got)
	}
	wantCSP := "default-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"
	if got := resp.Header.Get("Content-Security-Policy"); got != wantCSP {
		t.Errorf("Content-Security-Policy = %q, want %q", got, wantCSP)
	}
	// newStack собирает Handler с http:// BaseURL (h.Secure == false) — HSTS
	// на голом HTTP отправлять нельзя (см. securityHeaders).
	if got := resp.Header.Get("Strict-Transport-Security"); got != "" {
		t.Errorf("Strict-Transport-Security on http:// deploy = %q, want empty", got)
	}
}

// TestSecurityHeadersHSTS — HSTS выставляется только когда BaseURL Handler'а
// начинается с https:// (h.Secure): проверяем это напрямую через
// securityHeaders, не поднимая httptest.Server с реальным TLS.
func TestSecurityHeadersHSTS(t *testing.T) {
	pool := testenv.MigratedPG(t)
	authSvc := auth.NewService(pool)
	orgSvc := org.NewService(pool, 1_000_000)
	issueSvc := issue.NewService(pool)

	h := web.New(authSvc, orgSvc, issueSvc, nil, "https://gotcha.example")
	mux := http.NewServeMux()
	h.Register(mux)

	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/login")
	if err != nil {
		t.Fatalf("get /login: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if got := resp.Header.Get("Strict-Transport-Security"); got != "max-age=31536000" {
		t.Errorf("Strict-Transport-Security = %q, want max-age=31536000", got)
	}
}

// TestStyled404Page — незарегистрированный маршрут отдаёт 404 через layout
// (не голый "404 page not found" от stdlib ServeMux), и тоже несёт
// security-заголовки.
func TestStyled404Page(t *testing.T) {
	s := newStack(t)

	resp, err := http.Get(s.srv.URL + "/this-route-does-not-exist")
	if err != nil {
		t.Fatalf("get /this-route-does-not-exist: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	if !strings.Contains(string(body), `class="chromeless-top"`) {
		t.Fatalf("404 body missing layout (chromeless top bar): %s", body)
	}
	if !strings.Contains(string(body), "Gotcha") {
		t.Fatalf("404 body missing layout (logo/title): %s", body)
	}
	if got := resp.Header.Get("X-Frame-Options"); got != "DENY" {
		t.Errorf("X-Frame-Options on 404 = %q, want DENY", got)
	}
}

// TestSecurityHeadersHSTSVariants — что положили в Handler.HSTSHeader, то и
// уходит в ответ на https-инстансе. Отдельная строка про max-age=0: это
// аварийное снятие пина, и заголовок обязан ОТПРАВЛЯТЬСЯ, а не исчезнуть
// (исчезнувший заголовок пин не снимает — браузер держит его год).
func TestSecurityHeadersHSTSVariants(t *testing.T) {
	for _, tc := range []struct{ name, header, want string }{
		{"empty means no header", "", ""},
		{"default", "max-age=31536000", "max-age=31536000"},
		{"zero is sent", "max-age=0", "max-age=0"},
		{"subdomains", "max-age=31536000; includeSubDomains", "max-age=31536000; includeSubDomains"},
		{"preload", "max-age=31536000; includeSubDomains; preload", "max-age=31536000; includeSubDomains; preload"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := web.New(nil, nil, nil, nil, "https://gotcha.example")
			h.HSTSHeader = tc.header
			mux := http.NewServeMux()
			h.Register(mux)

			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest("GET", "/this-route-does-not-exist", nil))

			// Пустое ожидание проверяется по ОТСУТСТВИЮ заголовка в карте, а не
			// через Get: Get возвращает "" и для отсутствующего заголовка, и для
			// выставленного пустым — Set(name, "") прошёл бы такую проверку
			// незамеченным, а по сети ушёл бы пустой Strict-Transport-Security.
			values, present := rec.Header()["Strict-Transport-Security"]
			if tc.want == "" {
				if present {
					t.Errorf("Strict-Transport-Security присутствует со значением %q, заголовка быть не должно", values)
				}
				return
			}
			if !present {
				t.Fatal("Strict-Transport-Security отсутствует, want " + tc.want)
			}
			if got := rec.Header().Get("Strict-Transport-Security"); got != tc.want {
				t.Errorf("Strict-Transport-Security = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestSecurityHeadersHSTSNeverOnPlainHTTP — на http-инстансе заголовка нет
// даже при полностью заполненном HSTSHeader: проверка h.Secure не зависит от
// того, что подставил main.go.
func TestSecurityHeadersHSTSNeverOnPlainHTTP(t *testing.T) {
	h := web.New(nil, nil, nil, nil, "http://gotcha.example")
	h.HSTSHeader = "max-age=31536000; includeSubDomains; preload"
	mux := http.NewServeMux()
	h.Register(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/this-route-does-not-exist", nil))

	if values, present := rec.Header()["Strict-Transport-Security"]; present {
		t.Errorf("Strict-Transport-Security on http:// deploy = %q, want no header at all", values)
	}
}
