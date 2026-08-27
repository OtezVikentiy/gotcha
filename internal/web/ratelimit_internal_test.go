package web

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

func mustCIDR(t *testing.T, s string) *net.IPNet {
	t.Helper()
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		t.Fatalf("ParseCIDR(%q): %v", s, err)
	}
	return n
}

// TestClientIP закрепляет вывод реального IP клиента за/без доверенного прокси
// (SEC-L2). Ключевой инвариант безопасности: X-Forwarded-For доверяется ТОЛЬКО
// когда непосредственный пир входит в TrustedProxies — иначе клиент тривиально
// подделал бы заголовок и обошёл per-IP лимитер.
func TestClientIP(t *testing.T) {
	trusted := []*net.IPNet{mustCIDR(t, "10.0.0.0/8"), mustCIDR(t, "192.168.0.0/16")}
	cases := []struct {
		name              string
		trusted           []*net.IPNet
		remote, xff, want string
	}{
		{"no trusted proxies -> RemoteAddr, XFF ignored", nil, "203.0.113.7:1234", "1.2.3.4", "203.0.113.7"},
		{"trusted peer -> client from XFF", trusted, "10.1.2.3:9", "203.0.113.9", "203.0.113.9"},
		{"trusted peer -> rightmost non-trusted in XFF chain", trusted, "10.1.2.3:9", "203.0.113.9, 10.9.9.9", "203.0.113.9"},
		{"untrusted peer -> XFF ignored (spoofing blocked)", trusted, "203.0.113.50:9", "1.2.3.4", "203.0.113.50"},
		{"trusted peer, empty XFF -> peer", trusted, "10.1.2.3:9", "", "10.1.2.3"},
		{"trusted peer, all XFF hops trusted -> peer", trusted, "10.1.2.3:9", "10.9.9.9, 192.168.1.1", "10.1.2.3"},
		{"trusted peer, garbage XFF token skipped", trusted, "10.1.2.3:9", "not-an-ip, 203.0.113.5", "203.0.113.5"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := &Handler{TrustedProxies: c.trusted}
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.RemoteAddr = c.remote
			if c.xff != "" {
				r.Header.Set("X-Forwarded-For", c.xff)
			}
			if got := h.clientIP(r); got != c.want {
				t.Errorf("clientIP = %q, want %q", got, c.want)
			}
		})
	}
}

// TestRateLimitKey — ключ per-account лимитера: clientIP + '|' + нормализованный email.
func TestRateLimitKey(t *testing.T) {
	h := &Handler{}
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "203.0.113.7:1234"
	if got := h.rateLimitKey(r, "  User@Example.COM "); got != "203.0.113.7|user@example.com" {
		t.Errorf("rateLimitKey = %q, want %q", got, "203.0.113.7|user@example.com")
	}
}

// TestRateLimitKeyOversizedEmail — находка W1-C: поле email из формы логина
// ничем не ограничено на входе, и попадает в rl.hits ДО того, как отработает
// per-IP лимитер (см. loginSubmit). Без схлопывания в общий ключ каждая
// огромная попытка получала бы собственный ключ карты — рост карты на
// мегабайты за запрос.
func TestRateLimitKeyOversizedEmail(t *testing.T) {
	h := &Handler{}
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "203.0.113.7:1234"

	huge1 := strings.Repeat("a", 1<<20) // 1 МиБ
	huge2 := strings.Repeat("b", 1<<20) // другой мусор, тот же размер

	got1 := h.rateLimitKey(r, huge1)
	got2 := h.rateLimitKey(r, huge2)

	const want = "203.0.113.7|oversized"
	if got1 != want {
		t.Errorf("rateLimitKey(huge1) = %q, want %q", got1, want)
	}
	if got2 != want {
		t.Errorf("rateLimitKey(huge2) = %q, want %q", got2, want)
	}
	if got1 != got2 {
		t.Errorf("два разных огромных email с одного IP дали разные ключи: %q != %q", got1, got2)
	}
	if len(got1) > 64 {
		t.Errorf("ключ на выходе не должен нести присланный мусор, длина = %d", len(got1))
	}
}

