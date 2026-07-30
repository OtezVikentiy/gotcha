package web

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
)

// Ключи, собираемые конкатенацией, страж каталога поймать не может: множество
// их значений знает только вызывающий. Здесь это множество перечислено явно —
// новый уровень issue, статус пробы или пресет окна теперь не доедет до
// страницы сырым ключом.
//
// Проверяются ОБА языка: ключ, забытый только в одном каталоге, ловится
// паритетом, но ключ, забытый в обоих, — только этим тестом.
func TestDynamicKeysResolve(t *testing.T) {
	groups := map[string][]string{
		"issues.level.":     {"debug", "info", "warning", "error", "fatal"},
		"issues.status.":    {"unresolved", "resolved", "ignored"},
		"probe.status.":     {"online", "offline", "revoked"},
		"range.":            {"all", "1h", "24h", "7d", "30d", "custom"},
		"org.quota.kind.":   {"events.short", "transactions.short", "metrics.short", "profiles.short"},
		"uptime.consensus.": {string(uptime.ConsensusAny), string(uptime.ConsensusMajority), string(uptime.ConsensusAll)},
	}
	for _, lang := range []string{"ru", "en"} {
		ctx := i18n.WithLocale(context.Background(), i18n.Locale{Code: lang})
		for prefix, values := range groups {
			for _, v := range values {
				key := prefix + v
				if got := i18n.T(ctx, key); got == key {
					t.Errorf("[%s] ключ %q собирается в коде, но перевода нет — на странице будет сырой ключ", lang, key)
				}
			}
		}
	}
}

// TestHelpPanelKeysResolve — панель «Что это за раздел?» собирает два ключа на
// область. Раздел без перевода показывал бы «help.teams.title» заголовком.
func TestHelpPanelKeysResolve(t *testing.T) {
	areas := helpAreasInTemplates(t)
	if len(areas) < 10 {
		t.Fatalf("найдено %d областей помощи — сканер сломан", len(areas))
	}
	for _, lang := range []string{"ru", "en"} {
		ctx := i18n.WithLocale(context.Background(), i18n.Locale{Code: lang})
		for _, area := range areas {
			for _, suffix := range []string{".title", ".body"} {
				key := "help." + area + suffix
				if got := i18n.T(ctx, key); got == key {
					t.Errorf("[%s] панель помощи раздела %q без ключа %q", lang, area, key)
				}
			}
		}
	}
}

// helpAreasInTemplates собирает области, для которых шаблоны просят панель
// помощи: список обязан приходить из кода, иначе тест проверяет вчерашний набор.
func helpAreasInTemplates(t *testing.T) []string {
	t.Helper()
	seen := map[string]bool{}
	for _, path := range templFiles(t) {
		data := readFileString(t, path)
		const marker = `helpPanel("`
		for i := 0; ; {
			j := strings.Index(data[i:], marker)
			if j < 0 {
				break
			}
			start := i + j + len(marker)
			end := strings.Index(data[start:], `"`)
			if end < 0 {
				break
			}
			seen[data[start:start+end]] = true
			i = start + end
		}
	}
	out := make([]string, 0, len(seen))
	for a := range seen {
		out = append(out, a)
	}
	return out
}

// TestMonitorErrorCodesResolve — коды отказа валидации монитора попадают в ключ
// «error.monitor.<code>». Код без перевода показал бы пользователю сырой ключ
// вместо объяснения, что чинить.
func TestMonitorErrorCodesResolve(t *testing.T) {
	codes := monitorErrorCodes(t)
	if len(codes) < 20 {
		t.Fatalf("найдено %d кодов — сканер сломан", len(codes))
	}
	for _, lang := range []string{"ru", "en"} {
		ctx := i18n.WithLocale(context.Background(), i18n.Locale{Code: lang})
		for _, code := range codes {
			key := "error.monitor." + code
			if got := i18n.T(ctx, key); got == key {
				t.Errorf("[%s] код валидации %q без сообщения (%s)", lang, code, key)
			}
		}
	}
}

// templFiles — шаблоны продукта (без сгенерированных .go).
func templFiles(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir("templates")
	if err != nil {
		t.Fatalf("read templates dir: %v", err)
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".templ") {
			out = append(out, filepath.Join("templates", e.Name()))
		}
	}
	return out
}

func readFileString(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

// monitorErrorCodes собирает коды из вызовов invalid(...) в пакете uptime:
// список обязан приходить из кода, иначе тест закрепляет вчерашний набор.
func monitorErrorCodes(t *testing.T) []string {
	t.Helper()
	seen := map[string]bool{}
	err := filepath.WalkDir("../uptime", func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".go") || strings.HasSuffix(d.Name(), "_test.go") {
			return nil
		}
		for _, m := range invalidCallRe.FindAllStringSubmatch(readFileString(t, path), -1) {
			seen[m[1]] = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("обход internal/uptime: %v", err)
	}
	out := make([]string, 0, len(seen))
	for c := range seen {
		out = append(out, c)
	}
	return out
}

// invalidCallRe — invalid("field", "code", ...): второй аргумент и есть код.
var invalidCallRe = regexp.MustCompile(`invalid\("[^"]*",\s*"([^"]+)"`)
