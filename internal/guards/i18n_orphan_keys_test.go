package guards

import (
	"regexp"
	"sort"
	"strings"
	"testing"
)

// Ключи разбираются регекспом по набору символов; ключ вне набора сканер
// молча не увидел бы и объявил осиротевшим.
var simpleKeyRe = regexp.MustCompile(`^[a-zA-Z0-9_.]+$`)

func TestCatalogKeysMatchScannerCharset(t *testing.T) {
	tree := Load(t)
	var bad []string
	for k := range tree.Catalogs["ru"] {
		if !simpleKeyRe.MatchString(k) {
			bad = append(bad, k)
		}
	}
	for k := range tree.Plurals["ru"] {
		if !simpleKeyRe.MatchString(k) {
			bad = append(bad, k)
		}
	}
	sort.Strings(bad)
	if len(bad) > 0 {
		t.Fatalf("ключи вне набора символов сканера (%d): %s\nсканер их не увидит — расширьте simpleKeyRe вместе с literalRe",
			len(bad), strings.Join(bad, " "))
	}
}

var (
	literalRe       = regexp.MustCompile(`"([a-zA-Z0-9_.]+)"`)
	concatPrefixRe  = regexp.MustCompile(`"([a-zA-Z0-9_.]*)"\s*\+`)
	sprintfPrefixRe = regexp.MustCompile(`Sprintf\(\s*"([a-zA-Z0-9_.]*)%`)
)

// Префикс обязан начинаться непустым сегментом до точки — иначе короткий литерал
// слева от "+" ("total" + x) амнистирует всё, что с него начинается.
func isKeyPrefix(p string) bool {
	dot := strings.IndexByte(p, '.')
	return dot > 0
}

// Тела, в которых упоминание ключа считается признаком жизни: без сгенерированных
// _templ.go (дублируют свой .templ) и без тестов (ключ, живой только собственным
// тестом, мёртв с точки зрения продукта).
func referenceBodies(tree *Tree) []string {
	var out []string
	for _, f := range tree.GoFiles {
		if f.Generated || strings.HasSuffix(f.Path, "_test.go") {
			continue
		}
		out = append(out, stripCommentsPerLine(f.Body))
	}
	for _, f := range tree.Templates {
		out = append(out, stripCommentsPerLine(f.Body))
	}
	return out
}

func collectLiteralsAndPrefixes(bodies []string) (literals, prefixes map[string]bool) {
	literals = map[string]bool{}
	prefixes = map[string]bool{}
	for _, body := range bodies {
		for _, m := range literalRe.FindAllStringSubmatch(body, -1) {
			literals[m[1]] = true
		}
		for _, re := range []*regexp.Regexp{concatPrefixRe, sprintfPrefixRe} {
			for _, m := range re.FindAllStringSubmatch(body, -1) {
				if isKeyPrefix(m[1]) {
					prefixes[m[1]] = true
				}
			}
		}
	}
	return literals, prefixes
}

func hasDynamicPrefix(key string, prefixes map[string]bool) bool {
	for p := range prefixes {
		if strings.HasPrefix(key, p) {
			return true
		}
	}
	return false
}

const (
	minLiteralReferencedKeys = 390
	minDynamicPrefixes       = 32
	// Потолок ограничивает рост неаудируемой зоны, но не ловит широкий префикс поверх
	// уже используемых ключей — те отдаются более ранним категориям раньше амнистии.
	maxPrefixAmnestiedKeys = 260
	maxOrphanExemptions    = 1
)

// Ключ в каталоге, который сканер доказать не может, но он жив: каждая запись
// обязана называть место использования.
var orphanKeyExemptions = []Exemption{
	{
		Value:   "issue.times_seen",
		Finding: "ключ каталога не используется",
		Why:     "каноническая фикстура плюрализации: на формах этого ключа стоят plural_test.go и plural_extra_test.go, проверяющие Tn/pluralForm и фоллбэк локали, а не сам ключ, — заменим на любой другой полноформный ключ без потери смысла теста; исключение снимется само, когда ключ подключат в интерфейс",
	},
}

func TestCatalogKeysAreReferenced(t *testing.T) {
	tree := Load(t)
	messageKeys, pluralKeys := collectI18nKeys(t, tree)
	literals, prefixes := collectLiteralsAndPrefixes(referenceBodies(tree))

	catalog := map[string]bool{}
	for k := range tree.Catalogs["ru"] {
		catalog[k] = true
	}
	for k := range tree.Plurals["ru"] {
		catalog[k] = true
	}

	literalHits, amnestied := 0, 0
	seen := map[string]bool{}
	exempted := ExemptedValues(orphanKeyExemptions)
	var orphans []string
	for k := range catalog {
		if messageKeys[k] || pluralKeys[k] {
			continue
		}
		if literals[k] {
			literalHits++
			continue
		}
		if hasDynamicPrefix(k, prefixes) {
			amnestied++
			continue
		}
		seen[k] = true
		if exempted[k] {
			continue
		}
		orphans = append(orphans, k)
	}

	if literalHits < minLiteralReferencedKeys {
		t.Fatalf("по литералу опознано %d ключей при пороге %d — сломан сбор упоминаний", literalHits, minLiteralReferencedKeys)
	}
	if len(prefixes) < minDynamicPrefixes {
		t.Fatalf("извлечено %d динамических префиксов при пороге %d — сломан сбор префиксов", len(prefixes), minDynamicPrefixes)
	}
	if amnestied > maxPrefixAmnestiedKeys {
		t.Fatalf("префиксами амнистировано %d ключей при потолке %d — в каталоге завелись новые ключи, не используемые кодом, и их накрыл динамический префикс; либо это мёртвые ключи и их надо убрать, либо амнистия выросла законно (новый рецепт, новый раздел) и потолок поднимают осознанно",
			amnestied, maxPrefixAmnestiedKeys)
	}

	sort.Strings(orphans)
	if len(orphans) > 0 {
		t.Errorf("ключи есть в каталоге, но не используются (%d):\n  %s", len(orphans), strings.Join(orphans, "\n  "))
	}
	CheckExemptions(t, "i18n-orphan-keys", orphanKeyExemptions, maxOrphanExemptions, seen)
}