// TestRateLimitKeyLengthBoundary — граница из RFC 5321: 254 байта — ещё
// валидная длина email (ключ строится как есть), 255 — уже общее ведро.
func TestRateLimitKeyLengthBoundary(t *testing.T) {
	h := &Handler{}
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "203.0.113.7:1234"

	local254 := strings.Repeat("a", 254-len("@x.com")) + "@x.com"
	if len(local254) != 254 {
		t.Fatalf("тестовая заготовка: len(local254) = %d, want 254", len(local254))
	}
	if got, want := h.rateLimitKey(r, local254), "203.0.113.7|"+local254; got != want {
		t.Errorf("email длиной 254 байта: rateLimitKey = %q, want %q (не должен схлопываться)", got, want)
	}

	local255 := local254 + "x"
	if got, want := h.rateLimitKey(r, local255), "203.0.113.7|oversized"; got != want {
		t.Errorf("email длиной 255 байт: rateLimitKey = %q, want %q (обязан схлопнуться)", got, want)
	}
}

// TestRateLimitKeyNormalEmailUnchanged — регрессия на п.1 брифа: обычный
// email по-прежнему даёт прежний ключ (нижний регистр, обрезанные пробелы).
func TestRateLimitKeyNormalEmailUnchanged(t *testing.T) {
	h := &Handler{}
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "198.51.100.3:9"
	if got, want := h.rateLimitKey(r, "  Alice@Example.COM  "), "198.51.100.3|alice@example.com"; got != want {
		t.Errorf("rateLimitKey = %q, want %q", got, want)
	}
}

// authTestHandler собирает Handler с полным набором зависимостей
// loginSubmit/registerSubmit: реальный Auth на тестовой PG (не nil — иначе
// без MaxBytesReader код успевает дойти до h.Auth.Authenticate и падает
// nil-паникой раньше своего же ассерта, маскируя настоящую причину, см.
// ревью W1-C находка B) и все три лимитера с боевыми лимитами.
func authTestHandler(t *testing.T) *Handler {
	t.Helper()
	pool := testenv.MigratedPG(t)
	return &Handler{
		BaseURL:      "http://gotcha.example",
		Auth:         auth.NewService(pool),
		loginLimiter: newRateLimiter(time.Now, 5, time.Minute),
		ipLimiter:    newRateLimiter(time.Now, 20, time.Minute),
		emailLimiter: newRateLimiter(time.Now, 50, 15*time.Minute),
	}
}

// TestLoginSubmitOversizedBodyRejected — находка W1-C, пункт 3: тело запроса
// логина больше authFormMaxBodyBytes должно отбиваться на ParseForm (тем же
// путём, что и любая другая битая форма), а не доходить до rateLimitKey и
// раздувать карту лимитера, и уж тем более не 500/паника. Фикстура —
// authTestHandler с полностью живыми зависимостями: при мутации (снять
// MaxBytesReader) код парсит форму успешно и доходит до реальной
// аутентификации, так что регрессия должна ловиться на статусе/размере
// карты, а не панике на nil-лимитере/nil-Auth.
func TestLoginSubmitOversizedBodyRejected(t *testing.T) {
	h := authTestHandler(t)

	body := "email=" + strings.Repeat("a", authFormMaxBodyBytes*2) + "@x.com&password=x"
	r := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", h.BaseURL)

	rec := httptest.NewRecorder()
	h.loginSubmit(rec, r)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d (ошибка разбора формы, не 500/паника)", rec.Code, http.StatusBadRequest)
	}
	if got := h.loginLimiter.size(); got != 0 {
		t.Errorf("loginLimiter.size() = %d, want 0 — тело сверх лимита не должно доходить до rateLimitKey", got)
	}
	if got := h.emailLimiter.size(); got != 0 {
		t.Errorf("emailLimiter.size() = %d, want 0 — тело сверх лимита не должно доходить до limiterEmailKeyPart", got)
	}
}

