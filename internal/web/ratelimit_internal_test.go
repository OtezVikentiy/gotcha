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

func TestRateLimitKey(t *testing.T) {
	h := &Handler{}
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "203.0.113.7:1234"
	if got := h.rateLimitKey(r, "  User@Example.COM "); got != "203.0.113.7|user@example.com" {
		t.Errorf("rateLimitKey = %q, want %q", got, "203.0.113.7|user@example.com")
	}
}

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

func TestRateLimitKeyNormalEmailUnchanged(t *testing.T) {
	h := &Handler{}
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "198.51.100.3:9"
	if got, want := h.rateLimitKey(r, "  Alice@Example.COM  "), "198.51.100.3|alice@example.com"; got != want {
		t.Errorf("rateLimitKey = %q, want %q", got, want)
	}
}

// Auth — реальный, на тестовой PG, а не nil: без него код падает nil-паникой раньше своего
// же ассерта, маскируя настоящую причину теста.
func authTestHandler(t *testing.T) *Handler {
	t.Helper()
	pool := testenv.MigratedPG(t)
	return &Handler{
		BaseURL:                   "http://gotcha.example",
		Auth:                      auth.NewService(pool),
		loginLimiter:              newRateLimiter(time.Now, 5, time.Minute, loginLimiterMaxKeys, "loginLimiter"),
		ipLimiter:                 newRateLimiter(time.Now, 20, time.Minute, ipLimiterMaxKeys, "ipLimiter"),
		emailLimiter:              newRateLimiter(time.Now, 50, 15*time.Minute, emailLimiterMaxKeys, "emailLimiter"),
		passwordResetIPLimiter:    newRateLimiter(time.Now, 20, time.Minute, passwordResetMaxKeys, "passwordResetIPLimiter"),
		passwordResetEmailLimiter: newRateLimiter(time.Now, 5, 15*time.Minute, passwordResetMaxKeys, "passwordResetEmailLimiter"),
	}
}

// h.parseForm распознаёт http.MaxBytesError и отвечает 413, а не общим 400 — превышение
// предела тела отличимо от произвольно битой формы.
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

func TestEmailLimiterKeyCappedInLoginSubmit(t *testing.T) {
	h := authTestHandler(t)
	// limit=0 форсирует 429 сразу после проверки лимитов, не давая дойти до Authenticate —
	// здесь важен только размер карты, в который лёг ключ.
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

func TestLimiterEmailKeyPartNormalUnchanged(t *testing.T) {
	if got, want := limiterEmailKeyPart("  Alice@Example.COM  "), "alice@example.com"; got != want {
		t.Errorf("limiterEmailKeyPart = %q, want %q", got, want)
	}
}

// Атака на произвольные ключи не должна класть логин/статус-страницы для остальных: на
// потолке новый ключ вытесняет самый старый, а не получает отказ (K11).
func TestRateLimiterCapEvictsOldestForNewKeys(t *testing.T) {
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

	for i := capacity; i < capacity+50; i++ {
		key := fmt.Sprintf("203.0.113.%d:%d", i, i)
		if !rl.Allow(key) {
			t.Errorf("ключ %d сверх потолка: want true — переполнение обязано вытеснять старейший ключ, а не отказывать новому", i)
		}
	}
	if got := rl.size(); got != capacity {
		t.Errorf("size() после потока лишних ключей = %d, want %d (потолок не пробивается вверх, но и не отказывает)", got, capacity)
	}
}

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

	for i := 0; i < limit-1; i++ {
		if !rl.Allow("k2") {
			t.Fatalf("k2, дополнительная попытка %d из %d: want true (существующий ключ под потолком продолжает работать своим счётчиком)", i+1, limit-1)
		}
	}
	if rl.Allow("k2") {
		t.Errorf("k2 после исчерпания limit=%d: want false (обычный лимит, не потолок карты)", limit)
	}

	// k1 вставлен первым из троих — он старейший по очереди вставки и обязан уступить место k4.
	if !rl.Allow("k4") {
		t.Errorf("k4 (невиданный ключ на заполненной карте): want true — переполнение вытесняет k1, не отказывает")
	}
	if got := rl.size(); got != capacity {
		t.Errorf("size() = %d, want %d — вытеснение k1 держит размер на потолке", got, capacity)
	}
	if !rl.Allow("k1") {
		t.Errorf("k1 после вытеснения: want true — вытесненный ключ начинает с чистого счётчика")
	}
	if got := rl.size(); got != capacity {
		t.Errorf("size() после возврата k1 = %d, want %d — k1 вернулся вытеснив k2 или k3, размер не растёт", got, capacity)
	}
}

