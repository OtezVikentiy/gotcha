package scrub

import (
	"strings"
	"testing"
	"time"
)

func FuzzScrub(f *testing.F) {
	seeds := []string{
		"",
		"plain message without url",
		"GET https://api.example/x?token=SECRET&ok=1",
		"see (https://a?token=S) and \"https://b?token=T\".",
		"multi\n\tline https://h/?password=P\r\n  tail",
		"https://user:pw@host/path?a=1#access_token=T",
		"https://api/поиск?q=привет&token=SECRET",
		`{"password":"p","nested":{"api_key":"k"},"n":12345678901234567890}`,
		`{"a":1} trailing junk`,
		`{"a":1}` + "\n" + `{"password":"p"}`,
		`[["Authorization","Bearer x"],["Accept","*/*"]]`,
		"a=1&token=2&b=3",
		"https://a/?token=x" + strings.Repeat(")", 64),
		"https://a?token=1,https://b?token=2",
		"?next=https://h/?token=SECRET",
		"chrome-extension://abc/page?access_token=T",
		"https://[::1]:8080/x?token=T",
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, in string) {
		if len(in) > 1<<20 { // выше боевого лимита события смысла нет
			t.Skip()
		}

		full := NewScrubber(true, true, []string{"password", "token", "secret", "api_key", "authorization"})
		full.ScrubFreeText = true
		start := time.Now()
		full.ScrubMessage(in)
		full.ScrubJSON(in)
		full.scrubMaybeJSON(in)
		full.scrubStringLeaf(in)
		if el := time.Since(start); el > 5*time.Second {
			t.Fatalf("обработка %d байт заняла %v — похоже на нелинейность", len(in), el)
		}

		// Входы с '@' исключены: пароль в basic-auth маскируется всегда,
		// независимо от denylist — единственный легальный мутатор при пустом конфиге.
		if !strings.Contains(in, "@") {
			noop := NewScrubber(false, false, nil)
			if got := noop.ScrubMessage(in); got != in {
				t.Fatalf("ScrubMessage исказил вход без denylist:\n in %q\nout %q", in, got)
			}
			if got := noop.scrubStringLeaf(in); got != in {
				t.Fatalf("scrubStringLeaf исказил вход без denylist:\n in %q\nout %q", in, got)
			}
		}

		// marker длиннее `in` в байтах — тогда он физически не может целиком
		// встретиться внутри фаззового `in`, каким бы ни был его контент.
		marker := "S3CRET-" + strings.Repeat("Z", len(in)+1)
		leak := NewScrubber(false, false, []string{"token"})
		for _, c := range []struct {
			name  string
			in    string
			scrub func(string) string
		}{
			{"URL в свободном тексте", "https://h/?token=" + marker, leak.ScrubMessage},
			{"URL среди фаззового текста", "GET https://h/?token=" + marker + " " + in, leak.ScrubMessage},
			{"JSON-тело", `{"token":"` + marker + `"}`, leak.ScrubJSON},
		} {
			if strings.Contains(c.scrub(c.in), marker) {
				t.Fatalf("секрет пережил скраб (%s) в %q", c.name, c.in)
			}
		}
	})
}
