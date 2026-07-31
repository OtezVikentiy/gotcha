package guards

import (
	"regexp"
	"sort"
	"strings"
	"testing"
)

// Закрывает №122 (FE-21): светлая тема в app.css задаётся ПАРАМИ блоков —
// явный `:root[data-theme="light"]` (тема выставлена атрибутом, например из
// JS-переключателя) и системный `@media (prefers-color-scheme: light)`
// (браузер сообщает предпочтение ОС, атрибут при этом может быть не
// выставлен вовсе). Оба блока пары обязаны объявлять одно и то же — иначе
// пользователь получает разные стили в зависимости от того, ЧЕРЕЗ КАКОЙ из
// двух путей его увела в светлую тему. Синхронизируют их сейчас руками:
// следы видны в самом файле (часть строк второго блока каждой пары набрана с
// другим отступом), а старый контрастный тест читал только первый блок
// первой пары и разъезжание остальных не ловил.

// lightMediaAtRule — нормализованный (parseCSSBlocks уже прогоняет текст
// @-правила через normalizeSelector) текст @-правила системной светлой темы.
// В файле оно встречается ровно 4 раза, поэтому не разбираем произвольные
// prefers-color-scheme, а сверяемся с этим буквальным текстом: любой другой
// @media (по ширине вьюпорта, prefers-reduced-motion и т.п.) не участвует в
// паре светлой темы.
const lightMediaAtRule = "@media (prefers-color-scheme: light)"

// explicitLightPrefixRe — префикс ЯВНОЙ СВЕТЛОЙ стороны пары, ТОЛЬКО
// позитивная форма: `:root[data-theme="light"]` — «применить, когда тема
// явно выставлена в light» (токены, попап пикера дат, аватары).
//
// Раунд правок 1 (task-7-report.md): раньше здесь была ещё и альтернатива
// на негативную форму `:root:not([data-theme="light"])`, потому что обе
// формы содержат буквальную подстроку `data-theme="light"`. Это ровно тот
// класс дефекта, который весь подпроект и устраняет: подстрочное
// сопоставление приняло ОТРИЦАНИЕ за УТВЕРЖДЕНИЕ. `:not([data-theme="light"])`
// значит «тема НЕ light» — это ветка ТЁМНОЙ темы (у .btn-danger, app.css:2246,
// — компенсирующий контраст в тёмной теме, см. explicitDarkOverridePrefixRe
// ниже), а старая версия правила спаривала её со СВЕТЛОЙ системной веткой и
// закономерно находила «расхождение» между двумя противоположными темами.
// Аналог найден и в задаче 6: там маркер "tab" совпадал внутри "table",
// лечилось границей слова (\b); здесь совпадающих по смыслу тем не две
// формы одного маркера, а буквально противоположные условия — поэтому лечим
// не границей, а раздельными АНКЕРОВАННЫМИ регэкспами по полярности, без
// общей альтернативы.
var explicitLightPrefixRe = regexp.MustCompile(`^:root\[data-theme="light"\]\s*`)

// explicitDarkOverridePrefixRe — префикс ЯВНОЙ ТЁМНОЙ стороны (переопределение
// для НЕ-light, включая тему по умолчанию): `:root:not([data-theme="light"])`.
// Сама по себе НЕ входит в пару «явная светлая / системная светлая» — её
// значения ЗАВЕДОМО не обязаны совпадать со светлой системной веткой, это
// разные темы, и сравнивать их как «дубликат, который разошёлся» неверно по
// сути. Нужна только для того, чтобы отличать «системный блок легитимно не
// имеет светлой пары, потому что он ОТМЕНЯЕТ вот этот дарк-оверрайд для
// system-light-без-явного-атрибута» от «системный блок вообще осиротел» (см.
// разбор случаев в TestLightThemeBlocksAgree).
var explicitDarkOverridePrefixRe = regexp.MustCompile(`^:root:not\(\[data-theme="light"\]\)\s*`)

// mediaLightPrefixRe — системная сторона пары. В файле она всегда написана
// как `:root:not([data-theme="dark"])` — «применить, если тема явно не
// выставлена в dark» (то есть либо не выставлена вовсе, либо выставлена в
// light) — другой формы внутри lightMediaAtRule не встретилось (см.
// task-7-report.md).
var mediaLightPrefixRe = regexp.MustCompile(`^:root:not\(\[data-theme="dark"\]\)\s*`)

// lightPairKey проверяет, что selector начинается с префикса re, и
// возвращает остаток селектора после префикса — это и есть ключ, по
// которому сопоставляются явная и системная стороны одной пары (пустой ключ
// — сам корневой блок токенов, непустой — вложенный селектор вида
// ".dr-popup", ".btn-danger", ".avatar[class*=\"avatar--c\"]").
func lightPairKey(re *regexp.Regexp, selector string) (key string, ok bool) {
	if !re.MatchString(selector) {
		return "", false
	}
	return strings.TrimSpace(re.ReplaceAllString(selector, "")), true
}

