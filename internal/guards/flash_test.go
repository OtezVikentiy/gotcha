package guards

import (
	"regexp"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/web"
)

// flashCallRe находит вызовы flashOK/flashOKPair/flashWarn с ЛИТЕРАЛЬНЫМ
// ключом вторым аргументом: h.flashOK(w, "flash.saved", 0). flashOKPair
// (парный флеш с двумя счётчиками, B6) включён явно — фикс-раунд 1 ревью T4:
// регулярка без него молча пропускала h.flashOKPair(w, "flash.recipes_applied",
// ...), и выпавший из каталога перевод доехал бы до прода сырым ключом.
// Первый аргумент не требуется
// быть ровно "w" — [^,]+? допускает любое выражение получателя (в тестах это
// httptest.Recorder, здесь `rec`), тем же приёмом, что и literalKeyRe в
// i18n_keys_test.go для первого аргумента i18n.T/Tf/Tn.
//
// Сканирует МЕСТА ВЫЗОВА, а не литералы по префиксу "flash." — так решено
// осознанно (см. бриф задачи): черновая сверка по префиксу зацепила бы ещё
// два места, дефектом не являющиеся, — flash.typo
// (internal/web/flash_internal_test.go, намеренная фикстура теста поведения
// при неизвестном ключе) и flash.close (internal/web/templates/flash.templ,
// подпись кнопки закрытия — обычный i18n.T-ключ разметки, а не ключ
// flash-сообщения). Регулярка ниже требует непосредственно ".flashOK("/
// ".flashWarn(" перед литералом, поэтому оба места ей физически не видны:
// первое — вызов i18n.T, не flashOK/flashWarn; второе живёт в _test.go,
// который исключён из обхода ниже по той же причине, что и в
// i18n_keys_test.go (см. TestFlashCallKeysAreWhitelistedAndTranslated).
//
// Ключ, собранный НЕ литералом (internal/web/issues.go:318,
// h.flashOK(w, bulkActionFlashKey[r.FormValue("action")], int(n))) — карта
// «действие → готовый ключ», сама состоящая из литералов
// ("flash.issues_resolved" и т.п.), — этой регуляркой не находится: сразу
// после запятой требуется открывающая кавычка, а не идентификатор карты.
// Это та же граница, что и у конкатенации i18n-ключей (literalKeyRe,
// комментарий про m[3]=="+") — множество значений динамической карты знает
// только код-владелец. Отличие от i18n-конкатенации в том, что там значение
// СОБИРАЕТСЯ ("range."+preset), а здесь готовый ключ лежит literal-значением
// В САМОЙ карте — то есть карта ничем не отличается от места вызова с точки
// зрения риска забытого ключа. Раунд правок 1 (см. отчёт): забытое значение
// в bulkActionFlashKey дало бы ровно то же молчание, что нашла находка про
// отзыв приглашения, а эта регулярка его не видит — поэтому карта проверена
// ОТДЕЛЬНО, через экспортированный срез web.BulkActionFlashKeys, тем же
// приёмом, каким TestDynamicKeysResolve (i18n_dynamic_test.go) читает
// uptime.Kinds/org.Platforms из кода-владельца, а не копирует их литералом.
var flashCallRe = regexp.MustCompile(`\.(flashOK(?:Pair)?|flashWarn)\(\s*[^,]+?\s*,\s*"([^"]+)"`)

// minFlashCallsFound — порог защиты от слепоты: сканер обязан найти хотя бы
// столько вызовов, сколько их заведомо есть в дереве на момент написания
// теста, иначе сломанный обход (например, сузившийся до одного файла) даст
// ноль нарушений и будет неотличим от чистого кода.
//
// Факт на этом дереве — ровно 9 литеральных вызовов: issues.go:307
// (flash.nothing_selected), maintenance.go:284 (flash.saved),
// orgsettings.go:696 (flash.invite_revoked), orgsettings.go:933
// (flash.subject_purged), alerts.go:223/271/358/428 (flash.rules_saved,
// flash.channel_created, flash.channel_updated, flash.deleted), teams.go:224
// (flash.saved). issues.go:318 (динамический ключ через bulkActionFlashKey,
// см. flashCallRe) в это число не входит — регулярка его не видит.
// Перечень выше — снимок на момент написания теста; дерево с тех пор ушло
// вперёд (сейчас вызовов больше двух десятков), но порог — floor, а не
// точный счётчик, и за фактом не гонится. Фикс-раунд 1 ревью T4 (B6)
// добавил вызов нового вида — h.flashOKPair(w, "flash.recipes_applied", ...)
// в recipes.go — и включил flashOKPair в регулярку; порог поднят 8→9 как
// зарубка того же раунда: обвал обхода по-прежнему ловится, а сам вызов
// Pair-варианта проверен зелёным прогоном после расширения регулярки.
// Порог поставлен с запасом вниз тем же приёмом, что minKeysFound в
// i18n_keys_test.go: множество мало и меняется редко (новый вызов flashOK/
// flashWarn — по коммиту), поэтому запас в одну единицу достаточен, чтобы не
// дёргать тест на каждой мелкой правке, и при этом ловит обвал обхода.
const minFlashCallsFound = 9

// flashKeysBlockRe вырезает тело `var flashKeys = map[string]bool{...}` из
// исходника internal/web/flash.go, flashKeyEntryRe — literal-записи внутри
// него. Белый список читается из ЕГО ЖЕ исходника, а не дублируется здесь
// копией: скопированный список неизбежно разошёлся бы с оригиналом при
// следующей правке flash.go, и тест продолжал бы зеленеть на устаревшем
// снимке — тот же принцип, каким tree.go читает каталоги локалей из JSON
// вместо того, чтобы держать их копию в Go-коде.
var flashKeysBlockRe = regexp.MustCompile(`(?s)var flashKeys = map\[string\]bool\{(.*?)\n\}`)
var flashKeyEntryRe = regexp.MustCompile(`"([^"]+)":\s*true`)

