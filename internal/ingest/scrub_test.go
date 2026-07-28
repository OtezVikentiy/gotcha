package ingest

import (
	"encoding/json"
	"strings"
	"testing"
)

// ScrubUser: при ScrubIP=true ip зануляется; при ScrubEmail=false email не трогаем.
func TestScrubUser(t *testing.T) {
	s := NewScrubber(true, false, nil)
	ip := "1.2.3.4"
	email := "bob@example.com"
	s.ScrubUser(&ip, &email)
	if ip != "" {
		t.Fatalf("ip не занулён: %q", ip)
	}
	if email != "bob@example.com" {
		t.Fatalf("email не должен меняться при ScrubEmail=false: %q", email)
	}

	// nil-указатели безопасны.
	s.ScrubUser(nil, nil)
}

// ScrubTags: значение по denylist-ключу заменяется, остальные целы.
func TestScrubTags(t *testing.T) {
	s := NewScrubber(false, false, []string{"password"})
	tags := map[string]string{"password": "x", "user": "bob"}
	s.ScrubTags(tags)
	if tags["password"] != scrubMask {
		t.Fatalf("password не отредактирован: %q", tags["password"])
	}
	if tags["user"] != "bob" {
		t.Fatalf("user не должен меняться: %q", tags["user"])
	}
}

// ScrubJSON: рекурсивный обход, редакт по denylist, невалидный JSON как есть.
func TestScrubJSON(t *testing.T) {
	s := NewScrubber(false, false, []string{"token", "cookie"})
	raw := `{"a":{"token":"secret","ok":1},"cookie":"c"}`
	out := s.ScrubJSON(raw)

	var v map[string]any
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("результат — невалидный JSON: %v", err)
	}
	a := v["a"].(map[string]any)
	if a["token"] != scrubMask {
		t.Fatalf("token не отредактирован: %v", a["token"])
	}
	if a["ok"].(float64) != 1 {
		t.Fatalf("ok должен быть цел: %v", a["ok"])
	}
	if v["cookie"] != scrubMask {
		t.Fatalf("cookie не отредактирован: %v", v["cookie"])
	}

	// Невалидный JSON возвращается как есть.
	bad := `{not json`
	if got := s.ScrubJSON(bad); got != bad {
		t.Fatalf("невалидный JSON должен вернуться как есть: %q", got)
	}
}

// ScrubData: подстрочное совпадение ключа (http.authorization).
func TestScrubData(t *testing.T) {
	s := NewScrubber(false, false, []string{"authorization"})
	m := map[string]any{
		"http.authorization": "Bearer xyz",
		"http.method":        "GET",
	}
	s.ScrubData(m)
	if m["http.authorization"] != scrubMask {
		t.Fatalf("authorization не отредактирован: %v", m["http.authorization"])
	}
	if m["http.method"] != "GET" {
		t.Fatalf("method не должен меняться: %v", m["http.method"])
	}
}

// nil-Scrubber: все методы безопасны (no-op).
func TestScrubNilSafe(t *testing.T) {
	var s *Scrubber
	ip := "1.2.3.4"
	email := "bob@example.com"
	s.ScrubUser(&ip, &email)
	if ip != "1.2.3.4" || email != "bob@example.com" {
		t.Fatalf("nil-Scrubber не должен ничего менять: ip=%q email=%q", ip, email)
	}
	s.ScrubTags(map[string]string{"password": "x"})
	s.ScrubData(map[string]any{"token": "x"})
	if got := s.ScrubJSON(`{"token":"x"}`); got != `{"token":"x"}` {
		t.Fatalf("nil-Scrubber.ScrubJSON должен вернуть вход как есть: %q", got)
	}
	// RA-L10: ScrubText тоже nil-safe.
	if got := s.ScrubText("error for user@example.com"); got != "error for user@example.com" {
		t.Fatalf("nil-Scrubber.ScrubText должен вернуть вход как есть: %q", got)
	}
}

// RA-L10: при ScrubFreeText=false свободный текст не трогаем (текущее поведение).
func TestScrubTextDisabled(t *testing.T) {
	s := NewScrubber(false, false, nil) // ScrubFreeText по умолчанию false
	in := "error for user@example.com"
	if got := s.ScrubText(in); got != in {
		t.Fatalf("при выключенном флаге текст не должен меняться: %q", got)
	}
}

