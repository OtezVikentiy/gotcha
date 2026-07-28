package ingest

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"regexp"
	"strings"
)

const scrubMask = "[scrubbed]"

// sepReplacer убирает разделители из имени ключа перед сравнением с denylist:
// заголовок X-Api-Key и ключ api_key должны совпасть, хотя дефис ≠ подчёркивание.
var sepReplacer = strings.NewReplacer("-", "", "_", "", " ", "", ".", "")

func normKey(s string) string { return sepReplacer.Replace(strings.ToLower(s)) }

// emailTextMask — маска для email, найденного в СВОБОДНОМ тексте (RA-L10).
// Отдельна от scrubMask: тут редактируется подстрока значения, а не всё поле.
const emailTextMask = "[email]"

// emailTextRe — консервативный шаблон email для свободного текста (RA-L10).
// Умышленно узкий: local@domain.tld. Ничего кроме email не трогаем — номера
// карт/телефоны дают высокий процент ложных срабатываний на SQL/URL и вне скоупа.
var emailTextRe = regexp.MustCompile(`[\w.+-]+@[\w-]+\.[\w.-]+`)

type Scrubber struct {
	ScrubIP    bool
	ScrubEmail bool
	// ScrubFreeText включает опциональное маскирование email в свободном тексте
	// (message/exception value/span.description) — RA-L10. По умолчанию false:
	// текущее поведение (denylist по ключам) не меняется. Включается из main.go
	// установкой поля после NewScrubber (GOTCHA_SCRUB_FREETEXT).
	ScrubFreeText bool
	deny          map[string]bool
	denyNorm      map[string]bool // те же ключи без разделителей (см. sepReplacer)
}

func NewScrubber(scrubIP, scrubEmail bool, denyKeys []string) *Scrubber {
	deny := make(map[string]bool, len(denyKeys))
	denyNorm := make(map[string]bool, len(denyKeys))
	for _, k := range denyKeys {
		if k = strings.ToLower(strings.TrimSpace(k)); k != "" {
			deny[k] = true
			// normKey может дать "" на ключе из одних разделителей ("-", "_"):
			// такой пустой ключ иначе через strings.Contains(kn, "") маскировал бы
			// АБСОЛЮТНО все поля. Пропускаем.
			if kn := normKey(k); kn != "" {
				denyNorm[kn] = true
			}
		}
	}
	return &Scrubber{ScrubIP: scrubIP, ScrubEmail: scrubEmail, deny: deny, denyNorm: denyNorm}
}

// emailAttrKeys — ключи атрибутов, несущие email конечного пользователя. При
// ScrubEmail=true маскируются в тегах/данных/атрибутах так же, как denylist-ключи:
// иначе user.email оседал бы в transactions.tags, metric_points.attributes и
// span.data, хотя колонка events.user_email уже занулена (неполнота скрубинга).
// user_id/enduser.id намеренно НЕ трогаем — это идентификатор (не сам email), и
// по нему работает субъектное удаление/экспорт (152-ФЗ право на доступ/удаление).
var emailAttrKeys = map[string]bool{
	"user.email": true, "enduser.email": true, "email": true, "sentry.user.email": true,
}

// ipAttrKeys — ключи атрибутов, несущие IP конечного пользователя. При
// ScrubIP=true маскируются в тегах/данных/атрибутах так же, как email-ключи:
// иначе IP оседал бы в transactions.tags, metric_points.attributes и span.data,
// хотя колонка events.user_ip уже занулена (симметрично неполноте email-скрубинга).
var ipAttrKeys = map[string]bool{
	"client.address": true, "net.peer.ip": true, "net.sock.peer.addr": true,
	"user.ip": true, "sentry.user.ip_address": true, "http.client_ip": true,
}

