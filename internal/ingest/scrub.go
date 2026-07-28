package ingest

import (
	"bytes"
	"encoding/json"
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
			denyNorm[normKey(k)] = true
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
// формы, в которых их шлют SDK: X-Forwarded-For, x_forwarded_for и CGI/WSGI-форма
// HTTP_X_FORWARDED_FOR (PHP/WSGI кладут её в request.env) — все дают «…xforwardedfor…».
var ipHeaderKeysNorm = []string{"xforwardedfor", "xrealip", "xclientip", "remoteaddr"}

// denied — совпадает ли ключ (или его подстрока) с denylist, либо это email-ключ
// при включённом ScrubEmail / IP-ключ при включённом ScrubIP.
func (s *Scrubber) denied(key string) bool {
	k := strings.ToLower(key)
	if s.ScrubEmail && emailAttrKeys[k] {
		return true
	}
	if s.ScrubIP {
		if ipAttrKeys[k] {
			return true
		}
		kn := normKey(k)
		for _, ipk := range ipHeaderKeysNorm {
			if strings.Contains(kn, ipk) {
				return true
			}
		}
	}
	if s.deny[k] {
		return true
	}
	for d := range s.deny {
		if strings.Contains(k, d) {
			return true
		}
	}
	// Нормализованное сравнение: X-Api-Key/Api-Key ловятся ключом api_key,
	// хотя дефис не совпадает с подчёркиванием при подстрочном матче выше.
	kn := normKey(k)
	for d := range s.denyNorm {
		if strings.Contains(kn, d) {
			return true
		}
	}
	return false
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
	return s.scrubEmbeddedURLs(s.ScrubText(text))
}

// scrubEmbeddedURLs находит URL-подстроки (scheme://… до пробела) в свободном
// тексте и прогоняет каждую через scrubURLParams. Не-URL текст и текст без "://"
// возвращаются байт-в-байт (форматирование не трогаем, если чистить нечего).
func (s *Scrubber) scrubEmbeddedURLs(text string) string {
	if !strings.Contains(text, "://") {
		return text
	}
	fields := strings.Fields(text)
	changed := false
	for i, f := range fields {
		if looksLikeURL(f) {
			if scrubbed := s.scrubURLParams(f); scrubbed != f {
				fields[i] = scrubbed
				changed = true
			}
		}
	}
	if !changed {
		return text // "://" есть, но чистить нечего — не нормализуем пробелы
	}
	return strings.Join(fields, " ")
}

func (s *Scrubber) ScrubTags(tags map[string]string) {
	if s == nil {
		return
	}
	for k := range tags {
		if s.denied(k) {
			tags[k] = scrubMask
		}
	}
}

func (s *Scrubber) ScrubData(m map[string]any) {
	if s == nil {
		return
	}
	s.walk(m)
}

func (s *Scrubber) ScrubJSON(raw string) string {
	if s == nil || raw == "" {
		return raw
	}
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return raw // невалидный JSON не трогаем
	}
	v = s.scrubValue(v)
	out, err := json.Marshal(v)
	if err != nil {
		return raw
	}
	return string(out)
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
		if s.denied(dec) {
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
	if at < 0 {
		return u // '@' нет в authority (или он в пути) — не userinfo
	}
	userinfo := rest[:at]
	if colon := strings.IndexByte(userinfo, ':'); colon >= 0 {
		userinfo = userinfo[:colon+1] + scrubMask
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
// по ключам + free-text по значениям), иначе трактуем как form-encoded. Так JSON
// с паролем реально вычищается и НЕ портится, если значение содержит '=' (иначе
// scrubParams обрезал бы сегмент по первому '=').
func (s *Scrubber) scrubMaybeJSON(str string) string {
	if t := strings.TrimSpace(str); strings.HasPrefix(t, "{") || strings.HasPrefix(t, "[") {
		dec := json.NewDecoder(strings.NewReader(str))
		dec.UseNumber() // не терять точность bigint/snowflake-id при round-trip
		var v any
		if err := dec.Decode(&v); err == nil {
			// объект → чистка по КЛЮЧАМ (denied); массив пар [[name,value]] →
			// парная чистка (scrubValue не знает про пары, а scrubQueryLike знает).
			v = s.scrubQueryLike(v)
			var buf bytes.Buffer
			enc := json.NewEncoder(&buf)
			enc.SetEscapeHTML(false) // не ломать &,<,> в URL/HTML внутри тела
			if err := enc.Encode(v); err == nil {
				return strings.TrimRight(buf.String(), "\n") // Encode добавляет '\n'
			}
		}
	}
	return s.scrubParams(str)
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

// looksLikeURL — дешёвый признак «строка это URL» (scheme://…) без regexp на
// горячем пути инжеста: схема ≤10 символов из [a-z0-9+.-], затем "://".
func looksLikeURL(s string) bool {
	i := strings.Index(s, "://")
	if i <= 0 || i > 10 {
		return false
	}
	for j := 0; j < i; j++ {
		c := s[j]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			c == '+' || c == '.' || c == '-') {
			return false
		}
	}
	return true
}

// scrubStringLeaf чистит строковый лист: если это URL — через scrubURLParams
// (query-токены, fragment, basic-auth), иначе free-text через ScrubText. Так
// токен в url.full / request.url / значении заголовка Referer|Location
// маскируется независимо от имени ключа, под которым URL прилетел (SEC-M1/M3).
func (s *Scrubber) scrubStringLeaf(t string) string {
	if looksLikeURL(t) {
		return s.scrubURLParams(t)
	}
	return s.ScrubText(t)
}