// RA-L10: при ScrubFreeText=true email в свободном тексте маскируется на [email],
// а остальной текст не страдает.
func TestScrubTextEnabled(t *testing.T) {
	s := NewScrubber(false, false, nil)
	s.ScrubFreeText = true

	cases := []struct{ in, want string }{
		{"error for user@example.com", "error for [email]"},
		{"contact bob.smith+tag@sub.example.co.uk now", "contact [email] now"},
		{"a@b.com and c@d.org", "[email] and [email]"},
		{"no email here", "no email here"},
		{"", ""},
		// Консервативно: только email, номера/телефоны вне скоупа.
		{"card 4111 1111 1111 1111", "card 4111 1111 1111 1111"},
	}
	for _, c := range cases {
		if got := s.ScrubText(c.in); got != c.want {
			t.Errorf("ScrubText(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestScrubRequestFreeTextAndPairs (re-audit H2 + headers-array): при
// ScrubFreeText=true email маскируется на [email] ВНУТРИ url/query_string/data
// (иначе новый разбор параметров молча ослаблял бы free-text-скраб), а denylist
// ловит секреты и в массиве пар headers (Sentry шлёт заголовки как [[k,v],…]).
func TestScrubRequestFreeTextAndPairs(t *testing.T) {
	s := NewScrubber(false, false, []string{"password", "token", "authorization"})
	s.ScrubFreeText = true

	raw := `{` +
		`"url":"https://app/users/bob@example.com?token=SECRET",` +
		`"query_string":"q=carol@example.com&password=hunter2",` +
		`"data":"note=write to dave@example.com",` +
		`"headers":[["Authorization","Bearer x"],["X-Contact","erin@example.com"]]` +
		`}`
	var r map[string]any
	if err := json.Unmarshal([]byte(s.ScrubJSON(raw)), &r); err != nil {
		t.Fatalf("scrubbed request не JSON: %v", err)
	}

	if got, want := r["url"], "https://app/users/[email]?token=[scrubbed]"; got != want {
		t.Errorf("url = %v, want %q (email в пути + token в query)", got, want)
	}
	if got, want := r["query_string"], "q=[email]&password=[scrubbed]"; got != want {
		t.Errorf("query_string = %v, want %q", got, want)
	}
	if got, want := r["data"], "note=write to [email]"; got != want {
		t.Errorf("data = %v, want %q", got, want)
	}
	hdrs, _ := r["headers"].([]any)
	if len(hdrs) != 2 {
		t.Fatalf("headers = %v, want 2 пары", r["headers"])
	}
	a0, _ := hdrs[0].([]any)
	if len(a0) != 2 || a0[1] != scrubMask {
		t.Errorf("headers[0] = %v, want Authorization → %q", hdrs[0], scrubMask)
	}
	a1, _ := hdrs[1].([]any)
	if len(a1) != 2 || a1[1] != "[email]" {
		t.Errorf("headers[1] = %v, want X-Contact email → [email]", hdrs[1])
	}
}

// TestScrubRequestFollowups (re-audit follow-ups): JSON-тело чистится по ключам
// (и не портится, если значение содержит '='); токен во ФРАГМЕНТЕ URL и пароль
// в basic-auth маскируются; IP в env/forwarding-заголовках зануляется при ScrubIP.
func TestScrubRequestFollowups(t *testing.T) {
	s := NewScrubber(true /*ScrubIP*/, false, []string{"password", "token", "access_token"})

	raw := `{` +
		`"url":"https://bob:hunter2@api.example/cb?token=SECRET#access_token=ey123&state=1",` +
		`"data":"{\"password\":\"a=b\",\"keep\":\"me\"}",` +
		`"env":{"REMOTE_ADDR":"203.0.113.9"},` +
		`"headers":{"X-Forwarded-For":"203.0.113.9","Accept":"*/*"}` +
		`}`
	var r map[string]any
	if err := json.Unmarshal([]byte(s.ScrubJSON(raw)), &r); err != nil {
		t.Fatalf("scrubbed request не JSON: %v", err)
	}

	url, _ := r["url"].(string)
	for _, secret := range []string{"hunter2", "SECRET", "ey123"} {
		if contains(url, secret) {
			t.Errorf("url всё ещё содержит секрет %q: %s", secret, url)
		}
	}
	for _, want := range []string{"bob:[scrubbed]@", "token=[scrubbed]", "access_token=[scrubbed]"} {
		if !contains(url, want) {
			t.Errorf("url не содержит %q: %s", want, url)
		}
	}

	data, _ := r["data"].(string)
	if contains(data, "a=b") || !contains(data, "[scrubbed]") || !contains(data, `"keep":"me"`) {
		t.Errorf("JSON-тело: password должен быть замаскирован без обрезки, keep цел: %s", data)
	}

	env, _ := r["env"].(map[string]any)
	if env["REMOTE_ADDR"] != scrubMask {
		t.Errorf("env.REMOTE_ADDR = %v, want %q (ScrubIP)", env["REMOTE_ADDR"], scrubMask)
	}
	hdr, _ := r["headers"].(map[string]any)
	if hdr["X-Forwarded-For"] != scrubMask {
		t.Errorf("headers.X-Forwarded-For = %v, want %q (ScrubIP)", hdr["X-Forwarded-For"], scrubMask)
	}
	if hdr["Accept"] != "*/*" {
		t.Errorf("headers.Accept = %v, want не тронут", hdr["Accept"])
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }

// TestScrubRequestReAuditFixes фиксирует правки из re-audit follow-up волны:
// URL под любым ключом (url.full/Referer), CGI-форма IP-заголовка, целостность
// bigint/HTML в JSON-теле, hash-роутер fragment и «двойной» @ в userinfo.
func TestScrubRequestReAuditFixes(t *testing.T) {
	s := NewScrubber(true /*ScrubIP*/, false, []string{"password", "token", "access_token", "authorization"})
	s.ScrubFreeText = true // включаем маскирование email в свободном тексте

	// M1: URL прилетел под ключом url.full (стабильная OTel-семантика), не "url".
	// M3: URL в значении заголовка Referer/Location.
	// F4: CGI/WSGI-форма HTTP_X_FORWARDED_FOR в request.env.
	// F1: email в пути hash-роутера (#/users/…@…?tab=1).
	// F5: незакодированный '@' в пароле не оставляет хвост.
	raw := `{` +
		`"url.full":"https://api.example/cb?token=SECRET#access_token=ey123",` +
		`"env":{"HTTP_X_FORWARDED_FOR":"203.0.113.9"},` +
		`"headers":[["Referer","https://app.example/reset?token=REFSECRET"],["Accept","*/*"]],` +
		`"frag":"https://app.example/x#/users/john@example.com?tab=1",` +
		`"auth":"https://bob:p@ss@host.example/v1"` +
		`}`
	out := s.ScrubJSON(raw)
	var r map[string]any
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("scrubbed request не JSON: %v\n%s", err, out)
	}

	for _, secret := range []string{"SECRET", "ey123", "REFSECRET", "john@example.com", "p@ss@host"} {
		if contains(out, secret) {
			t.Errorf("вывод всё ещё содержит секрет %q:\n%s", secret, out)
		}
	}
	if uf, _ := r["url.full"].(string); !contains(uf, "token=[scrubbed]") || !contains(uf, "access_token=[scrubbed]") {
		t.Errorf("url.full не вычищен (M1): %s", uf)
	}
	if env, _ := r["env"].(map[string]any); env["HTTP_X_FORWARDED_FOR"] != scrubMask {
		t.Errorf("env.HTTP_X_FORWARDED_FOR не замаскирован (F4): %v", r["env"])
	}
	if a, _ := r["auth"].(string); !contains(a, "bob:[scrubbed]@host.example") {
		t.Errorf("basic-auth пароль с '@' не вычищен целиком (F5): %s", a)
	}

	// F2: целостность JSON-тела — bigint не теряет точность, '&'/'<' не эскейпятся.
	body := s.scrubMaybeJSON(`{"id":12345678901234567890,"q":"a<b&c","password":"x"}`)
	if !contains(body, "12345678901234567890") {
		t.Errorf("bigint id потерял точность (F2): %s", body)
	}
	if contains(body, `\u0026`) || !contains(body, "a<b&c") {
		// экранированный вывод был бы a<b&c — позитивная проверка это ловит.
		t.Errorf("'&'/'<' в теле сэскейплены (F2): %s", body)
	}
	if !contains(body, "[scrubbed]") {
		t.Errorf("password в теле не замаскирован (F2): %s", body)
	}

	// F3: заголовки в форме массива пар, пришедшие СТРОКОЙ-JSON, тоже парсятся как пары.
	hdrs := s.scrubMaybeJSON(`[["Authorization","Bearer T"],["Accept","*/*"]]`)
	if contains(hdrs, "Bearer T") || !contains(hdrs, "[scrubbed]") {
		t.Errorf("headers как JSON-строка (пары) не вычищены (F3): %s", hdrs)
	}

	// M2: query-токен во встроенном в описание URL чистится ВСЕГДА — даже при
	// ScrubFreeText=false (это утечка и по умолчанию).
	off := NewScrubber(true, false, []string{"token"}) // ScrubFreeText=false
	msg := off.ScrubMessage("GET https://api.example/reset?token=SECRET&ok=1")
	if contains(msg, "SECRET") || !contains(msg, "token=[scrubbed]") || !contains(msg, "ok=1") {
		t.Errorf("query-токен в описании не вычищен при ScrubFreeText=false (M2): %s", msg)
	}
	if plain := off.ScrubMessage("just a plain error message"); plain != "just a plain error message" {
		t.Errorf("текст без URL не должен меняться (M2): %q", plain)
	}
}