// ipHeaderKeysNorm — нормализованные (без разделителей) имена forwarding-заголовков
// и env-переменных с IP клиента, ловятся ПОДСТРОЧНО по normKey. Так совпадают все
// формы, в которых их шлют SDK: X-Forwarded-For, x_forwarded_for, CGI/WSGI-форма
// HTTP_X_FORWARDED_FOR, а также X-Real-IP, X-Client-IP, True-Client-IP,
// X-Cluster-Client-IP, CF-Connecting-IP, Client-IP, REMOTE_ADDR (SEC-P2-4).
// Подстроки подобраны так, чтобы НЕ задевать X-Forwarded-Proto/Host/Port.
var ipHeaderKeysNorm = []string{"forwardedfor", "realip", "clientip", "remoteaddr", "connectingip"}

// denied — совпадает ли ИМЯ КЛЮЧА (объекта/тега/атрибута) с denylist. Матч
// ПОДСТРОЧНЫЙ: X-Api-Key ловится словом api_key, user_password — словом password.
func (s *Scrubber) denied(key string) bool { return s.deniedKey(key, true) }

// deniedParam — совпадает ли ИМЯ query-параметра с denylist. Матч ПО СЛОВАМ
// (имя режется на слова по разделителям и границам camelCase, каждое слово точно
// сравнивается с denylist), а не подстрочный. Ловит составные секреты
// client_secret→secret, id_token→token, new_password→password, но НЕ подстроки:
// author≠auth, tokenizer≠token, cookies_accepted (cookies≠cookie). Компромисс
// между over-scrub подстрочного denied() (SEC-P2-2) и утечкой чисто-точного
// равенства (SEC-P1-1). session_expired даёт слово session (в denylist) →
// маскируется: безвредный fail-closed на несекретном поле.
func (s *Scrubber) deniedParam(name string) bool { return s.deniedKey(name, false) }

// deniedKey — общее ядро. substr=true — подстрочный матч denylist (для имён
// ключей объектов: X-Api-Key ловится словом api_key). substr=false — точное
// равенство ПЛЮС матч по словам (для имён query-параметров). email/IP-атрибуты и
// точное совпадение denylist проверяются в обоих режимах.
func (s *Scrubber) deniedKey(key string, substr bool) bool {
	k := strings.ToLower(key)
	if s.ScrubEmail && emailAttrKeys[k] {
		return true
	}
	kn := normKey(k)
	if s.ScrubIP {
		if ipAttrKeys[k] {
			return true
		}
		for _, ipk := range ipHeaderKeysNorm {
			if strings.Contains(kn, ipk) {
				return true
			}
		}
	}
	if s.deny[k] || s.denyNorm[kn] {
		return true
	}
	if substr {
		for d := range s.deny {
			if strings.Contains(k, d) {
				return true
			}
		}
		// Нормализованное сравнение: X-Api-Key/Api-Key ловятся ключом api_key,
		// хотя дефис не совпадает с подчёркиванием при подстрочном матче выше.
		for d := range s.denyNorm {
			if strings.Contains(kn, d) {
				return true
			}
		}
		return false
	}
	// substr=false: матч по СЛОВАМ (см. deniedParam). splitWords по ОРИГИНАЛУ key,
	// чтобы поймать границы camelCase до ToLower (idToken → id, Token).
	for _, w := range splitWords(key) {
		if s.denyNorm[strings.ToLower(w)] {
			return true
		}
	}
	return false
}

// splitWords режет имя на слова по не-буквенно-цифровым разделителям И границам
// camelCase (fooBar → foo, Bar; client_secret → client, secret). Регистр слов
// сохранён (вызывающий нормализует сам).
func splitWords(s string) []string {
	var words []string
	start := 0
	flush := func(end int) {
		if end > start {
			words = append(words, s[start:end])
		}
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		alnum := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
		if !alnum {
			flush(i)
			start = i + 1
			continue
		}
		if i > start && c >= 'A' && c <= 'Z' {
			if p := s[i-1]; p >= 'a' && p <= 'z' || p >= '0' && p <= '9' {
				flush(i)
				start = i
			}
		}
	}
	flush(len(s))
	return words
}

