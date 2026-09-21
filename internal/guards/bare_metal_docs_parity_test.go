package guards

import (
	"regexp"
	"strings"
	"testing"
)

var (
	usageBodyRe        = regexp.MustCompile(`(?s)usage\(\) \{\n\s*cat <<'EOF'\n(.*?)\nEOF\n`)
	usageFlagRe        = regexp.MustCompile(`(?m)^  (--[a-zA-Z0-9-]+)`)
	flagsTableHeaderRe = regexp.MustCompile(`(?m)^\|\s*(?:Флаг|Flag)\s*\|\s*(?:Значение|Meaning)\s*\|\s*$`)
	flagCellTokenRe    = regexp.MustCompile("`(--[a-zA-Z0-9-]+)[^`]*`")

	pgMajorConstRe   = regexp.MustCompile(`(?m)^PG_MAJOR="(\d+)"$`)
	chVersionConstRe = regexp.MustCompile(`(?m)^CH_VERSION="([0-9.]+)"$`)

	pgPostgresqlTokenRe    = regexp.MustCompile(`\bpostgresql-?(\d+)\b`)
	pgPgsqlTokenRe         = regexp.MustCompile(`\bpgsql-(\d+)\b`)
	pgPgdgTokenRe          = regexp.MustCompile(`\bpgdg(\d+)\b`)
	chProseTokenRe         = regexp.MustCompile(`ClickHouse (\d+\.\d+)\b`)
	chAwkVarTokenRe        = regexp.MustCompile(`v=(\d+\.\d+)\.`)
	chMadisonFilterTokenRe = regexp.MustCompile(`\^([0-9.\\]+)/`)
)

func usageFlags(t *testing.T, installer string) map[string]bool {
	t.Helper()
	m := usageBodyRe.FindStringSubmatch(installer)
	if m == nil {
		t.Fatalf("install-bare-metal.sh: тело usage() не найдено — сторож смотрит мимо функции")
	}
	result := map[string]bool{}
	for _, fm := range usageFlagRe.FindAllStringSubmatch(m[1], -1) {
		result[fm[1]] = true
	}
	if len(result) == 0 {
		t.Fatalf("install-bare-metal.sh: usage() не содержит ни одного флага — сторож смотрит мимо функции")
	}
	return result
}

// Строка таблицы — тот же формат «| ячейка | ячейка |», что и у таблицы ОС;
// комбинированная строка (`--pg-dsn`/`--ch-dsn`) несёт два флага в одной ячейке.
func parseFlagsTableFlags(t *testing.T, locale, doc string) map[string]bool {
	t.Helper()
	loc := flagsTableHeaderRe.FindStringIndex(doc)
	if loc == nil {
		t.Fatalf("%s: таблица флагов (заголовок «Флаг|Flag») не найдена — сторож смотрит мимо страницы", locale)
	}
	lines := strings.Split(doc[loc[1]:], "\n")
	if len(lines) < 3 {
		t.Fatalf("%s: за заголовком таблицы флагов нет строки разделителя и данных", locale)
	}
	result := map[string]bool{}
	for _, line := range lines[2:] {
		if strings.TrimSpace(line) == "" {
			break
		}
		m := osTableRowRe.FindStringSubmatch(line)
		if m == nil {
			t.Fatalf("%s: строка таблицы флагов не разобрана: %q", locale, line)
		}
		for _, fm := range flagCellTokenRe.FindAllStringSubmatch(m[1], -1) {
			result[fm[1]] = true
		}
	}
	if len(result) == 0 {
		t.Fatalf("%s: таблица флагов не содержит ни одного флага — сторож смотрит мимо страницы", locale)
	}
	return result
}

// Список флагов в usage() и в таблице «Установки скриптом» обязаны совпасть
// целиком в обе стороны — вхождение одного в другой ослабление не поймает.
func TestBareMetalFlagsTableMatchesUsage(t *testing.T) {
	tree := Load(t)
	want := usageFlags(t, installerBody(t, tree.Root))

	for locale, path := range bareMetalDocPaths(tree.Root) {
		got := parseFlagsTableFlags(t, locale, readDocFile(t, path))
		for flag := range want {
			if !got[flag] {
				t.Errorf("%s: таблица флагов не содержит %q, а usage() его отдаёт", locale, flag)
			}
		}
		for flag := range got {
			if !want[flag] {
				t.Errorf("%s: таблица флагов заявляет %q, которого нет в usage()", locale, flag)
			}
		}
	}
}

// Только ```bash-блоки: прочая разметка (заголовки, таблицы, инлайн-код) ручного
// пути не входит в скоуп построчной сверки локалей.
func extractBashBlocks(doc string) [][]string {
	var blocks [][]string
	inFence, isBash := false, false
	var cur []string
	for _, line := range strings.Split(doc, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			if !inFence {
				inFence, isBash, cur = true, trimmed == "```bash", nil
				continue
			}
			inFence = false
			if isBash {
				blocks = append(blocks, cur)
			}
			continue
		}
		if inFence && isBash {
			cur = append(cur, line)
		}
	}
	return blocks
}

// Переводимые куски команд ручного пути — узкий явный список, а не «любое
// слово в угловых скобках»: расширять его нужно осознанно, строка за строкой.
type bashLinePlaceholder struct{ ru, en string }

