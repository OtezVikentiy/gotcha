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

// denied — совпадает ли ключ (или его подстрока) с denylist.
func (s *Scrubber) denied(key string) bool {
	k := strings.ToLower(key)
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
	s.walkAny(v)
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
		s.walkAny(val)
	}
}

func (s *Scrubber) walkAny(v any) {
	switch t := v.(type) {
	case map[string]any:
		s.walk(t)
	case []any:
		for _, e := range t {
			s.walkAny(e)
		}
	}
}