func (s *Scrubber) ScrubUser(ip, email *string) {
	if s == nil {
		return
	}
	if s.ScrubIP && ip != nil {
		*ip = ""
	}
	if s.ScrubEmail && email != nil {
		*email = ""
	}
}

// ScrubText маскирует email-адреса в свободном тексте на [email], но только при
// включённом ScrubFreeText (RA-L10). nil-safe; при выключенном флаге и на пустой
// строке возвращает вход как есть. Кроме email ничего не трогает.
func (s *Scrubber) ScrubText(text string) string {
	if s == nil || !s.ScrubFreeText || text == "" {
		return text
	}
	return emailTextRe.ReplaceAllString(text, emailTextMask)
}

// ScrubMessage чистит человеко-читаемое поле (имя транзакции, message, exception
// value, описание спана): free-text email через ScrubText (по флагу) ПЛЮС query-
// токены в URL, встроенных в текст вида "GET https://api/x?token=…". В отличие от
// ScrubText URL-часть чистится ВСЕГДА, а не только при ScrubFreeText: query-токен
// в имени/описании — утечка и при дефолтном скрабинге (SEC-M2).
func (s *Scrubber) ScrubMessage(text string) string {
	if s == nil || text == "" {
		return text
	}
	// URL-скраб ПЕРВЫМ: он снимает basic-auth (user:pass@host) внутри URL, иначе
	// свободнотекстовый email-матч ошибочно принял бы "pass@host.tld" за email и
	// замаскировал бы домен. Free-text ScrubText — вторым, по не-URL остатку.
	return s.ScrubText(s.scrubEmbeddedURLs(text))
}

// scrubEmbeddedURLs находит URL-подстроки (scheme://… до пробельного разделителя)
// внутри свободного текста и прогоняет каждую через scrubURLParams. Разделители
// (пробелы, переводы строк, отступы) сохраняются БАЙТ-В-БАЙТ: замена идёт по
// индексам через strings.Builder, а не через Fields/Join, который схлопывал бы
// многострочные сообщения (SEC-P2-1). Обрамляющая пунктуация ('"`<>[]() и
// хвостовые .,;:) не мешает распознаванию и не съедается (SEC-P1-3). Текст без
// "://" возвращается как есть.
func (s *Scrubber) scrubEmbeddedURLs(text string) string {
	if !strings.Contains(text, "://") {
		return text
	}
	var b strings.Builder
	changed := false
	lastFlush := 0
	i := 0
	for i < len(text) {
		if isSpaceByte(text[i]) {
			i++
			continue
		}
		j := i
		for j < len(text) && !isSpaceByte(text[j]) {
			j++
		}
		tok := text[i:j]
		if scrubbed := s.scrubURLToken(tok); scrubbed != tok {
			if !changed { // ленивый старт: не аллоцируем, пока чистить нечего (SEC-P2-5)
				b.Grow(len(text))
				changed = true
			}
			b.WriteString(text[lastFlush:i]) // разделители + чистые токены до этого
			b.WriteString(scrubbed)
			lastFlush = j
		}
		i = j
	}
	if !changed {
		return text // ни один токен не изменён — вход возвращается байт-в-байт, 0 аллокаций
	}
	b.WriteString(text[lastFlush:])
	return b.String()
}

