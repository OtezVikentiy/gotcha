package ingest

import (
	"strings"
	"testing"
	"time"
)

// FuzzScrub — постоянный фаззинг-таргет скрубера. Класс дефектов, который он
// закрывает, находился адверсариальными прогонами четыре раза подряд (паника на
// срезах, квадратичный разбор хвоста, порча байтов), поэтому проверка живёт в
// репозитории, а не в чьей-то сессии: `go test -fuzz=FuzzScrub ./internal/ingest`.
// В обычном прогоне (без -fuzz) отрабатывает сид-корпус — это дёшево и защищает
// от регрессий на известных входах.
//
// Инварианты:
//  1. никогда не паникует;
//  2. без denylist и с выключенными флагами вход возвращается БАЙТ-В-БАЙТ
//     (скрубер не имеет права искажать данные, когда чистить нечего);
//  3. на входе не остаётся секрета, если его имя совпало с denylist;
//  4. обработка линейна по длине — патологический вход не вешает воркер.
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

		// (1)+(4): полный конфиг не паникует и укладывается во время.
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

		// (2): нечего чистить ⇒ вход не искажается. Входы с '@' исключены: пароль в
		// basic-auth (scheme://user:pw@host) маскируется всегда, независимо от
		// denylist — это единственный легальный мутатор при пустом конфиге.
		if !strings.Contains(in, "@") {
			noop := NewScrubber(false, false, nil)
			if got := noop.ScrubMessage(in); got != in {
				t.Fatalf("ScrubMessage исказил вход без denylist:\n in %q\nout %q", in, got)
			}
			if got := noop.scrubStringLeaf(in); got != in {
				t.Fatalf("scrubStringLeaf исказил вход без denylist:\n in %q\nout %q", in, got)
			}
		}

		// (3): секрет под denylist-именем не переживает скраб. Каждая форма
		// проверяется той функцией, которой она достаётся в проде: свободный
		// текст и URL — ScrubMessage, JSON-тело — ScrubJSON. Раньше тут стоял
		// один ассерт через && сразу по обеим функциям: он падал, только если
		// секрет протекал И там, И там, — то есть утечку ровно на одном пути
		// (единственный реальный сценарий) инвариант не ловил вовсе.
		leak := NewScrubber(false, false, []string{"token"})
		for _, c := range []struct {
			name  string
			in    string
			scrub func(string) string
		}{
			{"URL в свободном тексте", "https://h/?token=" + "S3CRET", leak.ScrubMessage},
			{"URL среди фаззового текста", "GET https://h/?token=" + "S3CRET" + " " + in, leak.ScrubMessage},
			{"JSON-тело", `{"token":"S3CRET"}`, leak.ScrubJSON},
		} {
			if strings.Contains(c.scrub(c.in), "S3CRET") {
				t.Fatalf("секрет пережил скраб (%s) в %q", c.name, c.in)
			}
		}
	})
}
