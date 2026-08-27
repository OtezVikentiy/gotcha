package web

import (
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// sweepThreshold — при превышении числа ключей карта становится кандидатом
// на фоновую, лениво-периодическую уборку (см. Allow); сама уборка при этом
// ограничена по частоте не чаще раза в rl.window, см. lastSweep. Не путать
// с потолком rl.maxKeys ниже: это разные механизмы — sweepThreshold лишь
// экономит полные обходы карты, пока она не подошла к потолку, а сам
// потолок обеспечивает жёсткую границу памяти независимо от того, успела
// сработать фоновая уборка или нет.
const sweepThreshold = 10000

// rateLimiter — фиксированное окно: не более limit попыток за window на
// ключ. Часы инжектируются, чтобы юнит-тесты могли перематывать время без
// реальных sleep.
type rateLimiter struct {
	mu     sync.Mutex
	limit  int
	window time.Duration
	now    func() time.Time
	hits   map[string][]time.Time

	// maxKeys — жёсткий потолок числа ключей в карте, свой для каждого
	// инстанса (находка W2-B: один потолок на все лимитеры пакета неверен —
	// у per-IP публичного лимитера и per-account лимитера логина совершенно
	// разная ожидаемая кардинальность). Значение и обоснование — на месте
	// создания в web.go/New(). Задаётся конструктором, см. newRateLimiter.
	maxKeys int

	// name — имя лимитера для лога отказа при переполнении (см. Allow).
	name string

	// lastSweep — момент (по rl.now()) последнего полного обхода карты, будь
	// то фоновый (sweepThreshold) или принудительный на упоре в maxKeys; не
	// даёт ЛЮБОМУ из них гоняться чаще раза в window, см. Allow. Общий
	// троттлинг обязателен для обоих путей: без него принудительная уборка
	// на упоре в потолок гоняла бы полный обход на КАЖДЫЙ запрос поверх
	// потолка — та же O(n) под мьютексом, что и у нетроттленного фонового
	// пути (находка ревью W2-B).
	lastSweep time.Time

	// sweepCalls — счётчик вызовов sweepExpired. Инструментарий только для
	// тестов (аналогично size() ниже) — проверяет, что уборка не запускается
	// на каждом вызове Allow за порогом.
	sweepCalls int
}

// newRateLimiter создаёт лимитер на limit попыток за window на ключ, с
// жёстким потолком maxKeys числа ключей в карте (см. поле maxKeys) и
// именем name для лога отказа при переполнении (см. Allow).
func newRateLimiter(now func() time.Time, limit int, window time.Duration, maxKeys int, name string) *rateLimiter {
	return &rateLimiter{
		limit:   limit,
		window:  window,
		now:     now,
		maxKeys: maxKeys,
		name:    name,
		hits:    make(map[string][]time.Time),
	}
}

// Allow регистрирует попытку под ключом key и сообщает, уложилась ли она в
// лимит. Вызовы сверх лимита тоже не забываются (иначе окно никогда не
// сдвинется), но не увеличивают счётчик сверх того, что уже отброшено по
// давности.
func (rl *rateLimiter) Allow(key string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := rl.now()

	// Число ключей в карте ограничено ДВОЙНО:
	//  1. Граница памяти гарантирована самим rateLimiter независимо от
	//     вызывающего кода: rl.maxKeys — жёсткий потолок на инстанс (см. поле
	//     и newRateLimiter). За потолком НЕВИДЕННЫЙ ранее ключ получает
	//     отказ, не заводя записи в карте, — см. блок ниже. Уже существующие
	//     ключи под потолком продолжают работать как раньше, своим счётчиком.
	//  2. Фоновая лениво-периодическая уборка (sweepExpired) удаляет ключи,
	//     чьё окно истекло целиком, и ограничена по частоте — не чаще раза в
	//     rl.window (см. lastSweep) — чтобы полный обход карты не оплачивал
	//     КАЖДЫЙ запрос, попавший за sweepThreshold.
	// На совести вызывающего кода остаётся длина ОТДЕЛЬНОГО ключа: для
	// email-based ключей (per-account и per-email лимитеры логина/
	// регистрации) она ограничена limiterEmailKeyPart и константами
	// maxEmailKeyBytes/oversizedEmailBucket ниже — без этого один ключ мог
	// бы весить мегабайты. Потолок rl.maxKeys от этого не защищает, он
	// только не даёт числу ключей расти неограниченно.
	if len(rl.hits) > sweepThreshold && now.Sub(rl.lastSweep) >= rl.window {
		rl.sweepExpired(now)
		rl.lastSweep = now
	}

	if _, exists := rl.hits[key]; !exists && len(rl.hits) >= rl.maxKeys {
		// Упёрлись в потолок на НЕВИДЕННОМ ранее ключе. Уборка здесь — ТОТ
		// ЖЕ троттлинг lastSweep/rl.window, что и у фонового sweepThreshold
		// выше (находка ревью W2-B): без него принудительная уборка на
		// каждом запросе поверх потолка возвращает ровно ту O(n) под
		// мьютексом, ради снятия которой троттлинг вводился, — атакующий,
		// удерживающий карту на потолке, получает усиление: каждый его
		// дешёвый запрос стоит сервера полного обхода maxKeys элементов, а
		// мьютекс на это время закрыт для чужих легитимных запросов на
		// ЭТОМ ЖЕ лимитере (loginLimiter общий на /login, /register, /sso).
		// Если с последней уборки прошло меньше окна — отказываем СЛЕПО, не
		// обходя карту: ключ всё равно не заводится, так что слепота здесь
		// ничего не портит, а число полных обходов в единицу времени
		// остаётся ограничено независимо от частоты запросов.
		if now.Sub(rl.lastSweep) >= rl.window {
			rl.sweepExpired(now)
			rl.lastSweep = now
		}
		if _, exists := rl.hits[key]; !exists && len(rl.hits) >= rl.maxKeys {
			// Уборка не помогла: в окне действительно живёт rl.maxKeys
			// различных ключей. Эти лимитеры защищают аутентификацию и
			// публичные роуты — молча снять защиту (пропустить всех)
			// хуже, чем отказать паре (ключу), которую не видели раньше:
			// состояние само рассосётся со следующим окном. Отказ обязан
			// быть громким — иначе оператор узнаёт об атаке постфактум.
			slog.Warn("rate limiter at capacity, denying unseen key",
				"limiter", rl.name, "keys", len(rl.hits), "max_keys", rl.maxKeys)
			return false
		}
	}

	cutoff := now.Add(-rl.window)
	fresh := rl.hits[key][:0]
	for _, t := range rl.hits[key] {
		if t.After(cutoff) {
			fresh = append(fresh, t)
		}
	}
	if len(fresh) >= rl.limit {
		rl.hits[key] = fresh
		return false
	}
	rl.hits[key] = append(fresh, now)
	return true
}

// sweepExpired removes all entries whose time windows have fully expired.
// Called with lock held, throttled to at most once per rl.window (see Allow).
func (rl *rateLimiter) sweepExpired(now time.Time) {
	rl.sweepCalls++
	cutoff := now.Add(-rl.window)
	for key, times := range rl.hits {
		// Keep only entries within the window
		fresh := times[:0]
		for _, t := range times {
			if t.After(cutoff) {
				fresh = append(fresh, t)
			}
		}
		if len(fresh) == 0 {
			// Entire window expired, delete the key
			delete(rl.hits, key)
		} else {
			rl.hits[key] = fresh
		}
	}
}

// size returns the current number of keys in the rate limiter map.
// Exported for testing purposes only.
func (rl *rateLimiter) size() int {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	return len(rl.hits)
}

// publicRateLimited навешивает per-IP лимит на НЕаутентифицированный роут
// (см. Handler.publicLimiter). Ответ — голый 429 без тела: адресаты этих роутов
// машинные (пробы, heartbeat-крон) либо публичные, страница ошибки им не нужна.
func (h *Handler) publicRateLimited(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.publicLimiter != nil && !h.publicLimiter.Allow(h.clientIP(r)) {
			w.Header().Set("Retry-After", "60")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		next(w, r)
	}
}

// SetAgentDistRateLimit переустанавливает порог per-IP лимитера раздачи
// бинарей агента (GOTCHA_AGENT_DIST_RATE_PER_MIN, ops-H4, cmd/gotcha/main.go).
// Отдельный метод, а не публичное поле лимитера: *rateLimiter — внутренний
// тип пакета, конструктору нужен источник времени. Вызывается один раз при
// старте, до начала обслуживания запросов — гонок с Allow() нет.
//
// perMinute <= 0 обнуляет h.agentLimiter (nil), а не создаёт лимитер с
// limit=0: внутри rateLimiter.Allow условие `len(fresh) >= rl.limit` при
// limit=0 истинно всегда, то есть лимитер с нулевым порогом резал бы 429
// АБСОЛЮТНО все запросы. В продукте соглашение обратное — 0 у числовых
// GOTCHA_*-переменных значит «без границы» (как *_RETENTION_DAYS), и
// agentDistRateLimited уже трактует nil как «лимит снят» — переиспользуем
// этот путь, чтобы оператор, вписавший 0, получил ожидаемое поведение, а не
// тихо запертую раздачу агента по всему парку.
func (h *Handler) SetAgentDistRateLimit(perMinute int) {
	if perMinute <= 0 {
		h.agentLimiter = nil
		return
	}
	h.agentLimiter = newRateLimiter(time.Now, perMinute, time.Minute, agentLimiterMaxKeys, "agentLimiter")
}

// agentDistRateLimited навешивает узкий per-IP лимит на раздачу бинарей
// агента (GET /agent/{file}, см. Handler.agentLimiter) — отдельно от общего
// publicLimiter, чтобы установки агента не могли использовать вес бинаря
// (~9.3 МиБ) для DoS общего пула соединений (см. комментарий поля).
func (h *Handler) agentDistRateLimited(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.agentLimiter != nil && !h.agentLimiter.Allow(h.clientIP(r)) {
			w.Header().Set("Retry-After", "60")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		next(w, r)
	}
}

// remoteHost — host из RemoteAddr (порт отброшен), т.е. адрес непосредственного
// TCP-пира. За reverse-proxy это адрес прокси, а не клиента (см. clientIP).
func remoteHost(r *http.Request) string {
	host := r.RemoteAddr
	if h, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		host = h
	}
	return host
}