// scrubURLToken маскирует query-токены КАЖДОГО URL-ядра внутри непробельного
// токена, сохраняя обрамление. URL распознаётся по "://" с откатом к началу схемы
// (ловятся `url=https://…`, `"https://…"`, `(https://…)`), а его КОНЕЦ — по набору
// URL-символов RFC 3986, а не по пробелу: иначе `payload={"url":"https://a?token=X",
// "next":"KEEP"}` затянул бы `,"next":"KEEP"` в query и scrubParams стёр бы их
// (SEC-P1-2). Запятая — граница, поэтому `url1,url2` обрабатываются оба (SEC-P2-4).
func (s *Scrubber) scrubURLToken(tok string) string {
	if !strings.Contains(tok, "://") {
		return tok
	}
	var b strings.Builder
	i := 0
	for i < len(tok) {
		rel := strings.Index(tok[i:], "://")
		if rel < 0 {
			b.WriteString(tok[i:])
			break
		}
		p := i + rel
		start := p
		for start > i && isSchemeByte(tok[start-1]) {
			start--
		}
		if start == p { // перед "://" нет символов схемы — пропускаем
			b.WriteString(tok[i : p+3])
			i = p + 3
			continue
		}
		end := p + 3
		for end < len(tok) && isURLByte(tok[end]) {
			end++
		}
		// Хвостовая пунктуация не входит в URL. Но закрывающую скобку )]} оставляем,
		// если внутри URL есть парная открывающая — это часть значения (маска SDK
		// token=[Filtered], IPv6 [::1]), а не обрамление предложения: иначе ']'
		// приклеился бы к маске как [scrubbed]] и рос при ре-обработке (SEC-P2-3).
		trimEnd := end
		for trimEnd > p+3 && isURLTrailByte(tok[trimEnd-1]) {
			c := tok[trimEnd-1]
			if op := matchingOpener(c); op != 0 && strings.IndexByte(tok[start:trimEnd-1], op) >= 0 {
				break
			}
			trimEnd--
		}
		b.WriteString(tok[i:start])
		b.WriteString(s.scrubURLParams(tok[start:trimEnd]))
		i = trimEnd
	}
	return b.String()
}

func isSpaceByte(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\v' || c == '\f'
}

func isSchemeByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
		c == '+' || c == '.' || c == '-'
}

// isURLByte — символы, из которых состоит URL (RFC 3986 unreserved+reserved+'%'),
// КРОМЕ запятой: запятая служит разделителем нескольких URL в одном токене. Кавычки,
// угловые/фигурные скобки, |, \, ^, backtick и пробел останавливают URL.
func isURLByte(c byte) bool {
	if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' {
		return true
	}
	switch c {
	case '-', '.', '_', '~', ':', '/', '?', '#', '[', ']', '@',
		'!', '$', '&', '\'', '(', ')', '*', '+', ';', '=', '%':
		return true
	}
	return false
}

// matchingOpener — парная открывающая скобка для закрывающей, иначе 0.
func matchingOpener(c byte) byte {
	switch c {
	case ')':
		return '('
	case ']':
		return '['
	case '}':
		return '{'
	}
	return 0
}

// isURLTrailByte — хвостовая пунктуация, не входящая в URL (обрамление предложения).
func isURLTrailByte(c byte) bool {
	switch c {
	case ')', ']', '}', '"', '\'', '`', '>', ',', '.', ';', ':', '!', '?':
		return true
	}
	return false
}

func (s *Scrubber) ScrubTags(tags map[string]string) {
	if s == nil {
		return
	}
	for k, v := range tags {
		if s.denied(k) {
			tags[k] = scrubMask
			continue
		}
		// Значение тега тоже может нести URL с токеном (url/referer/server_name) —
		// чистим так же, как строковые листья в ScrubData (SEC-P2-6/M1/M3).
		tags[k] = s.scrubStringLeaf(v)
	}
}

func (s *Scrubber) ScrubData(m map[string]any) {
	if s == nil {
		return
	}
	s.walk(m)
}

// decodeJSONValue разбирает ОДНО JSON-значение, сохраняя числа как json.Number
// (не терять точность bigint/snowflake-id при round-trip). Возвращает ok=false на
// невалидном JSON ИЛИ если после первого значения есть непробельный хвост
// (NDJSON / "{…} мусор") — такой вход не наш, вызывающий трактует его иначе, а не
// молча усекает (SEC-P2-3).
func decodeJSONValue(raw string) (any, bool) {
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, false
	}
	// dec.More() возвращает false на хвосте '}' или ']' (peek видит закрывающую
	// скобку), поэтому "{…}}" молча усекался бы. Читаем следующий токен: EOF —
	// значение единственное; что угодно другое — хвост, вход не наш (SEC-P2-2).
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, false
	}
	return v, true
}

