package web

import (
	"fmt"
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
		loginLimiter: newRateLimiter(time.Now, 5, time.Minute, loginLimiterMaxKeys, "loginLimiter"),
		ipLimiter:    newRateLimiter(time.Now, 20, time.Minute, ipLimiterMaxKeys, "ipLimiter"),
		emailLimiter: newRateLimiter(time.Now, 50, 15*time.Minute, emailLimiterMaxKeys, "emailLimiter"),
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
//
// Статус — 413 (K7-4): h.parseForm распознаёт http.MaxBytesError и отвечает
// 413, а не общим 400, — превышение предела тела отличимо от произвольно
// битой формы. До K7-4 здесь ожидался 400 (h.parseForm ещё не существовал,
// h.renderError звался напрямую без разбора причины ParseForm).
func TestLoginSubmitOversizedBodyRejected(t *testing.T) {
	h := authTestHandler(t)

	body := "email=" + strings.Repeat("a", authFormMaxBodyBytes*2) + "@x.com&password=x"
	r := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", h.BaseURL)

	rec := httptest.NewRecorder()
	h.loginSubmit(rec, r)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want %d (тело сверх authFormMaxBodyBytes — 413, не 500/паника)", rec.Code, http.StatusRequestEntityTooLarge)
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
	h.emailLimiter = newRateLimiter(time.Now, 0, time.Minute, emailLimiterMaxKeys, "emailLimiter")

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
	h.emailLimiter = newRateLimiter(time.Now, 0, time.Minute, emailLimiterMaxKeys, "emailLimiter")

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

// TestRateLimiterCapDeniesNewKeysAtCapacity — находка W2-B (доработка после
// ревью): поток из МНОГО РАЗНЫХ свежих ключей (время не двигается,
// принудительная уборка на потолке ничего не вычистит) не должен раздувать
// карту лимитера безгранично. Никакого общего overflow-ведра больше нет —
// после того как карта дошла до maxKeys, каждый НЕВИДЕННЫЙ ранее ключ
// получает отказ (false) и не заводит запись; карта не растёт ни на один
// ключ сверх потолка.
func TestRateLimiterCapDeniesNewKeysAtCapacity(t *testing.T) {
	now := time.Now()
	const capacity = 5
	rl := newRateLimiter(func() time.Time { return now }, 5, time.Minute, capacity, "test")

	for i := 0; i < capacity; i++ {
		key := fmt.Sprintf("203.0.113.%d:%d", i, i)
		if !rl.Allow(key) {
			t.Fatalf("заполнение потолка, ключ %d из %d: want true (карта ещё не на потолке)", i, capacity)
		}
	}
	if got := rl.size(); got != capacity {
		t.Fatalf("тестовая заготовка: size() = %d, want %d (карта на потолке)", got, capacity)
	}

	// Карта на потолке. Дальше — сплошь НЕВИДЕННЫЕ ключи: все обязаны
	// получить отказ, ни один не должен завести запись.
	for i := capacity; i < capacity+50; i++ {
		key := fmt.Sprintf("203.0.113.%d:%d", i, i)
		if rl.Allow(key) {
			t.Errorf("ключ %d сверх потолка: want false (потолок исчерпан)", i)
		}
	}
	if got := rl.size(); got != capacity {
		t.Errorf("size() после потока лишних ключей = %d, want %d (потолок не должен пробиваться)", got, capacity)
	}
}

// TestRateLimiterCapAllowsExistingKeysAtCapacity — находка ревью W2-B: гвард
// !exists в условии переполнения (см. Allow) обязан отличать НЕВИДЕННЫЙ ключ
// от УЖЕ существующего — карта на потолке не должна начинать отказывать
// собственным известным ключам, у них есть свой счётчик limit/window.
// Без гварда карта "на потолке" резала бы вообще любой Allow, в том числе
// повторные попытки уже учтённых клиентов, — это и есть регресс из брифа
// W2-B п.1 ("существующие ключи под потолком обязаны работать как раньше").
func TestRateLimiterCapAllowsExistingKeysAtCapacity(t *testing.T) {
	now := time.Now()
	const capacity = 3
	const limit = 5
	rl := newRateLimiter(func() time.Time { return now }, limit, time.Minute, capacity, "test")

	keys := []string{"k1", "k2", "k3"}
	for _, k := range keys {
		if !rl.Allow(k) {
			t.Fatalf("заполнение потолка, ключ %q: want true", k)
		}
	}
	if got := rl.size(); got != capacity {
		t.Fatalf("тестовая заготовка: size() = %d, want %d (карта на потолке)", got, capacity)
	}

	// k2 уже в карте. limit-1 дополнительных попыток обязаны пройти по
	// СОБСТВЕННОМУ счётчику k2, а не наткнуться на потолок карты.
	for i := 0; i < limit-1; i++ {
		if !rl.Allow("k2") {
			t.Fatalf("k2, дополнительная попытка %d из %d: want true (существующий ключ под потолком продолжает работать своим счётчиком)", i+1, limit-1)
		}
	}
	// Теперь у k2 ровно limit попаданий — следующая упирается в ОБЫЧНЫЙ
	// лимит (5/мин), а не в потолок карты.
	if rl.Allow("k2") {
		t.Errorf("k2 после исчерпания limit=%d: want false (обычный лимит, не потолок карты)", limit)
	}

	// А вот НОВЫЙ ключ на заполненной карте обязан получить отказ по потолку.
	if rl.Allow("k4") {
		t.Errorf("k4 (невиданный ключ на заполненной карте): want false — потолок обязан отказать")
	}
	if got := rl.size(); got != capacity {
		t.Errorf("size() = %d, want %d — ни повтор k2, ни отказанный k4 не должны были менять число ключей", got, capacity)
	}
}

// TestRateLimiterForcedSweepAtCapacityThrottled — находка повторного ревью
// W2-B: принудительная уборка на упоре в потолок (см. Allow) обязана быть
// ограничена по частоте ТАК ЖЕ, как фоновая (sweepThreshold) — общим
// lastSweep/rl.window. Без этого КАЖДЫЙ запрос поверх потолка гоняет полный
// обход карты под мьютексом — та самая O(n), ради снятия которой троттлинг
// вводился, только теперь с усилением: атакующий, удерживающий карту на
// потолке, дешёвым запросом заставляет сервер обходить maxKeys элементов, а
// мьютекс на это время закрыт для чужих легитимных запросов на том же
// лимитере. Карта заполняется до потолка, время не двигается, дальше — поток
// НЕВИДЕННЫХ ключей: число вызовов sweepExpired обязано остаться
// ограниченным, а не расти вместе с числом запросов.
func TestRateLimiterForcedSweepAtCapacityThrottled(t *testing.T) {
	now := time.Now()
	const capacity = 5
	rl := newRateLimiter(func() time.Time { return now }, 5, time.Minute, capacity, "test")

	for i := 0; i < capacity; i++ {
		if !rl.Allow(fmt.Sprintf("k%d", i)) {
			t.Fatalf("заполнение потолка, ключ %d: want true", i)
		}
	}
	before := rl.sweepCalls

	// Карта на потолке, время неподвижно — поток НЕВИДЕННЫХ ключей, каждый
	// обязан упереться в потолок (см. TestRateLimiterCapDeniesNewKeysAtCapacity).
	for i := 0; i < 500; i++ {
		rl.Allow(fmt.Sprintf("attacker%d", i))
	}

	if got := rl.sweepCalls - before; got != 1 {
		t.Errorf("sweepCalls вырос на %d за 500 запросов поверх потолка при неподвижном времени, want 1 (принудительная уборка обязана троттлиться так же, как фоновая — не чаще раза в rl.window)", got)
	}
}

// TestRateLimiterSweepThrottledByInterval — полный обход карты (sweepExpired)
// не должен запускаться на каждом вызове Allow за sweepThreshold — только не
// чаще раза в rl.window. maxKeys здесь заведомо больше числа вставляемых
// ключей, чтобы потолок carты не вмешивался — тест изолированно проверяет
// именно троттлинг ФОНОВОЙ уборки, не принудительную уборку на потолке (она
// покрыта TestRateLimiterCapDeniesNewKeysAtCapacity). Время в тесте не
// двигается вовсе, так что после первой уборки все последующие вызовы за
// порогом обязаны её пропускать.
func TestRateLimiterSweepThrottledByInterval(t *testing.T) {
	now := time.Now()
	rl := newRateLimiter(func() time.Time { return now }, 5, time.Minute, 50000, "test")

	// sweepThreshold+2 звонков: первые sweepThreshold+1 доводят карту РОВНО
	// до sweepThreshold+1 записей (проверка "> sweepThreshold" смотрит на
	// размер ДО вставки текущего ключа), звонок sweepThreshold+2 — первый,
	// который застаёт карту уже за порогом и запускает уборку.
	for i := 0; i < sweepThreshold+2; i++ {
		rl.Allow(fmt.Sprintf("198.51.100.%d:%d", i%256, i))
	}
	if got := rl.sweepCalls; got != 1 {
		t.Fatalf("после первого пересечения sweepThreshold: sweepCalls = %d, want 1", got)
	}

	for i := 0; i < 500; i++ {
		rl.Allow(fmt.Sprintf("198.51.100.%d:extra%d", i%256, i))
	}
	if got := rl.sweepCalls; got != 1 {
		t.Errorf("время не двигалось: sweepCalls = %d, want 1 (уборка не чаще раза в window)", got)
	}
}

// TestRateLimiterExpiredKeysStillSwept — регрессия: движение времени вперёд
// за пределы window по-прежнему освобождает карту от истёкших ключей — сам
// механизм уборки правкой не сломан, только частота её запуска ограничена.
// maxKeys заведомо выше числа вставляемых ключей — потолок carты здесь не
// участвует, см. комментарий у TestRateLimiterSweepThrottledByInterval.
func TestRateLimiterExpiredKeysStillSwept(t *testing.T) {
	now := time.Now()
	clock := &now
	rl := newRateLimiter(func() time.Time { return *clock }, 5, time.Minute, 50000, "test")

	for i := 0; i < sweepThreshold+2; i++ {
		rl.Allow(fmt.Sprintf("192.0.2.%d:%d", i%256, i))
	}
	before := rl.size()
	if before == 0 {
		t.Fatalf("тестовая заготовка: карта пуста после наполнения")
	}
	if got := rl.sweepCalls; got != 1 {
		t.Fatalf("тестовая заготовка: sweepCalls после наполнения = %d, want 1", got)
	}

	*clock = clock.Add(2 * time.Minute)
	if !rl.Allow("fresh-after-expiry") {
		t.Fatalf("Allow после сдвига времени: want true")
	}

	if got := rl.size(); got >= before {
		t.Errorf("size() после сдвига времени за window = %d, want < %d (истёкшие ключи должны были уйти)", got, before)
	}
	if got := rl.sweepCalls; got != 2 {
		t.Errorf("sweepCalls после сдвига времени = %d, want 2 (новая уборка после истечения интервала)", got)
	}
}

// TestRateLimiterNormalUsageUnaffectedByCap — регрессия: обычный сценарий с
// малым числом ключей (далеко под потолком) продолжает соблюдать limit/window
// ровно как до правки — потолок не должен задевать штатный трафик.
func TestRateLimiterNormalUsageUnaffectedByCap(t *testing.T) {
	now := time.Now()
	rl := newRateLimiter(func() time.Time { return now }, 3, time.Minute, 1000, "test")

	for i := 0; i < 3; i++ {
		if !rl.Allow("client-a") {
			t.Fatalf("попытка %d для client-a: want true (в пределах limit)", i+1)
		}
	}
	if rl.Allow("client-a") {
		t.Errorf("4-я попытка для client-a: want false (лимит исчерпан)")
	}
	if !rl.Allow("client-b") {
		t.Errorf("первая попытка для client-b: want true — у другого ключа свой счётчик")
	}
	if got, want := rl.size(), 2; got != want {
		t.Errorf("size() = %d, want %d", got, want)
	}
}

// TestLoginSubmitIPLimiterBoundsLoginLimiterGrowth — находка ревью W2-B,
// рабочий эксплойт: loginSubmit проверял per-account (ip|email) лимитер
// ПЕРВЫМ операндом || — раньше дешёвого per-IP лимитера. Один IP с потоком
// выдуманных email заполнял карту loginLimiter быстрее, чем успевал
// сработать ipLimiter, — 20000 POST с одной машины клали логин ВСЕЙ
// инсталляции переполнением карты (после чего каждый новый легитимный
// пользователь падал в отказ по потолку). Теперь ipLimiter (лимит 20/мин,
// ключ — clientIP) проверяется ПЕРВЫМ: один IP не может завести в
// loginLimiter больше ключей, чем разрешает ipLimiter за окно.
func TestLoginSubmitIPLimiterBoundsLoginLimiterGrowth(t *testing.T) {
	h := authTestHandler(t) // ipLimiter: limit 20, window 1 минута

	post := func(email string) int {
		body := "email=" + email + "@x.com&password=x"
		r := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Origin", h.BaseURL)
		r.RemoteAddr = "203.0.113.7:1" // один и тот же IP на все попытки
		rec := httptest.NewRecorder()
		h.loginSubmit(rec, r)
		return rec.Code
	}

	const attempts = 50
	const ipLimit = 20 // см. authTestHandler: ipLimiter limit
	var limited int
	for i := 0; i < attempts; i++ {
		if code := post(fmt.Sprintf("attacker%d", i)); code == http.StatusTooManyRequests {
			limited++
		}
	}

	if want := attempts - ipLimit; limited != want {
		t.Errorf("отказов 429 = %d, want %d — ipLimiter обязан пропускать ровно %d запросов с одного IP в минуту, остальные отсекать ДО loginLimiter", limited, want, ipLimit)
	}
	if got := h.loginLimiter.size(); got != ipLimit {
		t.Errorf("loginLimiter.size() = %d, want %d — один IP не должен заводить в loginLimiter больше ключей, чем разрешает ipLimiter", got, ipLimit)
	}
}

// TestSSOSubmitIPLimiterBoundsLoginLimiterGrowth — находка ревью W2-B,
// «третья дверь»: ssoSubmit проверял loginLimiter БЕЗ ipLimiter перед ним, а
// loginLimiter — ОДИН инстанс на /login, /register и /sso (plus profile.go).
// Закрыв порядок только в loginSubmit/registerSubmit, обходной путь к тому
// же отказу аутентификации оставался открыт через /sso: один IP потоком
// выдуманных email заполнял ТУ ЖЕ карту loginLimiter, что потом резала
// первую попытку входа любого реального пользователя на /login. Теперь
// ipLimiter (лимит 20/мин, ключ — clientIP) проверяется первым и здесь.
//
// Фикстура — ssoTestHandler (sso_internal_test.go), собирающая Handler через
// New() с боевыми лимитами: ssoSubmit после лимитеров доходит до
// h.Org.SSOByDomain, а Handler.Org — конкретный *org.Service, не интерфейс,
// nil-значение там паникует на разыменовании внутреннего пула соединений.
func TestSSOSubmitIPLimiterBoundsLoginLimiterGrowth(t *testing.T) {
	h, _, _, _ := ssoTestHandler(t) // ipLimiter: limit 20, window 1 минута (New())

	post := func(email string) int {
		body := "email=" + email + "@x.com"
		r := httptest.NewRequest(http.MethodPost, "/sso", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Origin", h.BaseURL)
		r.RemoteAddr = "203.0.113.7:1" // один и тот же IP на все попытки
		rec := httptest.NewRecorder()
		h.ssoSubmit(rec, r)
		return rec.Code
	}

	const attempts = 50
	const ipLimit = 20 // см. web.go New(): ipLimiter limit
	var limited int
	for i := 0; i < attempts; i++ {
		if code := post(fmt.Sprintf("attacker%d", i)); code == http.StatusTooManyRequests {
			limited++
		}
	}

	if want := attempts - ipLimit; limited != want {
		t.Errorf("отказов 429 = %d, want %d — ipLimiter обязан пропускать ровно %d запросов с одного IP в минуту через /sso, остальные отсекать ДО loginLimiter", limited, want, ipLimit)
	}
	if got := h.loginLimiter.size(); got != ipLimit {
		t.Errorf("loginLimiter.size() = %d, want %d — один IP не должен заводить в loginLimiter больше ключей через /sso, чем разрешает ipLimiter", got, ipLimit)
	}
}