// clientIP — реальный IP клиента, ключ для глобального per-IP лимита (SEC-L2).
//
// По умолчанию (TrustedProxies пуст) — это адрес непосредственного пира
// (RemoteAddr). За TLS-терминирующим reverse-proxy (штатная HTTPS-топология —
// nginx/traefik) RemoteAddr у ВСЕХ клиентов схлопывается в один IP прокси: тогда
// глобальный per-IP лимитер и обесценивается (все — один ключ), и превращается в
// self-DoS (один актор выбирает общий бакет и лочит логин всем). Поэтому, только
// когда непосредственный пир входит в доверенный список TrustedProxies
// (GOTCHA_TRUSTED_PROXIES), доверяем X-Forwarded-For и берём из него настоящего
// клиента. Иначе XFF — данные, подконтрольные клиенту, и их игнорируем (иначе
// тривиальный обход лимита подделкой заголовка).
func (h *Handler) clientIP(r *http.Request) string {
	host := remoteHost(r)
	if len(h.TrustedProxies) == 0 {
		return host
	}
	peer := net.ParseIP(host)
	if peer == nil || !ipInNets(peer, h.TrustedProxies) {
		return host // пир не доверенный прокси — XFF не доверяем
	}
	// Пир — доверенный прокси: идём по X-Forwarded-For справа налево и берём
	// первый адрес НЕ из доверенного набора — это клиент, ближайший к первому
	// нашему прокси (правые хопы дописаны нашими же прокси).
	parts := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
	for i := len(parts) - 1; i >= 0; i-- {
		p := strings.TrimSpace(parts[i])
		ip := net.ParseIP(p)
		if ip == nil {
			continue
		}
		if !ipInNets(ip, h.TrustedProxies) {
			return p
		}
	}
	return host // все хопы доверенные или XFF пуст — остаёмся на пире
}