// Вытеснить КОНКРЕТНЫЙ ключ нельзя одним лишним запросом: очередь вставки отдаёт под
// вытеснение переднего (самого старого), а не атакуемого — иначе сброс счётчика жертвы стоил
// бы одного запроса и обход лимита был бы бесплатным.
func TestRateLimiterEvictionIsFIFONotTargeted(t *testing.T) {
	now := time.Now()
	const capacity = 10
	rl := newRateLimiter(func() time.Time { return now }, 100, time.Minute, capacity, "test")

	for i := 0; i < capacity-1; i++ {
		if !rl.Allow(fmt.Sprintf("filler%d", i)) {
			t.Fatalf("filler%d: want true", i)
		}
	}
	if !rl.Allow("target") {
		t.Fatalf("target: want true")
	}
	if got := rl.size(); got != capacity {
		t.Fatalf("тестовая заготовка: size() = %d, want %d", got, capacity)
	}

	if !rl.Allow("attacker0") {
		t.Fatalf("attacker0: want true")
	}
	hitsBefore := len(rl.hits["target"])
	if hitsBefore == 0 {
		t.Fatalf("тестовая заготовка: у target нет истории после одного лишнего ключа")
	}
	if !rl.Allow("target") {
		t.Fatalf("target ещё раз: want true")
	}
	if got := len(rl.hits["target"]); got != hitsBefore+1 {
		t.Errorf("счётчик target = %d после одного лишнего ключа, want %d — вытеснен был filler0, не target (сброс счётчика жертвы одним запросом недопустим)", got, hitsBefore+1)
	}

	// Проталкиваем через очередь оставшийся потолок новых ключей — только теперь target
	// доходит до фронта и вытесняется. Цена такая же, как у самого переполнения карты.
	for i := 1; i < capacity; i++ {
		rl.Allow(fmt.Sprintf("attacker%d", i))
	}
	if got := len(rl.hits["target"]); got != 0 {
		t.Errorf("после протолкнутого потолка новых ключей target обязан быть вытеснен, got len=%d", got)
	}
}

// sweepExpired обязана чистить order/elems вместе с hits — иначе очередь вставки переживает
// hits, а evictOldestLocked начинает гонять мёртвые записи вместо освобождения ёмкости.
func TestRateLimiterSweepKeepsOrderConsistentWithHits(t *testing.T) {
	now := time.Now()
	clock := &now
	const capacity = 5
	rl := newRateLimiter(func() time.Time { return *clock }, 5, time.Second, capacity, "test")

	for i := 0; i < capacity; i++ {
		if !rl.Allow(fmt.Sprintf("stale%d", i)) {
			t.Fatalf("заполнение потолка, ключ %d: want true", i)
		}
	}

	*clock = clock.Add(2 * time.Second) // все hits старше window — sweepExpired обязана их убрать целиком

	if !rl.Allow("fresh") {
		t.Fatalf("fresh (принудительный свип на потолке): want true")
	}

	if got := rl.size(); got != 1 {
		t.Fatalf("size() = %d, want 1 — старые ключи должны были уйти свипом целиком", got)
	}
	if got := rl.order.Len(); got != rl.size() {
		t.Errorf("order.Len() = %d, want %d — очередь вставки обязана усохнуть вместе с hits", got, rl.size())
	}
	if got := len(rl.elems); got != rl.size() {
		t.Errorf("len(elems) = %d, want %d — elems обязана усохнуть вместе с hits", got, rl.size())
	}
}

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

	for i := 0; i < 500; i++ {
		rl.Allow(fmt.Sprintf("attacker%d", i))
	}

	if got := rl.sweepCalls - before; got != 1 {
		t.Errorf("sweepCalls вырос на %d за 500 запросов поверх потолка при неподвижном времени, want 1 (принудительная уборка обязана троттлиться так же, как фоновая — не чаще раза в rl.window)", got)
	}
}

