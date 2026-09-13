package scrub

import (
	"encoding/json"
	"strings"
	"testing"
)

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

	s.ScrubUser(nil, nil)
}

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

	bad := `{not json`
	if got := s.ScrubJSON(bad); got != bad {
		t.Fatalf("невалидный JSON должен вернуться как есть: %q", got)
	}
}

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
	if got := s.ScrubText("error for user@example.com"); got != "error for user@example.com" {
		t.Fatalf("nil-Scrubber.ScrubText должен вернуть вход как есть: %q", got)
	}
}

func TestScrubTextDisabled(t *testing.T) {
	s := NewScrubber(false, false, nil)
	in := "error for user@example.com"
	if got := s.ScrubText(in); got != in {
		t.Fatalf("при выключенном флаге текст не должен меняться: %q", got)
	}
}

func TestScrubTextEnabled(t *testing.T) {
	s := NewScrubber(false, false, nil)
	s.ScrubFreeText = true

	cases := []struct{ in, want string }{
		{"error for user@example.com", "error for [email]"},
		{"contact bob.smith+tag@sub.example.co.uk now", "contact [email] now"},
		{"a@b.com and c@d.org", "[email] and [email]"},
		{"no email here", "no email here"},
		{"", ""},
		{"card 4111 1111 1111 1111", "card 4111 1111 1111 1111"},
	}
	for _, c := range cases {
		if got := s.ScrubText(c.in); got != c.want {
			t.Errorf("ScrubText(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

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

func TestScrubRequestReAuditFixes(t *testing.T) {
	s := NewScrubber(true /*ScrubIP*/, false, []string{"password", "token", "access_token", "authorization"})
	s.ScrubFreeText = true

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

	body := s.scrubMaybeJSON(`{"id":12345678901234567890,"q":"a<b&c","password":"x"}`)
	if !contains(body, "12345678901234567890") {
		t.Errorf("bigint id потерял точность (F2): %s", body)
	}
	if contains(body, `\u0026`) || !contains(body, "a<b&c") {
		t.Errorf("'&'/'<' в теле сэскейплены (F2): %s", body)
	}
	if !contains(body, "[scrubbed]") {
		t.Errorf("password в теле не замаскирован (F2): %s", body)
	}

	hdrs := s.scrubMaybeJSON(`[["Authorization","Bearer T"],["Accept","*/*"]]`)
	if contains(hdrs, "Bearer T") || !contains(hdrs, "[scrubbed]") {
		t.Errorf("headers как JSON-строка (пары) не вычищены (F3): %s", hdrs)
	}

	off := NewScrubber(true, false, []string{"token"})
	msg := off.ScrubMessage("GET https://api.example/reset?token=SECRET&ok=1")
	if contains(msg, "SECRET") || !contains(msg, "token=[scrubbed]") || !contains(msg, "ok=1") {
		t.Errorf("query-токен в описании не вычищен при ScrubFreeText=false (M2): %s", msg)
	}
	if plain := off.ScrubMessage("just a plain error message"); plain != "just a plain error message" {
		t.Errorf("текст без URL не должен меняться (M2): %q", plain)
	}
}

func TestScrubReAuditRound2(t *testing.T) {
	s := NewScrubber(true /*IP*/, true /*email*/, []string{"password", "token", "access_token"})

	body := s.ScrubJSON(`{"id":12345678901234567890,"q":"a<b&c","password":"x"}`)
	if !contains(body, "12345678901234567890") {
		t.Errorf("ScrubJSON потерял точность bigint (P2-5): %s", body)
	}
	if contains(body, "&amp;") || !contains(body, "a<b&c") {
		t.Errorf("ScrubJSON сэскейпил &<> (P2-5): %s", body)
	}
	if !contains(body, "[scrubbed]") {
		t.Errorf("ScrubJSON не замаскировал password: %s", body)
	}

	pat := s.scrubURLParams("https://ghp_SECRETTOKEN@github.com/o/r.git")
	if contains(pat, "ghp_SECRETTOKEN") || !contains(pat, "[scrubbed]@github.com") {
		t.Errorf("одиночный userinfo/PAT не замаскирован (P1-2): %s", pat)
	}

	dat := s.ScrubJSON(`{"data":"https://u:pw@app/cb?state=xyz#access_token=SECRET"}`)
	if contains(dat, "SECRET") || contains(dat, ":pw@") || !contains(dat, "access_token=[scrubbed]") {
		t.Errorf("URL под data не вычищен полностью (P1-1): %s", dat)
	}

	crumb := s.ScrubJSON(`[{"type":"http","message":"GET https://api/x?token=SEC1","data":{"url":"https://api/x?token=SEC2"}}]`)
	if contains(crumb, "SEC1") || contains(crumb, "SEC2") {
		t.Errorf("токен во встроенном URL breadcrumb не вычищен (P1-2): %s", crumb)
	}

	for _, in := range []string{
		`fetch failed for "https://api/x?token=SECRET"`,
		`see (https://api/x?token=SECRET) now`,
		`req url=https://api/x?token=SECRET`,
	} {
		if out := s.ScrubMessage(in); contains(out, "SECRET") {
			t.Errorf("обрамлённый URL не вычищен (P1-3): %q → %q", in, out)
		}
	}

	multi := s.ScrubMessage("could not connect\n  DSN https://u:pw@db/app?token=SECRET\n  hint: firewall")
	if contains(multi, "SECRET") || contains(multi, ":pw@") {
		t.Errorf("многострочный секрет не вычищен (P2-1): %q", multi)
	}
	if strings.Count(multi, "\n") != 2 {
		t.Errorf("переводы строк схлопнуты (P2-1): %q", multi)
	}

	safe := s.ScrubJSON(`{"note":"https://blog/p?page=2&tokenizer=bpe&password=x"}`)
	if !contains(safe, "password=[scrubbed]") {
		t.Errorf("password должен маскироваться: %s", safe)
	}
	if contains(safe, "tokenizer=bpe") {
		t.Errorf("fail-closed: tokenizer (⊃token) маскируется без allowlist: %s", safe)
	}
	if !contains(safe, "page=2") {
		t.Errorf("имя без denylist-подстроки не трогаем: %s", safe)
	}
	allowed := NewScrubber(true, true, []string{"password", "token", "access_token"})
	allowed.ScrubFreeText = true
	allowed.SetAllowKeys([]string{"tokenizer"})
	back := allowed.ScrubJSON(`{"note":"https://blog/p?tokenizer=bpe&password=x"}`)
	if !contains(back, "tokenizer=bpe") {
		t.Errorf("allowlist должен вернуть tokenizer: %s", back)
	}
	if !contains(back, "password=[scrubbed]") {
		t.Errorf("allowlist не должен снимать маску с password: %s", back)
	}

	ndjson := s.ScrubJSON(`{"body":"{\"a\":1}\n{\"second\":2}"}`)
	if !contains(ndjson, "second") {
		t.Errorf("хвост JSON усечён (P2-3): %s", ndjson)
	}

	ips := s.ScrubJSON(`{"headers":{"True-Client-IP":"1.2.3.4","CF-Connecting-IP":"1.2.3.4","X-Forwarded-Proto":"https"}}`)
	if contains(ips, "1.2.3.4") {
		t.Errorf("forwarding-IP не замаскирован (P2-4): %s", ips)
	}
	if !contains(ips, `"https"`) {
		t.Errorf("X-Forwarded-Proto ошибочно замаскирован (P2-4): %s", ips)
	}

	same := "visit https://example.com/docs?page=2 for help"
	if out := s.ScrubMessage(same); out != same {
		t.Errorf("текст без секретов изменён: %q → %q", same, out)
	}

	for _, in := range []string{"this is not a url but contains :// inside", "//host/path"} {
		if out := s.ScrubMessage(in); out != in {
			t.Errorf("не-URL текст изменён: %q → %q", in, out)
		}
	}
}

func TestScrubReAuditRound3(t *testing.T) {
	s := NewScrubber(true, true, []string{"password", "token", "secret", "auth", "session", "cookie", "api_key"})

	comp := s.ScrubJSON(`{"data":"client_secret=CS&id_token=JWT&new_password=P&keep_me=1"}`)
	for _, leak := range []string{"client_secret=CS", "id_token=JWT", "new_password=P"} {
		if contains(comp, leak) {
			t.Errorf("составной секрет не замаскирован (P1-1): %q в %s", leak, comp)
		}
	}
	if !contains(comp, "keep_me=1") {
		t.Errorf("параметр без denylist-подстроки не должен маскироваться: %s", comp)
	}

	tail := s.ScrubMessage(`payload={"url":"https://a?token=X","next":"KEEP","id":7}`)
	if contains(tail, "token=X") || !contains(tail, `"next":"KEEP"`) || !contains(tail, `"id":7`) {
		t.Errorf("хвост после URL потерян/секрет не вычищен (P1-2): %s", tail)
	}

	dat := s.ScrubJSON(`{"data":"GET https://api/x?token=SECRET"}`)
	if contains(dat, "SECRET") {
		t.Errorf("встроенный URL под data не вычищен (P1-3): %s", dat)
	}

	nd := s.ScrubJSON(`{"body":"{\"password\":\"SEC_A\"}\n{\"password\":\"SEC_B\"}"}`)
	if contains(nd, "SEC_A") || contains(nd, "SEC_B") {
		t.Errorf("NDJSON: секрет утёк (P2-1): %s", nd)
	}

	if _, ok := decodeJSONValue(`{"a":1}}`); ok {
		t.Error("decodeJSONValue должен отвергать хвост '}' (P2-2)")
	}
	if _, ok := decodeJSONValue(`[1,2]}`); ok {
		t.Error("decodeJSONValue должен отвергать хвост ']' (P2-2)")
	}

	tags := map[string]string{"url": "https://h/p?token=SECRET", "server_name": "https://u:pw@h/"}
	s.ScrubTags(tags)
	if contains(tags["url"], "SECRET") || contains(tags["server_name"], ":pw@") {
		t.Errorf("ScrubTags не почистил значения (P2-6): %v", tags)
	}

	comma := s.ScrubMessage(`https://a/x?keep=1,https://b/y?token=SECRET`)
	if contains(comma, "token=SECRET") || !contains(comma, "keep=1") {
		t.Errorf("URL через запятую обработаны неверно: %s", comma)
	}

	framed := s.ScrubMessage("see (https://a?token=S)\n\tand \"https://b?token=T\".")
	want := "see (https://a?token=[scrubbed])\n\tand \"https://b?token=[scrubbed]\"."
	if framed != want {
		t.Errorf("обрамление/переносы не сохранены:\n got %q\nwant %q", framed, want)
	}

	idem := s.ScrubMessage("GET https://h/?token=[Filtered]")
	if contains(idem, "[scrubbed]]") {
		t.Errorf("не-идемпотентно, вырос ']' (P2-3): %s", idem)
	}
}

func TestScrubReAuditRound4(t *testing.T) {
	s := NewScrubber(true, true, []string{"password", "token", "secret", "auth", "session", "api_key", "credit_card", "card_number", "access_token"})

	comp := s.ScrubJSON(`{"data":"x_api_key=SEEKRET&X-Api-Key=K2&billing_credit_card=4111&cc_card_number=4222&page=2"}`)
	for _, leak := range []string{"SEEKRET", "K2", "4111", "4222"} {
		if contains(comp, leak) {
			t.Errorf("составной ключ не замаскирован в параметре (P1-A): %q в %s", leak, comp)
		}
	}
	if !contains(comp, "page=2") {
		t.Errorf("параметр без denylist-подстроки не должен маскироваться: %s", comp)
	}

	for _, in := range []string{
		"GET https://api/x?q=привет&token=SECRET",
		"GET https://api/поиск?token=SECRET",
		"GET https://пример.рф/?token=SECRET",
	} {
		if out := s.ScrubMessage(in); contains(out, "SECRET") {
			t.Errorf("UTF-8 оборвал разбор URL, токен утёк (P1): %q → %q", in, out)
		}
	}

	huge := "https://a/?token=x" + strings.Repeat(")", 100000)
	out := s.ScrubMessage(huge)
	if contains(out, "token=x") {
		t.Error("DoS-триммер: токен не вычищен (P1-B)")
	}

	for _, in := range []string{
		`{"data":"{\"password\":\"P4SS\"} trailing"}`,
		`{"data":"{\"password\":\"P4SS\"}}"}`,
	} {
		if got := s.ScrubJSON(in); contains(got, "P4SS") {
			t.Errorf("JSON с хвостом: секрет утёк (P1-C): %q → %q", in, got)
		}
	}

	for _, name := range []string{
		"IDToken", "apiKey", "APIKEY", "client_secret", "clientsecret",
		"X-Api-Key", "mytoken", "sessionToken", "user.password", "новый_пароль",
	} {
		probe := NewScrubber(false, false, []string{"password", "token", "secret", "api_key", "пароль"})
		if !probe.denied(name) {
			t.Errorf("denied(%q) = false, ожидается маскирование (fail-closed)", name)
		}
	}
	for _, name := range []string{"page", "limit", "user_id", "endpoint"} {
		probe := NewScrubber(false, false, []string{"password", "token", "secret", "api_key"})
		if probe.denied(name) {
			t.Errorf("denied(%q) = true, имя без denylist-подстроки маскировать не нужно", name)
		}
	}

	ru := NewScrubber(false, false, []string{"пароль"})
	if got := ru.scrubMaybeJSON("новый_пароль=SECRET&x=1"); contains(got, "SECRET") {
		t.Errorf("кириллический денилист не сработал в параметре: %s", got)
	}
}

func TestScrubNormalizesEmailAndIPKeys(t *testing.T) {
	s := NewScrubber(true, true, nil)

	masked := []string{
		"user.email", "user_email", "userEmail", "USER_EMAIL",
		"enduser.email", "sentry.user.email", "email", "e-mail", "E_Mail",
		"customer_email",
		"user.ip", "user_ip", "ip_address", "ipAddress", "IP-ADDRESS",
		"sentry.user.ip_address", "client.address", "client_address",
		"net.peer.ip", "net.sock.peer.addr", "http.client_ip",
		"X-Forwarded-For", "HTTP_X_FORWARDED_FOR", "X-Real-IP",
		"X-Cluster-Client-IP", "CF-Connecting-IP", "REMOTE_ADDR",
		"Forwarded", "forwarded",
		"network.peer.address", "network.local.address", "peer.address",
	}
	for _, k := range masked {
		if !s.denied(k) {
			t.Errorf("ключ %q НЕ маскируется — ПДн уедет в теги/атрибуты", k)
		}
	}

	kept := []string{
		"user_id", "user.id", "enduser.id", "userId",
		"X-Forwarded-Proto", "X-Forwarded-Host", "X-Forwarded-Port",
		"description", "zip_code", "recipient",
	}
	for _, k := range kept {
		if s.denied(k) {
			t.Errorf("ключ %q замаскирован напрасно", k)
		}
	}
}

func TestScrubEmailIPKeysRespectAllowList(t *testing.T) {
	s := NewScrubber(true, true, nil)
	s.SetAllowKeys([]string{"email_provider"})

	if s.denied("email_provider") {
		t.Error("allowlist не сработал для email_provider")
	}
	if !s.denied("user_email") {
		t.Error("allowlist не должен снимать маскировку с user_email")
	}
}

func TestScrubFreeTextMatchesUnicodeEmail(t *testing.T) {
	s := NewScrubber(false, false, nil)
	s.ScrubFreeText = true

	cases := []string{
		"иван@пример.рф",
		"пользователь@почта.москва",
		"ivan@example.com",
		"Ivan.Petrov+tag@sub.example.co.uk",
	}
	for _, addr := range cases {
		in := "ошибка у пользователя " + addr + " при входе"
		got := s.ScrubMessage(in)
		if strings.Contains(got, addr) {
			t.Errorf("email %q пережил маскирование: %q", addr, got)
		}
		if !strings.Contains(got, emailTextMask) {
			t.Errorf("маска не проставлена для %q: %q", addr, got)
		}
	}

	keep := []string{"5 @ 10 рублей", "массив[@index]", "user @ host"}
	for _, in := range keep {
		if got := s.ScrubMessage(in); got != in {
			t.Errorf("текст %q изменён напрасно: %q", in, got)
		}
	}
}

func TestDefaultDenyKeysReturnsCopy(t *testing.T) {
	keys := DefaultDenyKeys()
	if len(keys) == 0 {
		t.Fatal("DefaultDenyKeys вернул пустой список")
	}
	want := "password"
	found := false
	for _, k := range keys {
		if k == want {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("DefaultDenyKeys не содержит %q: %v", want, keys)
	}

	keys[0] = "испорчено"
	again := DefaultDenyKeys()
	if again[0] == "испорчено" {
		t.Error("DefaultDenyKeys не защищает пакетный список от изменений через срез")
	}
}

func TestDefaultDenyKeysScrubsRealPayload(t *testing.T) {
	s := NewScrubber(true, true, DefaultDenyKeys())
	m := map[string]any{
		"pwd":    "hunter2",
		"auth":   "Bearer abc",
		"token":  "xyz",
		"method": "GET",
	}
	s.ScrubData(m)
	for _, key := range []string{"pwd", "auth", "token"} {
		if m[key] != scrubMask {
			t.Errorf("%s не отредактирован дефолтным денилистом: %v", key, m[key])
		}
	}
	if m["method"] != "GET" {
		t.Errorf("безобидное поле method задето: %v", m["method"])
	}
}