// ipInNets — принадлежит ли ip хоть одной из сетей.
func ipInNets(ip net.IP, nets []*net.IPNet) bool {
	for _, n := range nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// maxEmailKeyBytes — верхняя граница длины email по RFC 5321 (максимум пути
// в конверте — 254 байта, включая локальную часть, @ и домен). Всё длиннее
// в принципе не может быть валидным адресом и логин по нему не пройдёт.
const maxEmailKeyBytes = 254

// oversizedEmailBucket — общий ключ-«ведро» для полей email длиннее
// maxEmailKeyBytes, см. limiterEmailKeyPart.
const oversizedEmailBucket = "oversized"

// limiterEmailKeyPart — email-часть ключа рейт-лимитера: нижний регистр,
// обрезанные пробелы, длина ограничена maxEmailKeyBytes. Поле email
// приходит из формы логина/регистрации без ограничения размера, а
// вызывающая сторона кладёт результат в rl.hits ДО того, как успевает
// отработать per-IP лимитер, и не освобождает ключ при отказе
// (rl.hits[key] = fresh сохраняет саму строку) — поэтому длина должна быть
// ограничена здесь, в ЕДИНСТВЕННОМ месте, где строится email-часть ключа
// (используется и per-account rateLimitKey ниже, и напрямую для
// h.emailLimiter в auth.go), а не по счастливой случайности одного из двух
// путей. Любой email длиннее maxEmailKeyBytes схлопывается в общий ключ
// (oversizedEmailBucket), а не получает по ключу на попытку.
//
// Не путать с normalizeEmail (auth.go): та нормализация используется и там,
// где кап по длине недопустим — сравнение/поиск аккаунта, — обрезка иначе
// схлопнула бы разные адреса в один и тот же аккаунт.
func limiterEmailKeyPart(email string) string {
	email = strings.ToLower(strings.TrimSpace(email))
	if len(email) > maxEmailKeyBytes {
		return oversizedEmailBucket
	}
	return email
}

// rateLimitKey строит ключ ip|email для per-account лимитера логина/
// регистрации, см. limiterEmailKeyPart про кап длины email-части.
func (h *Handler) rateLimitKey(r *http.Request, email string) string {
	return h.clientIP(r) + "|" + limiterEmailKeyPart(email)
}
