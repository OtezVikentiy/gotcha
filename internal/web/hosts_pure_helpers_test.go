package web

import "testing"

// TestNormalizeHostGroup — {"env","role"} проходят как есть, всё прочее
// (включая "" и опечатки) схлопывается в "" — незнакомое значение в query
// не должно 500-ить страницу.
func TestNormalizeHostGroup(t *testing.T) {
	cases := map[string]string{
		"env": "env", "role": "role",
		"":         "",
		"bogus":    "",
		"Env":      "", // регистрозависимо
		"env ":     "",
		"role,env": "",
	}
	for in, want := range cases {
		if got := normalizeHostGroup(in); got != want {
			t.Errorf("normalizeHostGroup(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestIsLocalBaseURL — localhost/127.0.0.1/::1 считаются локальными,
// произвольный внешний хост — нет, невалидный URL — нет (не паника).
func TestIsLocalBaseURL(t *testing.T) {
	cases := map[string]bool{
		"http://localhost:8080": true,
		"https://localhost":     true,
		"http://127.0.0.1:3000": true,
		"http://[::1]:8080":     true,
		"https://example.com":   false,
		"http://192.168.1.1":    false,
		"://not a url\x7f":      false,
		"":                      false,
	}
	for in, want := range cases {
		if got := isLocalBaseURL(in); got != want {
			t.Errorf("isLocalBaseURL(%q) = %v, want %v", in, got, want)
		}
	}
}

// TestAgentBaseURLSecure — https:// на любом хосте ИЛИ http:// на localhost
// безопасны; http:// на внешнем хосте — нет.
func TestAgentBaseURLSecure(t *testing.T) {
	cases := map[string]bool{
		"https://gotcha.example.com": true,
		"http://localhost:8080":      true,
		"http://127.0.0.1":           true,
		"http://gotcha.example.com":  false,
	}
	for in, want := range cases {
		if got := agentBaseURLSecure(in); got != want {
			t.Errorf("agentBaseURLSecure(%q) = %v, want %v", in, got, want)
		}
	}
}

// TestParseSemverBase — X.Y.Z разбирается, суффикс вида git-describe
// отбрасывается, всё, что не сводится к трём числовым группам, — ok=false.
func TestParseSemverBase(t *testing.T) {
	cases := []struct {
		in            string
		maj, min, pat int
		ok            bool
	}{
		{"v1.2.3", 1, 2, 3, true},
		{"1.2.3", 1, 2, 3, true},
		{"v0.20.0", 0, 20, 0, true},
		{"v1.2.3-5-gabcdef-dirty", 1, 2, 3, true}, // суффикс отброшен
		{"v1.2", 0, 0, 0, false},                  // не три группы
		{"v1.2.3.4", 0, 0, 0, false},              // лишняя группа
		{"vX.Y.Z", 0, 0, 0, false},                // нечисловые символы обрывают скан до split — тоже "не три группы"
		// Скан пускает в группы только [0-9.], поэтому единственный способ
		// дойти до самого Atoi и получить там ошибку (при сохранении «трёх
		// частей» по split) — пустая группа между точками.
		{"v.2.3", 0, 0, 0, false}, // пустая major-группа — Atoi major падает
		{"v1..3", 0, 0, 0, false}, // пустая minor-группа — Atoi minor падает
		{"v1.2.", 0, 0, 0, false}, // пустая patch-группа — Atoi patch падает
		{"", 0, 0, 0, false},
		{"garbage", 0, 0, 0, false},
	}
	for _, c := range cases {
		maj, min, pat, ok := parseSemverBase(c.in)
		if maj != c.maj || min != c.min || pat != c.pat || ok != c.ok {
			t.Errorf("parseSemverBase(%q) = %d,%d,%d,%v want %d,%d,%d,%v",
				c.in, maj, min, pat, ok, c.maj, c.min, c.pat, c.ok)
		}
	}
}

// TestBoolFormValue — кодирует чекбокс явными "1"/"0", не отсутствием ключа.
func TestBoolFormValue(t *testing.T) {
	if got := boolFormValue(true); got != "1" {
		t.Errorf("boolFormValue(true) = %q, want \"1\"", got)
	}
	if got := boolFormValue(false); got != "0" {
		t.Errorf("boolFormValue(false) = %q, want \"0\"", got)
	}
}
