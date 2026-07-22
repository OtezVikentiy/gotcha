package ingest

import (
	"encoding/json"
	"regexp"
	"strings"
)

const scrubMask = "[scrubbed]"

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
}

func NewScrubber(scrubIP, scrubEmail bool, denyKeys []string) *Scrubber {
	deny := make(map[string]bool, len(denyKeys))
	for _, k := range denyKeys {
		if k = strings.ToLower(strings.TrimSpace(k)); k != "" {
			deny[k] = true
		}
	}
	return &Scrubber{ScrubIP: scrubIP, ScrubEmail: scrubEmail, deny: deny}
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
		m[k] = s.scrubValue(val)
	}
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
