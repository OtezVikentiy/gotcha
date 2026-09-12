package guards

import (
	"regexp"
	"sort"
	"strings"
	"testing"
)

// Сверяется буквальным текстом — в файле встречается только эта форма @media.
const lightMediaAtRule = "@media (prefers-color-scheme: light)"

// Только позитивная форма: :not([data-theme="light"]) значит «тема НЕ light» —
// это ветка тёмной темы, спаривать её со светлой системной веткой нельзя.
var explicitLightPrefixRe = regexp.MustCompile(`^:root\[data-theme="light"\]\s*`)

// Не входит в пару «явная светлая / системная светлая» — её значения не
// обязаны совпадать со светлой; нужна только отличить легитимную отмену от сироты.
var explicitDarkOverridePrefixRe = regexp.MustCompile(`^:root:not\(\[data-theme="light"\]\)\s*`)

// Системная сторона в файле всегда :root:not([data-theme="dark"]) — другой формы нет.
var mediaLightPrefixRe = regexp.MustCompile(`^:root:not\(\[data-theme="dark"\]\)\s*`)

// Возвращает остаток селектора после префикса — пустой ключ значит корневой
// блок токенов, непустой — вложенный селектор.
func lightPairKey(re *regexp.Regexp, selector string) (key string, ok bool) {
	if !re.MatchString(selector) {
		return "", false
	}
	return strings.TrimSpace(re.ReplaceAllString(selector, "")), true
}

// Последнее объявление в блоке побеждает при повторении свойства — сравниваются
// итоговые наборы деклараций, не текст тела (иначе разный отступ читался бы как расхождение).
func parseCSSDeclarations(body string) map[string]string {
	out := map[string]string{}
	for _, stmt := range strings.Split(body, ";") {
		i := strings.IndexByte(stmt, ':')
		if i < 0 {
			continue
		}
		name := strings.TrimSpace(stmt[:i])
		if name == "" {
			continue
		}
		out[name] = strings.TrimSpace(stmt[i+1:])
	}
	return out
}

// Пусто: настоящие пары сходятся полностью. Остаётся на случай будущего расхождения.
var debtLightThemeExemptions = []Exemption{}

const maxDebtLightThemeExemptions = 0

// Явная и системная стороны каждой пары обязаны совпадать; исключение —
// дарк-оверрайд (.btn-danger), у которого отсутствие светлой пары не находка, а дизайн.
func TestLightThemeBlocksAgree(t *testing.T) {
	tree := Load(t)
	blocks := parseCSSBlocks(tree.CSS.Body)

	type side struct {
		key   string
		block cssBlock
	}
	var explicitLightSides, explicitDarkOverrideSides, mediaSides []side

	for _, b := range blocks {
		if b.AtRule == "" {
			if key, ok := lightPairKey(explicitLightPrefixRe, b.Selector); ok {
				explicitLightSides = append(explicitLightSides, side{key, b})
				continue
			}
			if key, ok := lightPairKey(explicitDarkOverridePrefixRe, b.Selector); ok {
				explicitDarkOverrideSides = append(explicitDarkOverrideSides, side{key, b})
			}
			continue
		}
		if b.AtRule == lightMediaAtRule {
			if key, ok := lightPairKey(mediaLightPrefixRe, b.Selector); ok {
				mediaSides = append(mediaSides, side{key, b})
			}
		}
	}

	// Нижние пороги — иначе сломанный разбор дал бы пустые срезы молча:
	// пустой debtLightThemeExemptions не отличит «пар 3» от «пар 0».
	if len(explicitLightSides) < 3 {
		t.Fatalf("найдено %d явных блоков светлой темы (:root[data-theme=\"light\"]), ожидалось не меньше 3 — разбор сломан или структура app.css изменилась сильнее, чем правило умеет понять", len(explicitLightSides))
	}
	if len(mediaSides) < 4 {
		t.Fatalf("найдено %d системных блоков светлой темы (внутри %s), ожидалось не меньше 4 — разбор сломан или структура app.css изменилась сильнее, чем правило умеет понять", len(mediaSides), lightMediaAtRule)
	}
	if len(explicitDarkOverrideSides) < 1 {
		t.Fatalf("найдено %d явных дарк-оверрайдов (:root:not([data-theme=\"light\"])), ожидался минимум 1 (.btn-danger) — разбор сломан или структура app.css изменилась сильнее, чем правило умеет понять", len(explicitDarkOverrideSides))
	}

	explicitLightByKey := map[string]cssBlock{}
	for _, s := range explicitLightSides {
		explicitLightByKey[s.key] = s.block
	}
	explicitDarkOverrideByKey := map[string]cssBlock{}
	for _, s := range explicitDarkOverrideSides {
		explicitDarkOverrideByKey[s.key] = s.block
	}
	mediaByKey := map[string]cssBlock{}
	for _, s := range mediaSides {
		mediaByKey[s.key] = s.block
	}

	allKeys := map[string]bool{}
	for k := range explicitLightByKey {
		allKeys[k] = true
	}
	for k := range mediaByKey {
		allKeys[k] = true
	}
	keys := make([]string, 0, len(allKeys))
	for k := range allKeys {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	exempt := ExemptedValues(debtLightThemeExemptions)
	seen := map[string]bool{}

	for _, key := range keys {
		label := key
		if label == "" {
			label = "(корневые токены :root)"
		}

		eb, eok := explicitLightByKey[key]
		mb, mok := mediaByKey[key]

		switch {
		case eok && mok:
			// Настоящая пара — сравниваем разобранные объявления.
			eDecl := parseCSSDeclarations(eb.Body)
			mDecl := parseCSSDeclarations(mb.Body)

			props := map[string]bool{}
			for p := range eDecl {
				props[p] = true
			}
			for p := range mDecl {
				props[p] = true
			}
			propNames := make([]string, 0, len(props))
			for p := range props {
				propNames = append(propNames, p)
			}
			sort.Strings(propNames)

			for _, prop := range propNames {
				ev, eHas := eDecl[prop]
				mv, mHas := mDecl[prop]
				if eHas && mHas && ev == mv {
					continue
				}

				value := label + " " + prop
				seen[value] = true
				if exempt[value] {
					continue
				}

				evDisplay, mvDisplay := ev, mv
				if !eHas {
					evDisplay = "(свойство отсутствует)"
				}
				if !mHas {
					mvDisplay = "(свойство отсутствует)"
				}
				t.Errorf("app.css:%d/%d: пара %s расходится по %q — явная сторона: %s, системная сторона: %s",
					eb.Line, mb.Line, label, prop, evDisplay, mvDisplay)
			}

		case eok && !mok:
			// Явный светлый блок без системного партнёра — тоже находка.
			t.Errorf("app.css:%d: %s — есть явный блок светлой темы, но нет парного системного (%s) (непарный блок, находка)",
				eb.Line, label, lightMediaAtRule)

		case !eok && mok:
			if _, isOverrideRevert := explicitDarkOverrideByKey[key]; isOverrideRevert {
				// Легитимная отмена дарк-оверрайда системной светлой темой (.btn-danger) —
				// у системного блока законно нет светлой пары.
				continue
			}
			// Системный блок без партнёра ни в каком виде — тоже находка.
			t.Errorf("app.css:%d: %s — системный блок светлой темы без пары ни в явном :root[data-theme=\"light\"], ни в дарк-оверрайде :root:not([data-theme=\"light\"]) (непарный блок, находка)",
				mb.Line, label)
		}
	}

	CheckExemptions(t, "TestLightThemeBlocksAgree (долг подпроекта G)", debtLightThemeExemptions, maxDebtLightThemeExemptions, seen)
}