// encodeJSONValue сериализует без HTML-эскейпа (&,<,> в URL/HTML внутри тела не
// ломаются) и без хвостового '\n', который добавляет json.Encoder.
func encodeJSONValue(v any) (string, bool) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "", false
	}
	return strings.TrimSuffix(buf.String(), "\n"), true
}

func (s *Scrubber) ScrubJSON(raw string) string {
	if s == nil || raw == "" {
		return raw
	}
	v, ok := decodeJSONValue(raw)
	if !ok {
		return raw // невалидный JSON (или мусорный хвост) не трогаем
	}
	v = s.scrubValue(v)
	out, ok := encodeJSONValue(v)
	if !ok {
		return raw
	}
	return out
}

func (s *Scrubber) walk(m map[string]any) {
	for k, val := range m {
		if s.denied(k) {
			m[k] = scrubMask
			continue
		}
		// denylist сравнивает КЛЮЧИ, но url/query_string/тело-формы несут
		// секреты (token=…&password=…) ВНУТРИ строкового значения. Их надо
		// разобрать по параметрам и вычистить по тем же denylist-именам —
		// иначе reset-токены/API-ключи/пароли осели бы в CH и на детали issue.
		switch strings.ToLower(k) {
		case "url", "http.url":
			if str, ok := val.(string); ok {
				m[k] = s.scrubURLParams(str)
				continue
			}
		case "query_string", "querystring", "data", "body", "headers":
			// headers — тоже: Sentry шлёт их и как массив пар [[name,value],…],
			// где denied() по имени иначе не срабатывает (Authorization/X-Api-Key
			// в паре утекали бы). scrubQueryLike знает все три формы.
			m[k] = s.scrubQueryLike(val)
			continue
		}
		m[k] = s.scrubValue(val)
	}
}

// scrubParams вычищает значения denylist-параметров в form-encoded строке
// (a=b&token=…&c=d), сохраняя остальные сегменты байт-в-байт: незапрещённые
// параметры и non-form-строки (JSON-тело и т.п.) не искажаются.
func (s *Scrubber) scrubParams(query string) string {
	if query == "" {
		return query
	}
	parts := strings.Split(query, "&")
	for i, p := range parts {
		eq := strings.IndexByte(p, '=')
		if eq < 0 {
			continue
		}
		name := p[:eq]
		dec := name
		if u, err := url.QueryUnescape(name); err == nil {
			dec = u
		}
		if s.deniedParam(dec) {
			parts[i] = name + "=" + scrubMask
		}
	}
	// Свободный текст в значениях (email при ScrubFreeText) маскируется тем же
	// ScrubText, что и раньше через scrubValue: без этого новый разбор параметров
	// молча ослаблял free-text-скраб именно в url/query_string/теле. ScrubText —
	// no-op при ScrubFreeText=false, а Join(Split(x,"&"),"&") == x, поэтому в
	// дефолте строка возвращается байт-в-байт.
	return s.ScrubText(strings.Join(parts, "&"))
}

// scrubURLParams вычищает секреты в URL: denylist-параметры и в query, и во
// ФРАГМЕНТЕ (implicit-flow OAuth кладёт access_token в #fragment), пароль в
// basic-auth (scheme://user:pass@host), плюс email в пути/значениях (free-text).
func (s *Scrubber) scrubURLParams(u string) string {
	// Фрагмент отделяем первым — он может нести токены (#access_token=… или
	// hash-router #/path?token=…), а не только якорь.
	frag := ""
	if h := strings.IndexByte(u, '#'); h >= 0 {
		frag, u = s.scrubFragment(u[h:]), u[:h]
	}
	q := strings.IndexByte(u, '?')
	if q < 0 {
		return s.ScrubText(s.stripUserinfo(u)) + frag // query нет
	}
	base, rest := u[:q], u[q+1:]
	return s.ScrubText(s.stripUserinfo(base)) + "?" + s.scrubParams(rest) + frag
}

