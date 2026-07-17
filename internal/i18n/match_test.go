package i18n

import "testing"

func TestParse(t *testing.T) {
	for _, code := range []string{"ru", "en"} {
		if l, ok := Parse(code); !ok || l.Code != code {
			t.Fatalf("Parse(%q) = %v,%v", code, l, ok)
		}
	}
	if _, ok := Parse("fr"); ok {
		t.Fatalf("Parse(fr) should be !ok")
	}
	if _, ok := Parse(""); ok {
		t.Fatalf("Parse(empty) should be !ok")
	}
}

func TestMatchAcceptLanguage(t *testing.T) {
	cases := map[string]string{
		"en-US,en;q=0.9": "en",
		"ru-RU,ru;q=0.9": "ru",
		"fr-FR,fr;q=0.9": "ru", // не поддержан → дефолт
		"":               "ru",
	}
	for header, want := range cases {
		if got := Match(header).Code; got != want {
			t.Fatalf("Match(%q).Code = %q, want %q", header, got, want)
		}
	}
}
