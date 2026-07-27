package ingest

import (
	"encoding/json"
	"net/url"
	"regexp"
	"strings"
)

const scrubMask = "[scrubbed]"

// sepReplacer убирает разделители из имени ключа перед сравнением с denylist:
// заголовок X-Api-Key и ключ api_key должны совпасть, хотя дефис ≠ подчёркивание.
var sepReplacer = strings.NewReplacer("-", "", "_", "", " ", "")

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

// denied — совпадает ли ключ (или его подстрока) с denylist, либо это email-ключ
// при включённом ScrubEmail / IP-ключ при включённом ScrubIP.
func (s *Scrubber) denied(key string) bool {
	k := strings.ToLower(key)
	if s.ScrubEmail && emailAttrKeys[k] {
		return true
	}
	if s.ScrubIP && ipAttrKeys[k] {
		return true
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
		case "query_string", "querystring", "data", "body":
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
	changed := false
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
			changed = true
		}
	}
	if !changed {
		return query
	}
	return strings.Join(parts, "&")
}

// scrubURLParams вычищает denylist-параметры в query-части URL, не трогая путь
// и фрагмент. Схема/хост остаются как есть (basic-auth в userinfo — отдельно).
func (s *Scrubber) scrubURLParams(u string) string {
	q := strings.IndexByte(u, '?')
	if q < 0 {
		return u
	}
	base, rest := u[:q], u[q+1:]
	frag := ""
	if h := strings.IndexByte(rest, '#'); h >= 0 {
		frag, rest = rest[h:], rest[:h]
	}
	return base + "?" + s.scrubParams(rest) + frag
}

// scrubQueryLike чистит query_string/тело формы в любой из форм, что шлют SDK:
// строка (a=b&…), массив пар [[name,value],…] или объект {name:value}.
func (s *Scrubber) scrubQueryLike(v any) any {
	switch t := v.(type) {
	case string:
		return s.scrubParams(t)
	case map[string]any:
		s.walk(t) // объектная форма: имена — это КЛЮЧИ, ловятся denied()
		return t
	case []any:
		for i, e := range t {
			if pair, ok := e.([]any); ok && len(pair) == 2 {
				if name, ok := pair[0].(string); ok && s.denied(name) {
					pair[1] = scrubMask
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
		return s.ScrubText(t)
	}
	return v
}
