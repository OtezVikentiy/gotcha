package web

import "testing"

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

func TestParseSemverBase(t *testing.T) {
	cases := []struct {
		in            string
		maj, min, pat int
		ok            bool
	}{
		{"v1.2.3", 1, 2, 3, true},
		{"1.2.3", 1, 2, 3, true},
		{"v0.20.0", 0, 20, 0, true},
		{"v1.2.3-5-gabcdef-dirty", 1, 2, 3, true},
		{"v1.2", 0, 0, 0, false},
		{"v1.2.3.4", 0, 0, 0, false},
		{"vX.Y.Z", 0, 0, 0, false}, // нечисловые символы обрывают скан до split — тоже "не три группы"
		// Скан пускает в группы только [0-9.]: пустая группа между точками — единственный способ
		// дойти до Atoi и получить ошибку там, сохранив «три части» по split.
		{"v.2.3", 0, 0, 0, false},
		{"v1..3", 0, 0, 0, false},
		{"v1.2.", 0, 0, 0, false},
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

func TestBoolFormValue(t *testing.T) {
	if got := boolFormValue(true); got != "1" {
		t.Errorf("boolFormValue(true) = %q, want \"1\"", got)
	}
	if got := boolFormValue(false); got != "0" {
		t.Errorf("boolFormValue(false) = %q, want \"0\"", got)
	}
}