// scrubFragment чистит параметры во фрагменте URL. Просто путь/якорь (#/path,
// #section) → без изменений; #access_token=… и #/path?token=… → параметры
// прогоняются через denylist.
func (s *Scrubber) scrubFragment(frag string) string {
	if frag == "" {
		return frag
	}
	body := frag[1:] // без ведущего '#'
	if q := strings.IndexByte(body, '?'); q >= 0 {
		// body[:q] — путь hash-роутера (#/users/john@example.com): тоже free-text,
		// прогоняем через ScrubText, иначе email в нём переживёт маскирование.
		return "#" + s.ScrubText(body[:q]) + "?" + s.scrubParams(body[q+1:])
	}
	return "#" + s.scrubParams(body)
}

// stripUserinfo маскирует пароль в basic-auth части URL: scheme://user:pass@host
// → scheme://user:[scrubbed]@host. Трогает только authority (до первого '/').
func (s *Scrubber) stripUserinfo(u string) string {
	si := strings.Index(u, "://")
	if si < 0 {
		return u
	}
	rest := u[si+3:]
	// authority — часть до первого '/'; '@' ищем в ней и берём ПОСЛЕДНИЙ,
	// чтобы незакодированный '@' в пароле (user:p@ss@host) не оставил хвост.
	authority := rest
	if slash := strings.IndexByte(rest, '/'); slash >= 0 {
		authority = rest[:slash]
	}
	at := strings.LastIndexByte(authority, '@')
	if at <= 0 {
		return u // '@' нет в authority, он в пути, или userinfo пуст (scheme://@host)
	}
	userinfo := rest[:at]
	if colon := strings.IndexByte(userinfo, ':'); colon >= 0 {
		userinfo = userinfo[:colon+1] + scrubMask // user:pass → user:[scrubbed]
	} else {
		userinfo = scrubMask // одиночный userinfo (обычно токен/PAT, scheme://ghp_…@host)
	}
	return u[:si+3] + userinfo + rest[at:]
}

// scrubQueryLike чистит query_string/тело формы в любой из форм, что шлют SDK:
// строка (a=b&…), массив пар [[name,value],…] или объект {name:value}.
func (s *Scrubber) scrubQueryLike(v any) any {
	switch t := v.(type) {
	case string:
		return s.scrubMaybeJSON(t)
	case map[string]any:
		s.walk(t) // объектная форма: имена — это КЛЮЧИ, ловятся denied()
		return t
	case []any:
		for i, e := range t {
			if pair, ok := e.([]any); ok && len(pair) == 2 {
				name, _ := pair[0].(string)
				if s.denied(name) {
					pair[1] = scrubMask
				} else if sv, ok := pair[1].(string); ok {
					pair[1] = s.scrubStringLeaf(sv) // значение пары: URL → params, иначе free-text
				}
				t[i] = pair
				continue
			}
			t[i] = s.scrubValue(e)
		}
		return t
	}
	return s.scrubValue(v)
}

// scrubMaybeJSON: строковое тело/данные, начинающееся с { или [ — это JSON
// (частая форма тела запроса), поэтому разбираем и чистим рекурсивно (denylist
// по ключам + free-text по значениям). Иначе, если значение — это ЦЕЛИКОМ URL,
// чистим его полным URL-скрабом (фрагмент/basic-auth под data/body/query иначе не
// трогались — SEC-P1-1); в остальных случаях трактуем как form-encoded query.
func (s *Scrubber) scrubMaybeJSON(str string) string {
	if t := strings.TrimSpace(str); strings.HasPrefix(t, "{") || strings.HasPrefix(t, "[") {
		// объект → чистка по КЛЮЧАМ (denied); массив пар [[name,value]] → парная
		// чистка (scrubValue не знает про пары, а scrubQueryLike знает). Одно ИЛИ
		// несколько значений подряд (NDJSON / concatenated) — каждое чистится.
		if out, ok := s.scrubJSONStream(str); ok {
			return out
		}
		// невалидный JSON — падаем ниже
	}
	if looksLikeURL(str) {
		return s.scrubURLParams(str)
	}
	// Form-encoded query (token=…&password=…) ПЛЮС встроенные в текст URL: тело под
	// data/body вида "GET https://api/x?token=…" иначе не чистилось (scrubParams не
	// видит URL, а deniedParam не матчит "GET https://…?token" как имя) — SEC-P1-3.
	return s.scrubEmbeddedURLs(s.scrubParams(str))
}

