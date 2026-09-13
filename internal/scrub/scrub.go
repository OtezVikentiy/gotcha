package scrub

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

// Убирает разделители перед сравнением с denylist: X-Api-Key и api_key должны совпасть.
var sepReplacer = strings.NewReplacer("-", "", "_", "", " ", "", ".", "")

// Живёт здесь, а не в cmd/gotcha/config.go — export должен маскировать теми же ключами.
var defaultDenyKeys = []string{
	"password", "passwd", "pwd", "pass", "token", "secret", "authorization", "auth",
	"cookie", "api_key", "apikey", "access_token", "refresh_token",
	"session", "credit_card", "card_number", "cvv",
}

// Копия — чтобы вызывающий (например, export.MaskJSON) не мог испортить общий список.
func DefaultDenyKeys() []string { return append([]string(nil), defaultDenyKeys...) }

func normKey(s string) string { return sepReplacer.Replace(strings.ToLower(s)) }

// Отдельна от scrubMask: тут редактируется подстрока значения, а не всё поле.
const emailTextMask = "[email]"

// Юникод-классы (\p{L}\p{N}), а не \w — в Go \w это ASCII-only, кириллические
// адреса вида иван@пример.рф иначе не ловятся.
var emailTextRe = regexp.MustCompile(`[\p{L}\p{N}._+-]+@[\p{L}\p{N}-]+\.[\p{L}\p{N}.-]+`)

type Scrubber struct {
	ScrubIP    bool
	ScrubEmail bool
	// Дефолт false — включается из main.go установкой поля (GOTCHA_SCRUB_FREETEXT).
	ScrubFreeText bool
	denyNorm      []string
	allowNorm     map[string]bool
}

func NewScrubber(scrubIP, scrubEmail bool, denyKeys []string) *Scrubber {
	s := &Scrubber{ScrubIP: scrubIP, ScrubEmail: scrubEmail, allowNorm: map[string]bool{}}
	for _, k := range denyKeys {
		// normKey может дать "" на ключе из одних разделителей — пустая подстрока
		// матчила бы любое имя. Пропускаем.
		if kn := normKey(strings.TrimSpace(k)); kn != "" {
			s.denyNorm = append(s.denyNorm, kn)
		}
	}
	return s
}

func (s *Scrubber) SetAllowKeys(keys []string) {
	s.allowNorm = make(map[string]bool, len(keys))
	for _, k := range keys {
		if kn := normKey(strings.TrimSpace(k)); kn != "" {
			s.allowNorm[kn] = true
		}
	}
}

// «email» подстрокой ловит все формы разом (user.email, user_email, E-Mail).
// user_id/enduser.id НЕ трогаем — это идентификатор, не сам email.
var emailAttrKeysNorm = []string{"email"}

// Подстроки подобраны так, чтобы НЕ задевать X-Forwarded-Proto/Host/Port.
var ipAttrKeysNorm = []string{
	"userip",          // user.ip, user_ip, sentry.user.ip
	"ipaddress",       // ip_address, sentry.user.ip_address, IPAddress
	"clientaddress",   // client.address, client_address
	"netpeerip",       // net.peer.ip
	"netsockpeeraddr", // net.sock.peer.addr
	"clientip",        // http.client_ip, X-Client-IP, True-Client-IP, X-Cluster-Client-IP
	"forwardedfor",    // X-Forwarded-For, HTTP_X_FORWARDED_FOR
	"realip",          // X-Real-IP
	"remoteaddr",      // REMOTE_ADDR
	"connectingip",    // CF-Connecting-IP
	"peeraddress",     // network.peer.address (стабильная замена net.peer.ip в OTel semconv), peer.address
	"localaddress",    // network.local.address
}

// Матчится ТОЧНО, не подстрочно: подстрока "forwarded" задела бы
// X-Forwarded-Proto/Host/Port, которые IP не несут.
const forwardedExact = "forwarded"

