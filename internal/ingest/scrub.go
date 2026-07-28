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
	denyNorm      []string        // denylist, нормализованный (см. normKey)
	allowNorm     map[string]bool // точные имена-исключения (см. SetAllowKeys)
}

func NewScrubber(scrubIP, scrubEmail bool, denyKeys []string) *Scrubber {
	s := &Scrubber{ScrubIP: scrubIP, ScrubEmail: scrubEmail, allowNorm: map[string]bool{}}
	for _, k := range denyKeys {
		// normKey даёт "" на ключе из одних разделителей ("-", "_"): пустая подстрока
		// матчилась бы с ЛЮБЫМ именем и замаскировала бы всё. Пропускаем.
		if kn := normKey(strings.TrimSpace(k)); kn != "" {
			s.denyNorm = append(s.denyNorm, kn)
		}
	}
	return s
}

// SetAllowKeys задаёт имена-исключения из denylist (GOTCHA_SCRUB_ALLOW_KEYS).
// Матч denylist намеренно подстрочный и fail-closed (см. denied), поэтому
// безобидные поля вроде author (⊃auth) или tokenizer (⊃token) по умолчанию
// маскируются. Оператор возвращает нужные ему поля точным именем — это
// осознанное решение с его стороны, а не молчаливая дыра в скрубере.
func (s *Scrubber) SetAllowKeys(keys []string) {
	s.allowNorm = make(map[string]bool, len(keys))
	for _, k := range keys {
		if kn := normKey(strings.TrimSpace(k)); kn != "" {
			s.allowNorm[kn] = true
		}
	}
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

// denied — маскировать ли значение под этим ИМЕНЕМ. ЕДИНОЕ правило для всех
// поверхностей: ключи объектов, имена query-параметров, заголовки, теги, атрибуты.
// Имя нормализуется (регистр и разделители -_. игнорируются) и проверяется на
// ВХОЖДЕНИЕ denylist-слова: api_key ловит x_api_key/X-Api-Key/apiKey, token ловит
// mytoken/id_token/sessionToken, secret ловит client_secret/clientsecret.
//
// Правило намеренно FAIL-CLOSED: под-scrub — это утечка ПДн, over-scrub — потеря
// отладочного поля, обратимая через SetAllowKeys. Поэтому подстрока, а не «слово»
// или точное равенство: любые будущие и склеенные имена ловятся по построению, а
// цена — author⊃auth, tokenizer⊃token маскируются по умолчанию (лечится allowlist).
func (s *Scrubber) denied(name string) bool {
	if s == nil {
		return false
	}
	k := strings.ToLower(name)
	kn := normKey(k)
	if s.allowNorm[kn] {
		return false // оператор явно разрешил это имя
	}
	if s.ScrubEmail && emailAttrKeys[k] {
		return true
	}
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
	for _, d := range s.denyNorm {
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
	// URL-скраб ПЕРВЫМ: он снимает basic-auth (user:pass@host) внутри URL, иначе
	// свободнотекстовый email-матч ошибочно принял бы "pass@host.tld" за email и
	// замаскировал бы домен. Free-text ScrubText — вторым, по не-URL остатку.
	return s.ScrubText(s.scrubURLsIn(text))
}

// urlInTextRe находит URL внутри свободного текста: схема (буква + до 20 символов
// [a-z0-9+.-]) + "://" + тело до пробела или символа, который не может быть частью
// URL (кавычки, угловые/фигурные скобки, |, \, ^, backtick). Не-ASCII байты —
// кириллица в пути/query — частью URL СЧИТАЮТСЯ. Поиск через regexp, а не ручной
// посимвольный разбор: RE2 линеен по входу, поэтому ни квадратичного поведения на
// длинных хвостах, ни зависимости от байтовой природы UTF-8 тут нет по построению.
var urlInTextRe = regexp.MustCompile("[a-zA-Z][a-zA-Z0-9+.\\-]{0,19}://[^\\s\"'`<>\\\\^|{}]*")

// maxURLNestDepth — предел рекурсии «URL внутри значения параметра» (?next=https://…).
// Вложенность глубже — патология, а не боевые данные; ограничение исключает и
// переполнение стека на подобранном входе.
const maxURLNestDepth = 4

// scrubURLsIn чистит КАЖДЫЙ URL, встреченный в тексте, а всё остальное сохраняет
// БАЙТ-В-БАЙТ: разделители, переносы строк и обрамление не трогаются, а когда
// чистить нечего — возвращается исходная строка без единой аллокации.
func (s *Scrubber) scrubURLsIn(text string) string { return s.scrubURLsInDepth(text, 0) }

func (s *Scrubber) scrubURLsInDepth(text string, depth int) string {
	if !strings.Contains(text, "://") {
		return text
	}
	locs := urlInTextRe.FindAllStringIndex(text, -1)
	if locs == nil {
		return text
	}
	var b strings.Builder
	changed := false
	last := 0
	for _, loc := range locs {
		core, tail := splitURLTail(text[loc[0]:loc[1]])
		cleaned := s.scrubURLParamsDepth(core, depth)
		if cleaned == core {
			continue
		}
		if !changed {
			b.Grow(len(text))
			changed = true
		}
		b.WriteString(text[last:loc[0]])
		b.WriteString(cleaned)
		b.WriteString(tail)
		last = loc[1]
	}
	if !changed {
		return text
	}
	b.WriteString(text[last:])
	return b.String()
}

// splitURLTail отделяет хвостовую пунктуацию, не входящую в URL (точка или запятая
// в конце предложения, закрывающая скобка обрамления). Закрывающая скобка ОСТАЁТСЯ
// частью URL, если внутри есть парная открывающая: это значение (маска SDK
// token=[Filtered], IPv6-хост [::1]), а не обрамление. Наличие открывающих
// считается ОДИН раз — проверка внутри цикла тримминга дала бы O(n²).
func splitURLTail(u string) (core, tail string) {
	var hasParen, hasBracket bool
	for i := 0; i < len(u); i++ {
		switch u[i] {
		case '(':
			hasParen = true
		case '[':
			hasBracket = true
		}
	}
	end := len(u)
	for end > 0 {
		switch u[end-1] {
		case ')':
			if hasParen {
				return u[:end], u[end:]
			}
		case ']':
			if hasBracket {
				return u[:end], u[end:]
			}
		case ',', '.', ';', ':', '!', '?':
			// хвостовая пунктуация предложения — отрезаем
		default:
			return u[:end], u[end:]
		}
		end--
	}
	return u[:end], u[end:]
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
// параметры и non-form-строки (JSON-тело и т.п.) не искажаются. Значение
// НЕзапрещённого параметра, само содержащее URL (?next=https://h/?token=…),
// рекурсивно чистится — иначе секрет во вложенном URL уезжал бы сырым.
func (s *Scrubber) scrubParams(query string) string { return s.scrubParamsDepth(query, 0) }

func (s *Scrubber) scrubParamsDepth(query string, depth int) string {
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
			continue
		}
		if v := p[eq+1:]; depth < maxURLNestDepth && strings.Contains(v, "://") {
			parts[i] = name + "=" + s.scrubURLsInDepth(v, depth+1)
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
func (s *Scrubber) scrubURLParams(u string) string { return s.scrubURLParamsDepth(u, 0) }

func (s *Scrubber) scrubURLParamsDepth(u string, depth int) string {
	// Фрагмент отделяем первым — он может нести токены (#access_token=… или
	// hash-router #/path?token=…), а не только якорь.
	frag := ""
	if h := strings.IndexByte(u, '#'); h >= 0 {
		frag, u = s.scrubFragmentDepth(u[h:], depth), u[:h]
	}
	q := strings.IndexByte(u, '?')
	if q < 0 {
		return s.ScrubText(s.stripUserinfo(u)) + frag // query нет
	}
	base, rest := u[:q], u[q+1:]
	return s.ScrubText(s.stripUserinfo(base)) + "?" + s.scrubParamsDepth(rest, depth) + frag
}

// scrubFragment чистит параметры во фрагменте URL. Просто путь/якорь (#/path,
// #section) → без изменений; #access_token=… и #/path?token=… → параметры
// прогоняются через denylist.
func (s *Scrubber) scrubFragmentDepth(frag string, depth int) string {
	if frag == "" {
		return frag
	}
	body := frag[1:] // без ведущего '#'
	if q := strings.IndexByte(body, '?'); q >= 0 {
		// body[:q] — путь hash-роутера (#/users/john@example.com): тоже free-text,
		// прогоняем через ScrubText, иначе email в нём переживёт маскирование.
		return "#" + s.ScrubText(body[:q]) + "?" + s.scrubParamsDepth(body[q+1:], depth)
	}
	return "#" + s.scrubParamsDepth(body, depth)
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
	// Form-encoded query (token=…&password=…) ПЛЮС URL — и когда значение это URL
	// целиком, и когда он встроен в текст ("GET https://api/x?token=…"): scrubURLsIn
	// покрывает оба случая одним проходом (SEC-P1-1/P1-3).
	return s.scrubURLsIn(s.scrubParams(str))
}

// scrubJSONStream чистит поток из одного или нескольких JSON-значений подряд
// (обычное тело — одно; NDJSON / concatenated — несколько), сохраняя данные из
// всех: раньше хвост после первого значения молча терялся, а с ним утекал секрет
// (SEC-P2-1). Если после успешно разобранного префикса идёт МУСОРНЫЙ хвост, префикс
// всё равно чистится, а остаток (граница — dec.InputOffset()) прогоняется через
// текстовый фолбэк и приклеивается — иначе секрет в префиксе уезжал бы открытым
// (SEC-P1-C). ok=false только если НИ ОДНО значение не разобралось (тогда вызывающий
// трактует вход как form/URL). Многозначный поток склеивается через '\n' (канон
// NDJSON — форма любого concatenated-потока канонизируется); одиночное — как есть.
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
			if len(parts) == 0 {
				return "", false // ничего не разобрали — пусть решает вызывающий
			}
			// разобранный префикс уже почищен; необработанный остаток — текстом
			rest := s.scrubURLsIn(s.scrubParams(str[dec.InputOffset():]))
			return strings.Join(parts, "\n") + rest, true
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