// scrubJSONStream чистит поток из одного или нескольких JSON-значений подряд
// (обычное тело — одно; NDJSON / concatenated — несколько), сохраняя данные из
// всех: раньше хвост после первого значения молча терялся, а с ним утекал секрет
// (SEC-P2-1). ok=false, если это не валидная последовательность JSON (тогда
// вызывающий трактует вход как form/URL). Многозначный поток пере-склеивается
// через '\n' (канон NDJSON); одиночное значение сериализуется как есть.
func (s *Scrubber) scrubJSONStream(str string) (string, bool) {
	dec := json.NewDecoder(strings.NewReader(str))
	dec.UseNumber()
	var parts []string
	for {
		var v any
		err := dec.Decode(&v)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", false
		}
		out, ok := encodeJSONValue(s.scrubQueryLike(v))
		if !ok {
			return "", false
		}
		parts = append(parts, out)
	}
	if len(parts) == 0 {
		return "", false
	}
	return strings.Join(parts, "\n"), true
}

// scrubValue рекурсивно чистит произвольное значение и возвращает результат
// (строковые листья могут замениться, поэтому возврат, а не in-place). Карты и
// срезы обходятся; строковые ЗНАЧЕНИЯ прогоняются через ScrubText — так email в
// свободном тексте кадров стектрейса, contexts и span.data маскируется даже там,
// где denylist по КЛЮЧАМ не сработал (RA-L10). ScrubText — no-op при
// ScrubFreeText=false, поэтому поведение по умолчанию не меняется.
func (s *Scrubber) scrubValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		s.walk(t)
		return t
	case []any:
		for i, e := range t {
			t[i] = s.scrubValue(e)
		}
		return t
	case string:
		return s.scrubStringLeaf(t)
	}
	return v
}

// looksLikeURL — дешёвый признак «строка ЦЕЛИКОМ это URL» (scheme://…) без regexp
// на горячем пути инжеста: схема из isSchemeByte, затем "://". Схема ≤20 символов
// (chrome-extension:// = 16), поэтому "://" ищется только в первых 23 байтах — O(1).
// Пробел/перенос ⇒ это НЕ «целиком URL», а текст со встроенным URL (его обрабатывает
// ветка scrubEmbeddedURLs в scrubStringLeaf), поэтому такие строки отвергаем.
func looksLikeURL(u string) bool {
	head := u
	if len(head) > 23 {
		head = head[:23]
	}
	i := strings.Index(head, "://")
	if i <= 0 {
		return false
	}
	for j := 0; j < i; j++ {
		if !isSchemeByte(u[j]) {
			return false
		}
	}
	return !strings.ContainsAny(u, " \t\n\r\v\f")
}

// scrubStringLeaf чистит строковый лист под ЛЮБЫМ ключом. Есть "://" — это URL
// целиком (url.full/Referer, SEC-M1/M3) ИЛИ встроенный в текст (breadcrumb.message/
// db.statement, SEC-P1-2): ScrubMessage покрывает оба (scrubURLToken распознаёт и
// «вся строка URL», и URL внутри токена, а scrubURLParams чистит fragment/basic-auth).
// Прочее — free-text email через ScrubText.
func (s *Scrubber) scrubStringLeaf(t string) string {
	if strings.Contains(t, "://") {
		return s.ScrubMessage(t)
	}
	return s.ScrubText(t)
}