// Матч ПОДСТРОЧНЫЙ и fail-closed по построению: author⊃auth и tokenizer⊃token
// маскируются по умолчанию — цена дешевле утечки ПДн, лечится SetAllowKeys.
func (s *Scrubber) denied(name string) bool {
	if s == nil {
		return false
	}
	k := strings.ToLower(name)
	kn := normKey(k)
	if s.allowNorm[kn] {
		return false
	}
	if s.ScrubEmail {
		for _, n := range emailAttrKeysNorm {
			if strings.Contains(kn, n) {
				return true
			}
		}
	}
	if s.ScrubIP {
		if kn == forwardedExact {
			return true
		}
		for _, n := range ipAttrKeysNorm {
			if strings.Contains(kn, n) {
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

func (s *Scrubber) ScrubText(text string) string {
	if s == nil || !s.ScrubFreeText || text == "" {
		return text
	}
	return emailTextRe.ReplaceAllString(text, emailTextMask)
}

// URL-часть чистится ВСЕГДА, не только при ScrubFreeText: query-токен в
// message/имени — утечка и в дефолтном скрабинге.
func (s *Scrubber) ScrubMessage(text string) string {
	if s == nil || text == "" {
		return text
	}
	// URL-скраб первым — иначе email-матч принял бы "pass@host.tld" из
	// basic-auth за email и испортил бы домен.
	return s.ScrubText(s.scrubURLsIn(text))
}

// RE2 линеен по входу — ни квадратичного поведения на длинных хвостах, ни
// зависимости от байтовой природы UTF-8 (кириллица в пути/query — часть URL).
var urlInTextRe = regexp.MustCompile("[a-zA-Z][a-zA-Z0-9+.\\-]{0,19}://[^\\s\"'`<>\\\\^|{}]*")

// Глубже — патология, не боевые данные; ограничивает и переполнение стека.
const maxURLNestDepth = 4

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

// Закрывающая скобка остаётся в URL, если есть парная открывающая — иначе это
// обрамление предложения, а не значение (маска token=[Filtered], IPv6 [::1]).
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
		// Значение тега тоже может нести URL с токеном (referer/server_name).
		tags[k] = s.scrubStringLeaf(v)
	}
}

func (s *Scrubber) ScrubData(m map[string]any) {
	if s == nil {
		return
	}
	s.walk(m)
}

// Числа как json.Number — не теряет точность bigint/snowflake-id при round-trip.
// ok=false и на валидном значении с непробельным хвостом (NDJSON/мусор) — не наш вход.
func decodeJSONValue(raw string) (any, bool) {
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, false
	}
	// dec.More() не видит хвост после закрывающей скобки — читаем следующий
	// токен: EOF значит хвоста нет, что угодно другое — вход не наш.
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, false
	}
	return v, true
}

// Без HTML-эскейпа (&,<,> в URL внутри тела не ломаются) и без хвостового
// '\n', который добавляет json.Encoder по умолчанию.
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
		return raw
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
		// denylist ловит по ключу, но url/query_string/тело формы несут секреты
		// ВНУТРИ строкового значения — разбираем по параметрам теми же именами.
		switch strings.ToLower(k) {
		case "url", "http.url":
			if str, ok := val.(string); ok {
				m[k] = s.scrubURLParams(str)
				continue
			}
		case "query_string", "querystring", "data", "body", "headers":
			// headers — тоже: Sentry шлёт их и как массив пар [[name,value],…],
			// где denied() по имени не срабатывает.
			m[k] = s.scrubQueryLike(val)
			continue
		}
		m[k] = s.scrubValue(val)
	}
}

// Незапрещённый параметр может сам содержать URL (?next=https://…) — чистится
// рекурсивно, иначе секрет во вложенном URL уезжает сырым.
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
	// Свободный текст в значениях (email при ScrubFreeText) чистится тем же
	// ScrubText под конец — no-op при флаге выключенном, строка не меняется побайтово.
	return s.ScrubText(strings.Join(parts, "&"))
}