// maxKeys здесь заведомо больше вставляемых ключей — тест изолирует троттлинг фоновой уборки
// от принудительной уборки на потолке (см. TestRateLimiterCapEvictsOldestForNewKeys).
func TestRateLimiterSweepThrottledByInterval(t *testing.T) {
	now := time.Now()
	rl := newRateLimiter(func() time.Time { return now }, 5, time.Minute, 50000, "test")

	// sweepThreshold+1 записей ещё не запускает уборку ("> sweepThreshold" смотрит на размер
	// ДО вставки) — sweepThreshold+2-й вызов первым застаёт карту уже за порогом.
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

// Атака, забивающая карту ipLimiter выдуманными адресами, не должна класть логин для
// остальных: новый IP обязан дойти до Authenticate (и получить обычный отказ по паролю),
// а не 429 только из-за переполнения карты чужими ключами.
func TestLoginSubmitSurvivesIPLimiterMapOverflow(t *testing.T) {
	h := authTestHandler(t)
	const capacity = 5
	h.ipLimiter = newRateLimiter(time.Now, 20, time.Minute, capacity, "ipLimiter")

	post := func(remoteAddr string) int {
		body := "email=nobody@x.com&password=wrong"
		r := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Origin", h.BaseURL)
		r.RemoteAddr = remoteAddr
		rec := httptest.NewRecorder()
		h.loginSubmit(rec, r)
		return rec.Code
	}

	for i := 0; i < capacity+30; i++ {
		if code := post(fmt.Sprintf("203.0.113.%d:1", i%250)); code == http.StatusTooManyRequests {
			t.Fatalf("запрос %d при заполнении карты: status = 429 — переполнение чужими IP не должно класть логин", i)
		}
	}
	if got := h.ipLimiter.size(); got != capacity {
		t.Fatalf("тестовая заготовка: ipLimiter.size() = %d, want %d (карта на потолке)", got, capacity)
	}

	if code := post("198.51.100.9:1"); code != http.StatusUnprocessableEntity {
		t.Errorf("новый IP на заполненной карте ipLimiter: status = %d, want %d (неверный пароль, а не 429 из-за переполнения)", code, http.StatusUnprocessableEntity)
	}
}

// publicRateLimited закрывает публичную статус-страницу — та же переполненная чужими IP
// карта не должна прятать её от новых анонимных посетителей ровно во время аварии.
func TestPublicRateLimitedSurvivesMapOverflow(t *testing.T) {
	const capacity = 5
	h := &Handler{publicLimiter: newRateLimiter(time.Now, 12, time.Minute, capacity, "publicLimiter")}
	guarded := h.publicRateLimited(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	get := func(remoteAddr string) int {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/status/x", nil)
		req.RemoteAddr = remoteAddr
		guarded(rec, req)
		return rec.Code
	}

	for i := 0; i < capacity+30; i++ {
		if code := get(fmt.Sprintf("203.0.113.%d:1", i%250)); code == http.StatusTooManyRequests {
			t.Fatalf("запрос %d при заполнении карты: status = 429 — переполнение чужими IP не должно класть статус-страницу", i)
		}
	}
	if got := h.publicLimiter.size(); got != capacity {
		t.Fatalf("тестовая заготовка: publicLimiter.size() = %d, want %d (карта на потолке)", got, capacity)
	}

	if code := get("198.51.100.9:1"); code != http.StatusOK {
		t.Errorf("новый анонимный посетитель на заполненной карте: status = %d, want 200 — статус-страница обязана открыться, а не 429", code)
	}
}

// ipLimiter проверяется раньше per-account loginLimiter: иначе один IP потоком выдуманных
// email раздувает карту loginLimiter быстрее, чем успевает сработать ipLimiter.
func TestLoginSubmitIPLimiterBoundsLoginLimiterGrowth(t *testing.T) {
	h := authTestHandler(t) // ipLimiter: limit 20, window 1 минута

	post := func(email string) int {
		body := "email=" + email + "@x.com&password=x"
		r := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Origin", h.BaseURL)
		r.RemoteAddr = "203.0.113.7:1"
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

// loginLimiter — общий инстанс на /login, /register и /sso: тот же порядок (ipLimiter первым)
// обязателен и здесь, иначе один IP раздувает ту же карту в обход через /sso.
func TestSSOSubmitIPLimiterBoundsLoginLimiterGrowth(t *testing.T) {
	h, _, _, _ := ssoTestHandler(t) // ipLimiter: limit 20, window 1 минута (New())

	post := func(email string) int {
		body := "email=" + email + "@x.com"
		r := httptest.NewRequest(http.MethodPost, "/sso", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Origin", h.BaseURL)
		r.RemoteAddr = "203.0.113.7:1"
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