var bashLinePlaceholders = []bashLinePlaceholder{
	{ru: "придумайте-свой-пароль", en: "choose-your-own-password"},
	{ru: `ClickHouse, пароль пользователя gotcha: %s\n`, en: `ClickHouse, password of the gotcha user: %s\n`},
	{ru: "<пароль-из-шага-2>", en: "<password-from-step-2>"},
	{ru: "<пароль-из-шага-3>", en: "<password-from-step-3>"},
}

func normalizeBashLine(line string, isRU bool) string {
	for i, p := range bashLinePlaceholders {
		marker := "@@" + string(rune('a'+i)) + "@@"
		if isRU {
			line = strings.ReplaceAll(line, p.ru, marker)
		} else {
			line = strings.ReplaceAll(line, p.en, marker)
		}
	}
	return line
}

func TestBareMetalManualBashBlocksMatchAcrossLocales(t *testing.T) {
	tree := Load(t)
	paths := bareMetalDocPaths(tree.Root)
	ru := extractBashBlocks(readDocFile(t, paths["ru"]))
	en := extractBashBlocks(readDocFile(t, paths["en"]))

	if len(ru) == 0 || len(en) == 0 {
		t.Fatalf("bash-блоков не найдено: ru=%d, en=%d — сторож смотрит мимо страниц", len(ru), len(en))
	}
	if len(ru) != len(en) {
		t.Fatalf("число bash-блоков разошлось: ru=%d, en=%d", len(ru), len(en))
	}
	for i := range ru {
		if len(ru[i]) != len(en[i]) {
			t.Errorf("блок %d: число строк разошлось: ru=%d, en=%d", i, len(ru[i]), len(en[i]))
			continue
		}
		for j := range ru[i] {
			if normalizeBashLine(ru[i][j], true) != normalizeBashLine(en[i][j], false) {
				t.Errorf("блок %d строка %d: ru=%q, en=%q", i, j, ru[i][j], en[i][j])
			}
		}
	}
}

func scriptVersions(t *testing.T, installer string) (pgMajor, chVersion string) {
	t.Helper()
	m := pgMajorConstRe.FindStringSubmatch(installer)
	if m == nil {
		t.Fatalf("install-bare-metal.sh: PG_MAJOR не найден — сторож смотрит мимо константы")
	}
	pgMajor = m[1]
	m = chVersionConstRe.FindStringSubmatch(installer)
	if m == nil {
		t.Fatalf("install-bare-metal.sh: CH_VERSION не найден — сторож смотрит мимо константы")
	}
	chVersion = m[1]
	return pgMajor, chVersion
}

// apt-cache madison фильтр несёт версию как regex-литерал ("^25\.3\.") —
// снимаем экранирующие бэкслеши и хвостовую точку перед сравнением.
func normalizeMadisonVersion(raw string) string {
	return strings.TrimRight(strings.ReplaceAll(raw, `\`, ""), ".")
}

// Истина — PG_MAJOR/CH_VERSION в install-bare-metal.sh; дока сверяется с
// ними, а не локали между собой. Список источников мажора — не регэксп общего
// вида, а перечисленные формы, в которых он реально встречается в тексте.
func TestBareMetalDocPackageVersionsMatchScript(t *testing.T) {
	tree := Load(t)
	installer := installerBody(t, tree.Root)
	pgMajor, chVersion := scriptVersions(t, installer)

	for locale, path := range bareMetalDocPaths(tree.Root) {
		doc := readDocFile(t, path)

		pgSeen := 0
		for _, re := range []*regexp.Regexp{pgPostgresqlTokenRe, pgPgsqlTokenRe, pgPgdgTokenRe} {
			for _, m := range re.FindAllStringSubmatch(doc, -1) {
				pgSeen++
				if m[1] != pgMajor {
					t.Errorf("%s: %q несёт мажор %s, а скрипт ставит PostgreSQL %s", locale, m[0], m[1], pgMajor)
				}
			}
		}
		if pgSeen == 0 {
			t.Errorf("%s: ни одного версионного упоминания PostgreSQL не найдено — сторож смотрит мимо страницы", locale)
		}

		chSeen := 0
		for _, m := range chProseTokenRe.FindAllStringSubmatch(doc, -1) {
			chSeen++
			if m[1] != chVersion {
				t.Errorf("%s: %q несёт версию %s, а скрипт ставит ClickHouse %s", locale, m[0], m[1], chVersion)
			}
		}
		for _, m := range chAwkVarTokenRe.FindAllStringSubmatch(doc, -1) {
			chSeen++
			if m[1] != chVersion {
				t.Errorf("%s: фильтр версии dnf list %q несёт %s, а скрипт ставит ClickHouse %s", locale, m[0], m[1], chVersion)
			}
		}
		for _, m := range chMadisonFilterTokenRe.FindAllStringSubmatch(doc, -1) {
			chSeen++
			if got := normalizeMadisonVersion(m[1]); got != chVersion {
				t.Errorf("%s: фильтр apt-cache madison %q несёт %s, а скрипт ставит ClickHouse %s", locale, m[0], got, chVersion)
			}
		}
		if chSeen == 0 {
			t.Errorf("%s: ни одного версионного упоминания ClickHouse не найдено — сторож смотрит мимо страницы", locale)
		}
	}
}