// TestFlashCallKeysAreWhitelistedAndTranslated: каждый ключ, с которым
// реально зовут flashOK/flashWarn, обязан быть в белом списке flashKeys и
// иметь перевод в обоих каталогах локалей. Проверяются оба источника таких
// ключей: литеральные вызовы (flashCallRe, по месту в файле) и динамическая
// карта bulkActionFlashKey (через web.BulkActionFlashKeys, см. раунд правок
// 1 в отчёте) — второй источник добавлен после ревью, вскрывшего, что
// сканер по местам вызова карту не видит в принципе.
//
// Забытый в flashKeys ключ setFlash теперь ловит логом (см. D1,
// internal/web/flash.go), но это находка ПОСЛЕ деплоя — оператор должен
// заметить строку в логе, чтобы узнать о пропаже сообщения. Этот сторож
// ловит ту же дыру ДО деплоя, на этапе `go test`, точно так же, как
// TestEveryKeyInCodeExistsInCatalog ловит забытый i18n-ключ раньше, чем его
// увидит посетитель.
//
// Ключ проверяется на перевод в messages ИЛИ plurals той же локали, а не
// только в messages: flashView (internal/web/templates/flash.templ) сама
// выбирает каталог по числу — i18n.Tn(ctx, f.Key, f.N) при N>0, иначе
// i18n.T(ctx, f.Key), — и какая ветка сработает в проде, решает рантайм-
// значение n, переданное в flashOK/flashWarn (например int(res.Total())),
// а не то, что видно на месте статического вызова. Проверено по факту:
// flash.subject_purged и вся тройка flash.issues_* переведены ТОЛЬКО как
// формы множественного числа (в messages их нет вовсе) — требование "быть
// обязательно в messages" завалило бы тест на чистом дереве.
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
		// _templ.go дублирует свой .templ, но flashOK/flashWarn — вызовы
		// уровня хендлера, в шаблонах их не бывает вовсе; исключение здесь
		// для единообразия с остальными построчными сканерами пакета, а не
		// потому что были найдены ложные срабатывания.
		//
		// _test.go исключён по правилу пакета guards (см. docблок в
		// tree.go, п.1 чеклиста, и обоснование в i18n_keys_test.go): тест
		// вправе намеренно звать flashOK с несуществующим ключом, проверяя
		// поведение setFlash при дыре в белом списке (см.
		// internal/web/flash_internal_test.go: flash.typo и
		// flash.definitely_not_in_the_list) — это часть проверки, а не
		// находка.
		//
		// internal/guards/* исключён по правилу пакета (чеклист, п.2):
		// правило ищет паттерн КОДА (вызов flashOK/flashWarn), и без этого
		// исключения любой будущий пример такого вызова в комментарии этого
		// же файла (как несколькими строками выше — в комментарии к
		// flashCallRe) обманул бы сканер.
		if f.Generated || strings.HasSuffix(f.Path, "_test.go") || strings.HasPrefix(f.Path, "internal/guards/") {
			continue
		}
		for i, line := range strings.Split(f.Body, "\n") {
			// Первый шаг любого построчного сканера пакета (чеклист, п.1) —
			// отсечь "//"-комментарий тем же приёмом, что и у соседей
			// (stripTrailingComment, i18n_leak_test.go): иначе пример вызова
			// в комментарии кода нашёлся бы как настоящий.
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

	// Раунд правок 1: ключ, собранный через bulkActionFlashKey
	// (internal/web/issues.go), сканером выше не виден (см. комментарий к
	// flashCallRe) — карта проверяется отдельно, читая множество значений из
	// кода-владельца (web.BulkActionFlashKeys), а не литералом здесь.
	//
	// Пустой срез — не "нечего проверять", а сигнал, что сборка самого среза
	// в issues.go сломана (тот же приём, что у groups в TestDynamicKeysResolve
	// и areas/codes в TestHelpPanelKeysResolve/TestMonitorErrorCodesResolve):
	// без этого fatal цикл `for range nil` молча оставил бы тест зелёным, что
	// неотличимо от «карта проверена и вопросов нет».
	if len(web.BulkActionFlashKeys) == 0 {
		t.Fatal("web.BulkActionFlashKeys пуст — сборка среза в issues.go сломана, а не карта bulkActionFlashKey опустела по замыслу")
	}
	for _, key := range web.BulkActionFlashKeys {
		checkFlashKey(t, tree, whitelist, "internal/web/issues.go (bulkActionFlashKey)", 0, key)
	}
}

// checkFlashKey — общая проверка одного ключа flash-сообщения: обязан быть в
// белом списке flashKeys и иметь перевод (в messages ИЛИ plurals, см.
// докблок TestFlashCallKeysAreWhitelistedAndTranslated) в обеих локалях.
// Используется и для ключей, найденных по месту вызова (со строкой), и для
// ключей, прочитанных из динамической карты (line=0 — своей строки в файле у
// значения карты нет, сообщение об ошибке называет карту, а не строку).
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
		if _, ok := tree.Plurals[lang][key]; ok {
			continue
		}
		if line > 0 {
			t.Errorf("%s:%d: [%s] ключ %q есть в flashKeys, но перевода нет ни в messages, ни в plurals каталога",
				path, line, lang, key)
		} else {
			t.Errorf("%s: [%s] ключ %q есть в flashKeys, но перевода нет ни в messages, ни в plurals каталога",
				path, lang, key)
		}
	}
}

// flashWhitelist разбирает `var flashKeys = map[string]bool{...}` из
// internal/web/flash.go и возвращает множество допустимых ключей.
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
