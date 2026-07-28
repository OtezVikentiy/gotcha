package ingest

import (
	"encoding/json"
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