// TestEmailLimiterKeyCappedInLoginSubmit — находка ревью W1-C (A): ключ
// h.emailLimiter строится не через rateLimitKey (там есть IP), а напрямую из
// email в loginSubmit — этот путь тоже обязан капать длину, иначе перебор с
// пула IP (обходящего per-IP лимитер) засорял бы emailLimiter.hits ключами
// вплоть до размера authFormMaxBodyBytes вместо ≤254 байт.
//
// emailLimiter здесь намеренно с limit=0: Allow всегда возвращает false и
// сохраняет пришедший ключ (см. комментарий про rl.hits[key] = fresh в
// ratelimit.go) — это форсирует ветку 429 сразу после проверки лимитов, не
// давая исполнению дойти до h.Auth.Authenticate, и одновременно даёт
// заглянуть в размер карты, чтобы убедиться, что оба гигантских email легли
// в один и тот же ключ, а не в два разных.
func TestEmailLimiterKeyCappedInLoginSubmit(t *testing.T) {
	h := authTestHandler(t)
	h.emailLimiter = newRateLimiter(time.Now, 0, time.Minute)

	huge1 := strings.Repeat("a", 2000) + "@x.com" // > maxEmailKeyBytes, < authFormMaxBodyBytes
	huge2 := strings.Repeat("b", 2000) + "@x.com"

	post := func(remoteAddr, email string) int {
		body := "email=" + email + "&password=x"
		r := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Origin", h.BaseURL)
		r.RemoteAddr = remoteAddr
		rec := httptest.NewRecorder()
		h.loginSubmit(rec, r)
		return rec.Code
	}

	if code := post("203.0.113.7:1", huge1); code != http.StatusTooManyRequests {
		t.Fatalf("первый запрос: status = %d, want 429 (emailLimiter с limit=0 обязан отказать)", code)
	}
	if code := post("198.51.100.9:1", huge2); code != http.StatusTooManyRequests {
		t.Fatalf("второй запрос с другого IP: status = %d, want 429", code)
	}

	if got := h.emailLimiter.size(); got != 1 {
		t.Errorf("emailLimiter.size() = %d, want 1 — два огромных email с разных IP обязаны схлопнуться в один ключ", got)
	}
}

// TestEmailLimiterKeyCappedInRegisterSubmit — тот же дефект-класс, что и
// TestEmailLimiterKeyCappedInLoginSubmit, но по пути registerSubmit
// (там ключ emailLimiter раньше строился через normalizeEmail(email) без
// капа по длине).
func TestEmailLimiterKeyCappedInRegisterSubmit(t *testing.T) {
	h := authTestHandler(t)
	h.emailLimiter = newRateLimiter(time.Now, 0, time.Minute)

	huge1 := strings.Repeat("a", 2000) + "@x.com"
	huge2 := strings.Repeat("b", 2000) + "@x.com"

	post := func(remoteAddr, email string) int {
		body := "email=" + email + "&password=x&password2=x"
		r := httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Origin", h.BaseURL)
		r.RemoteAddr = remoteAddr
		rec := httptest.NewRecorder()
		h.registerSubmit(rec, r)
		return rec.Code
	}

	if code := post("203.0.113.7:1", huge1); code != http.StatusTooManyRequests {
		t.Fatalf("первый запрос: status = %d, want 429 (emailLimiter с limit=0 обязан отказать)", code)
	}
	if code := post("198.51.100.9:1", huge2); code != http.StatusTooManyRequests {
		t.Fatalf("второй запрос с другого IP: status = %d, want 429", code)
	}

	if got := h.emailLimiter.size(); got != 1 {
		t.Errorf("emailLimiter.size() = %d, want 1 — два огромных email с разных IP обязаны схлопнуться в один ключ", got)
	}
}

// TestLimiterEmailKeyPartNormalUnchanged — регрессия: обычный email через
// limiterEmailKeyPart даёт тот же результат, что раньше давали
// strings.ToLower(strings.TrimSpace(email)) в loginSubmit и
// normalizeEmail(email) в registerSubmit.
func TestLimiterEmailKeyPartNormalUnchanged(t *testing.T) {
	if got, want := limiterEmailKeyPart("  Alice@Example.COM  "), "alice@example.com"; got != want {
		t.Errorf("limiterEmailKeyPart = %q, want %q", got, want)
	}
}