// parseCSSDeclarations разбирает тело ОДНОГО блока (без вложенных фигурных
// скобок — parseCSSBlocks успел их развернуть) на набор «свойство →
// значение». Как и lastBorderColorValue в css_border_test.go, при повторении
// одного свойства внутри одного блока побеждает последнее по тексту — но
// здесь для ВСЕХ свойств блока разом, а не для одного заранее известного:
// сравнивать пары нужно по итоговому набору объявлений, а не по количеству
// строк или по тексту тела целиком (иначе разный отступ второго блока пары —
// то самое видимое следствие ручной синхронизации — читался бы как
// расхождение там, где значения на самом деле совпадают).
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

// debtLightThemeExemptions — найденные расхождения между явной СВЕТЛОЙ и
// системной СВЕТЛОЙ стороной пары. После раунда правок 1 пусто: все три
// настоящие пары (токены, попап пикера дат, аватары) сходятся полностью;
// .btn-danger больше не считается парой вовсе (её явная сторона —
// дарк-оверрайд, а не светлая, см. explicitDarkOverridePrefixRe) и не
// участвует в сравнении. Список остаётся на будущее: если правку внесут и
// одна из трёх настоящих пар разъедется, находку заводит владелец с
// присвоенным номером; app.css сама по себе эта задача не правит (подпроект
// G).
var debtLightThemeExemptions = []Exemption{}

// maxDebtLightThemeExemptions — потолок долга: 0, так как настоящих
// расхождений сейчас нет. Опускает только подпроект G, по мере появления
// (и обоснования) новых находок.
const maxDebtLightThemeExemptions = 0

// TestLightThemeBlocksAgree: явная (:root[data-theme="light"]) и системная
// (@media (prefers-color-scheme: light)) стороны каждой НАСТОЯЩЕЙ пары
// светлой темы обязаны объявлять один и тот же набор свойств с одними и
// теми же значениями. Правило также ловит структурно неполные пары в обе
// стороны — явный светлый блок без системного партнёра и системный светлый
// блок без явного партнёра НИ В КАКОМ ВИДЕ, — но не путает с ними легитимную
// конструкцию «дарк-оверрайд + его отмена системной светлой темой»
// (.btn-danger), у которой отсутствие светлой пары — не находка, а дизайн.
//
// Так было не всегда: контраст токенов светлой темы (§1.4.3/1.4.11) правили
// в прошлых волнах через ОДИН тест на первом блоке первой пары (токены) —
// остальные пары и системную сторону токенов никто не перечитывал заново при
// каждой правке цвета, полагаясь на память человека, который синхронизирует
// оба места руками. Это ровно тот класс дефектов, который устраняет весь
// подпроект: перечисление известного вместо перебора с исключениями.
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

	// Сверка на количество — защита от «позеленел по неверной причине»: если
	// кто-то переименует селектор темы или сам parseCSSBlocks сломается, эти
	// срезы опустеют (или сильно сократятся), и тест обязан упасть явно
	// здесь, а не тихо пройти по пустому множеству пар. Опираться в этом
	// случае на устаревание debtLightThemeExemptions нельзя — список сейчас
	// пуст, и CheckExemptions с пустым списком не заметит разницу между
	// «пар 3» и «пар 0». Три явных светлых блока (токены/попап/аватары),
	// четыре системных (те же три плюс .btn-danger) и один дарк-оверрайд
	// (.btn-danger) — по фактически найденному в файле (см.
	// task-7-report.md); если появятся легитимные новые, пороги остаются
	// нижней границей и не мешают.
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
			// Явный светлый блок без системного партнёра — обратная ошибка
			// из раунда правок 1: правило обязано ловить и её.
			t.Errorf("app.css:%d: %s — есть явный блок светлой темы, но нет парного системного (%s) (непарный блок, находка)",
				eb.Line, label, lightMediaAtRule)

		case !eok && mok:
			if _, isOverrideRevert := explicitDarkOverrideByKey[key]; isOverrideRevert {
				// Легитимная конструкция «дарк-оверрайд + его отмена
				// системной светлой темой» (.btn-danger): у системного блока
				// нет и не должно быть светлой пары — его партнёр это
				// explicitDarkOverrideByKey[key], а не explicitLightByKey.
				// Не находка — не сравниваем и не считаем непарным.
				continue
			}
			// Системный блок без партнёра ни в каком виде — тоже обратная
			// ошибка, которую правило обязано ловить.
			t.Errorf("app.css:%d: %s — системный блок светлой темы без пары ни в явном :root[data-theme=\"light\"], ни в дарк-оверрайде :root:not([data-theme=\"light\"]) (непарный блок, находка)",
				mb.Line, label)
		}
	}

	CheckExemptions(t, "TestLightThemeBlocksAgree (долг подпроекта G)", debtLightThemeExemptions, maxDebtLightThemeExemptions, seen)
}
