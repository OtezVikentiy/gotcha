package guards

import (
	"regexp"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/web"
)

var flashCallRe = regexp.MustCompile(`\.(flashOK(?:Pair)?|flashWarn)\(\s*[^,]+?\s*,\s*"([^"]+)"`)

const minFlashCallsFound = 9

var flashKeysBlockRe = regexp.MustCompile(`(?s)var flashKeys = map\[string\]bool\{(.*?)\n\}`)
var flashKeyEntryRe = regexp.MustCompile(`"([^"]+)":\s*true`)

// N==0 (повторное действие) заставляет flashView звать i18n.T с каталога
// messages, а не Tn с plurals — перевода только в plurals для него мало.
func TestFlashCallKeysAreWhitelistedAndTranslated(t *testing.T) {
	tree := Load(t)
	whitelist := flashWhitelist(t, tree)

	type call struct {
		path string
		line int
		key  string
	}
	var calls []call
	for _, f := range tree.GoFiles {
		// _test.go содержит намеренные вызовы с несуществующим ключом (проверка
		// поведения на дыре в белом списке), internal/guards/* — саму эту регулярку.
		if f.Generated || strings.HasSuffix(f.Path, "_test.go") || strings.HasPrefix(f.Path, "internal/guards/") {
			continue
		}
		for i, line := range strings.Split(f.Body, "\n") {
			line = stripTrailingComment(line)
			for _, m := range flashCallRe.FindAllStringSubmatch(line, -1) {
				calls = append(calls, call{path: f.Path, line: i + 1, key: m[2]})
			}
		}
	}

	if len(calls) < minFlashCallsFound {
		t.Fatalf("сканер нашёл %d вызовов flashOK/flashWarn с литеральным ключом, ожидалось не меньше %d — это регрессия самого сканера, а не кода",
			len(calls), minFlashCallsFound)
	}

	for _, c := range calls {
		checkFlashKey(t, tree, whitelist, c.path, c.line, c.key)
	}

	// Пустой срез — сигнал, что сборка среза в issues.go сломана, а не что
	// карту нечего проверять.
	if len(web.BulkActionFlashKeys) == 0 {
		t.Fatal("web.BulkActionFlashKeys пуст — сборка среза в issues.go сломана, а не карта bulkActionFlashKey опустела по замыслу")
	}
	for _, key := range web.BulkActionFlashKeys {
		checkFlashKey(t, tree, whitelist, "internal/web/issues.go (bulkActionFlashKey)", 0, key)
	}
}

func checkFlashKey(t *testing.T, tree *Tree, whitelist map[string]bool, path string, line int, key string) {
	t.Helper()
	if !whitelist[key] {
		if line > 0 {
			t.Errorf("%s:%d: flashOK/flashWarn зовётся с ключом %q, которого нет в белом списке flashKeys (internal/web/flash.go)",
				path, line, key)
		} else {
			t.Errorf("%s: значение %q не входит в белый список flashKeys (internal/web/flash.go)", path, key)
		}
		return
	}
	for _, lang := range []string{"ru", "en"} {
		if _, ok := tree.Catalogs[lang][key]; ok {
			continue
		}
		if line > 0 {
			t.Errorf("%s:%d: [%s] ключ %q есть в flashKeys, но перевода нет в messages каталога — на N==0 (повторное действие) flashView зовёт i18n.T и покажет сырой ключ",
				path, line, lang, key)
		} else {
			t.Errorf("%s: [%s] ключ %q есть в flashKeys, но перевода нет в messages каталога — на N==0 (повторное действие) flashView зовёт i18n.T и покажет сырой ключ",
				path, lang, key)
		}
	}
}

func flashWhitelist(t *testing.T, tree *Tree) map[string]bool {
	t.Helper()
	for _, f := range tree.GoFiles {
		if f.Path != "internal/web/flash.go" {
			continue
		}
		m := flashKeysBlockRe.FindStringSubmatch(f.Body)
		if m == nil {
			t.Fatal("не нашли `var flashKeys = map[string]bool{...}` в internal/web/flash.go — разбор сломан или список переименован/переструктурирован")
		}
		out := map[string]bool{}
		for _, e := range flashKeyEntryRe.FindAllStringSubmatch(m[1], -1) {
			out[e[1]] = true
		}
		if len(out) == 0 {
			t.Fatal("flashKeys разобран пустым — регулярка flashKeyEntryRe сломана")
		}
		return out
	}
	t.Fatal("internal/web/flash.go не найден в дереве")
	return nil
}