// Чистит query И фрагмент (#access_token — implicit OAuth), basic-auth пароль и email в пути.
func (s *Scrubber) scrubURLParams(u string) string { return s.scrubURLParamsDepth(u, 0) }

func (s *Scrubber) scrubURLParamsDepth(u string, depth int) string {
	// Фрагмент отделяем первым — он может нести токены (#access_token=…),
	// а не только якорь.
	frag := ""
	if h := strings.IndexByte(u, '#'); h >= 0 {
		frag, u = s.scrubFragmentDepth(u[h:], depth), u[:h]
	}
	q := strings.IndexByte(u, '?')
	if q < 0 {
		return s.ScrubText(s.stripUserinfo(u)) + frag
	}
	base, rest := u[:q], u[q+1:]
	return s.ScrubText(s.stripUserinfo(base)) + "?" + s.scrubParamsDepth(rest, depth) + frag
}

func (s *Scrubber) scrubFragmentDepth(frag string, depth int) string {
	if frag == "" {
		return frag
	}
	body := frag[1:]
	if q := strings.IndexByte(body, '?'); q >= 0 {
		// body[:q] — путь hash-роутера (#/users/john@example.com): тоже
		// free-text, иначе email в нём переживёт маскирование.
		return "#" + s.ScrubText(body[:q]) + "?" + s.scrubParamsDepth(body[q+1:], depth)
	}
	return "#" + s.scrubParamsDepth(body, depth)
}

func (s *Scrubber) stripUserinfo(u string) string {
	si := strings.Index(u, "://")
	if si < 0 {
		return u
	}
	rest := u[si+3:]
	// '@' ищем в authority (до первого '/') и берём ПОСЛЕДНИЙ — незакодированный
	// '@' в пароле (user:p@ss@host) иначе оставил бы хвост.
	authority := rest
	if slash := strings.IndexByte(rest, '/'); slash >= 0 {
		authority = rest[:slash]
	}
	at := strings.LastIndexByte(authority, '@')
	if at <= 0 {
		return u
	}
	userinfo := rest[:at]
	if colon := strings.IndexByte(userinfo, ':'); colon >= 0 {
		userinfo = userinfo[:colon+1] + scrubMask
	} else {
		userinfo = scrubMask // одиночный userinfo — обычно токен/PAT, scheme://ghp_…@host
	}
	return u[:si+3] + userinfo + rest[at:]
}

func (s *Scrubber) scrubQueryLike(v any) any {
	switch t := v.(type) {
	case string:
		return s.scrubMaybeJSON(t)
	case map[string]any:
		s.walk(t)
		return t
	case []any:
		for i, e := range t {
			if pair, ok := e.([]any); ok && len(pair) == 2 {
				name, _ := pair[0].(string)
				if s.denied(name) {
					pair[1] = scrubMask
				} else if sv, ok := pair[1].(string); ok {
					pair[1] = s.scrubStringLeaf(sv)
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

func (s *Scrubber) scrubMaybeJSON(str string) string {
	if t := strings.TrimSpace(str); strings.HasPrefix(t, "{") || strings.HasPrefix(t, "[") {
		if out, ok := s.scrubJSONStream(str); ok {
			return out
		}
	}
	return s.scrubURLsIn(s.scrubParams(str))
}

// Мусорный хвост после успешно разобранного префикса дочищается отдельно как
// текст, а не отбрасывается — иначе секрет в префиксе уезжал бы открытым.
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
				return "", false
			}
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

// Возврат, а не in-place — строковые листья могут замениться. Значения (не
// только по denylist-ключам) идут через ScrubText — email в свободном тексте маскируется тоже.
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

func (s *Scrubber) scrubStringLeaf(t string) string {
	if strings.Contains(t, "://") {
		return s.ScrubMessage(t)
	}
	return s.ScrubText(t)
}
