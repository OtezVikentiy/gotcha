package guards

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/export"
	"gitflic.ru/otezvikentiy/gotcha/internal/host"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/issue"
	"gitflic.ru/otezvikentiy/gotcha/internal/log"
	"gitflic.ru/otezvikentiy/gotcha/internal/metric"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/recipes"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
	"gitflic.ru/otezvikentiy/gotcha/internal/web"
)

// Ключи, собираемые конкатенацией, страж каталога (i18n_keys_test.go) поймать
// не может: множество их значений знает только вызывающий, и он же должен его
// перечислить. Раньше это множество перечислялось здесь же литералом — новый
// уровень issue, статус пробы или класс квоты доезжал бы до страницы сырым
// ключом, а тест оставался зелёным, потому что не знал о новом значении.
//
// Правило вместо этого читает каждое множество из ЕГО источника истины —
// строго в таком порядке приоритета (см. task-3-report.md, раздел «Выбор
// источника истины по группам», для обоснования по каждой из шести групп,
// на момент её написания; задача C9 добавила ещё три по тому же приоритету
// №3):
//
//  1. CHECK-ограничение в схеме — самый авторитетный источник: он же не даёт
//     множеству разъехаться с базой. Единственная группа, у которой такое
//     ограничение действительно есть, — issues.status (issuesStatusValues).
//  2. Существующие экспортированные константа/карта в коде — уровни
//     консенсуса (uptime.Consensus*) и пресеты диапазона (web.TimeRangePresets
//     + web.RangeAll) уже были доступны как код, нужно было только сослаться
//     напрямую вместо копирования строк.
//  3. Новая константа в пакете-владельце — заведена для трёх множеств,
//     которые не описаны ни схемой, ни существующим экспортом: уровни issue
//     (issue.Levels), статусы пробы (uptime.ProbeStatuses), классы квоты
//     (org.QuotaKinds). Задача C9 добавила по той же схеме ещё четыре: типы
//     монитора (uptime.Kinds — константы KindHTTP и т.д. уже существовали,
//     не хватало только экспортированного среза), платформы онбординга
//     (org.Platforms — раньше был только приватный литеральный whitelist
//     internal/web/onboarding.go, allowedPlatforms, теперь он сам строится из
//     org.Platforms), агрегации правил по метрикам (metric.Aggregations —
//     раньше приватная validAggregations в internal/metric/rule.go) и типы
//     метрики (metric.MetricTypes — раунд правок 1, см. ниже). Задача A1/T12
//     добавила пятое множество по той же схеме — виды встроенных инцидентов
//     хоста (host.Kinds: disk/memory/load/silent), источник истины для
//     подписей в уведомлениях host.HostNotifier. Задача 2 плана C2 добавила
//     шестое — уровни severity просмотрщика логов (log.Severities), тот же
//     мультиселект-фильтр на /projects/{id}/logs, что и severity в самом
//     запросе.
//
// Раунд правок 1 (metrics.templ): та же задача нашла ЕЩЁ два места сырого
// значения в соседнем файле, не входившем в исходный список брифа — тип
// метрики (metrics.templ:97, :42) и селект агрегации на странице метрики
// (metrics.templ:112-117). Тип метрики — четвёртая группа here по приоритету
// №3 (metric.MetricTypes, см. докблок в internal/metric/metric.go: закрытый
// список, MapOTLP тихо пропускает всё, что не gauge/sum/histogram).
//
// Агрегация в metrics.templ — ОТДЕЛЬНОЙ группы не заводит, хотя выглядит
// похоже: metricAggOptions (internal/web/templates/metrics.templ) на первый
// взгляд собирает опции из вызова функции, а не из литерального перечисления,
// как в metricalerts.templ, — та же форма, что у настоящих открытых множеств
// в этом файле (rangePresetKeys, quotaKindShortKeys). Разница в том, ЧТО она
// возвращает: обе ветки metricAggOptions — жёстко зашитые литеральные
// подмножества metric.Aggregations, а сравниваемый vm.Agg до попадания в
// шаблон уже нормализован через metricAggFor (internal/web/metrics.go) тем же
// закрытым перечислением. Открытого хвоста тут в принципе нет: любое значение,
// которое реально может оказаться в <option>, гарантированно входит в
// metric.Aggregations — то есть уже проверено группой "metrics.aggregation."
// ниже, надмножеством. Второй, отдельной группы это не требует — заводить её
// значило бы дважды проверять то же самое множество разными путями.
//
// Проверяются ОБА языка: ключ, забытый только в одном каталоге, ловится
// паритетом (internal/i18n/catalog_test.go), но ключ, забытый в обоих, —
// только этим тестом.
func TestDynamicKeysResolve(t *testing.T) {
	tree := Load(t)

	groups := map[string][]string{
		"issues.status.":       issuesStatusValues(t, tree),
		"issues.level.":        issue.Levels,
		"probe.status.":        uptime.ProbeStatuses,
		"range.":               rangePresetKeys(),
		"org.quota.kind.":      quotaKindShortKeys(),
		"uptime.consensus.":    {string(uptime.ConsensusAny), string(uptime.ConsensusMajority), string(uptime.ConsensusAll)},
		"platform.":            org.Platforms,
		"uptime.kind.":         uptime.Kinds,
		"metrics.aggregation.": metric.Aggregations,
		"metrics.type.":        metric.MetricTypes,
		"hosts.kind.":          host.Kinds,
		"logs.severity.":       log.Severities,
		"recipes.":             recipeDynamicKeys(),
	}
	// Пустая группа — не "нечего проверять", а сигнал, что сборка САМОЙ группы
	// сломана (баг в quotaKindShortKeys/rangePresetKeys или опустевший
	// issue.Levels/uptime.ProbeStatuses), а не что в каталоге всё в порядке:
	// цикл `for _, v := range values` по пустому срезу не найдёт ни одной
	// находки и молча оставит тест зелёным. Тот же приём, что и в соседних
	// TestHelpPanelKeysResolve/TestMonitorErrorCodesResolve этого файла.
	for prefix, values := range groups {
		if len(values) == 0 {
			t.Fatalf("группа %q пуста — сборка множества значений сломана, а не каталог", prefix)
		}
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

// rangePresetKeys — суффиксы группы "range.": ключи web.TimeRangePresets
// (пресеты "1h"/"24h"/"7d"/"30d") плюс web.RangeAll ("all").
//
// "custom" сюда сознательно не входит: ключ "range.custom" в
// templates/timerange.templ вызывается литералом (`i18n.T(ctx, "range.custom")`,
// без конкатенации), поэтому его уже покрывает общий сканер каталога
// (i18n_keys_test.go) — числить его ещё и здесь значило бы дублировать
// проверку одного и того же ключа двумя правилами.
func rangePresetKeys() []string {
	out := make([]string, 0, len(web.TimeRangePresets)+1)
	out = append(out, web.RangeAll)
	for k := range web.TimeRangePresets {
		out = append(out, k)
	}
	return out
}

// quotaKindShortKeys — суффиксы группы "org.quota.kind.": ЕДИНСТВЕННАЯ
// настоящая конкатенация в продукте — droppedBreakdown в
// internal/web/orgsettings.go строит ключ как
// `"org.quota.kind."+kind.key+".short"`, то есть каждое значение org.QuotaKinds
// с ДОБАВЛЕННЫМ суффиксом ".short".
//
// Базовые ключи (`org.quota.kind.events` и т.д., без суффикса) сюда
// сознательно НЕ входят: они собираются в orgsettings.go литералом
// (`i18n.T(r.Context(), "org.quota.kind.events")`, без конкатенации) и уже
// проверяются общим сканером каталога (i18n_keys_test.go). Числить их и
// здесь значило бы дублировать чужую проверку и одновременно упускать
// единственную реальную точку риска — забытый перевод именно ".short"-формы,
// которая на странице (разбивка отброшенного по организации) реально
// собирается конкатенацией и рискует остаться сырым ключом.
func quotaKindShortKeys() []string {
	out := make([]string, 0, len(org.QuotaKinds))
	for _, k := range org.QuotaKinds {
		out = append(out, k+".short")
	}
	return out
}

// recipeDynamicKeys — суффиксы группы "recipes.": страницы рецептов (B6)
// собирают конкатенацией четыре формы ключей на каждый рецепт реестра —
// "recipes.<id>.title"/".desc" (карточка и заголовок), ".chart.<key>"
// (заголовок преднастроенного графика), ".series.<suffix>" (подпись ряда в
// легенде; пуст у одиночных рядов — тогда ключа нет) и ".rule.<notekey>"
// (пояснение рекомендованного порога в таблице). Источник истины —
// recipes.All() (приоритет №2: существующий экспорт пакета-владельца):
// новый рецепт/график/порог в реестре без перевода в ОБОИХ каталогах валит
// тест, а не доезжает до страницы сырым ключом.
//
// Статические ключи страниц ("recipes.list.title", "recipes.assumption",
// "recipes.docker.no_rules" и т.д.) сюда сознательно не входят: они зовутся
// литералами и уже покрыты общим сканером каталога (i18n_keys_test.go).
func recipeDynamicKeys() []string {
	var out []string
	for _, r := range recipes.All() {
		out = append(out, r.ID+".title", r.ID+".desc")
		for _, c := range r.Charts {
			out = append(out, r.ID+".chart."+c.Key)
			for _, s := range c.Series {
				if s.LabelSuffix != "" {
					out = append(out, r.ID+".series."+s.LabelSuffix)
				}
			}
		}
		for _, rule := range r.Rules {
			out = append(out, r.ID+".rule."+rule.NoteKey)
		}
	}
	return out
}

// issuesStatusValues — источник истины для issues.status.*: CHECK-ограничение
// в миграции 0003_issues.up.sql, а не константа в коде. В internal/issue есть
// своя копия множества (validStatuses), но она НЕ экспортирована — заведена
// для рантайм-валидации внутри пакета, а не как то самое множество для
// внешних читателей — и, что важнее, она вторична: именно CHECK не даёт
// значению в БД разъехаться со списком, а копия в Go могла бы отстать от
// него незамеченной. Приоритет №1 (схема) в этом случае обгоняет приоритет
// №2 (константа в коде).
func issuesStatusValues(t *testing.T, tree *Tree) []string {
	t.Helper()
	return checkInValues(t, migrationBody(t, tree, "0003_issues.up.sql"), "status")
}

// migrationBody возвращает тело PostgreSQL-миграции по суффиксу пути
// (например "0003_issues.up.sql") — искать по суффиху, а не по точному пути,
// удобнее вызывающим и не завязывает их на длину tree.Root.
func migrationBody(t *testing.T, tree *Tree, pathSuffix string) string {
	t.Helper()
	for _, f := range tree.MigrationsPG {
		if strings.HasSuffix(f.Path, pathSuffix) {
			return f.Body
		}
	}
	t.Fatalf("миграция с суффиксом пути %q не найдена в дереве", pathSuffix)
	return ""
}

// checkInValues разбирает `CHECK (column IN ('a','b','c'))` в тексте миграции
// и возвращает перечисленные значения без кавычек. column подставляется
// вызывающим внутри пакета (не пользовательский ввод), regexp.QuoteMeta
// применён на всякий случай — дешевле, чем полагаться на то, что имя колонки
// никогда не будет содержать спецсимволы регулярки.
//
// `\s` между именем колонки и IN — не косметика: в схеме объявление CHECK
// нередко переносится на следующую строку после DEFAULT (см.
// 0003_issues.up.sql: `DEFAULT 'unresolved'\n  CHECK (status IN (...))`), а
// `\s` в Go (в отличие от `.`) матчит и перевод строки без флага (?s) —
// поэтому разбор работает без дополнительных флагов регулярки.
func checkInValues(t *testing.T, body, column string) []string {
	t.Helper()
	re := regexp.MustCompile(`CHECK\s*\(\s*` + regexp.QuoteMeta(column) + `\s+IN\s*\(([^)]*)\)\)`)
	m := re.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("не нашли CHECK (%s IN (...)) в миграции", column)
	}
	var out []string
	for _, part := range strings.Split(m[1], ",") {
		out = append(out, strings.Trim(strings.TrimSpace(part), "'"))
	}
	return out
}

// TestHelpPanelKeysResolve — панель «Что это за раздел?» собирает два ключа на
// область (`help.<area>.title`/`.body`). Раздел без перевода показывал бы
// "help.teams.title" заголовком.
func TestHelpPanelKeysResolve(t *testing.T) {
	tree := Load(t)
	areas := helpAreasInTemplates(t, tree)
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
// помощи: список обязан приходить из кода, иначе тест проверяет вчерашний
// набор.
func helpAreasInTemplates(t *testing.T, tree *Tree) []string {
	t.Helper()
	seen := map[string]bool{}
	const marker = `helpPanel("`
	for _, f := range tree.Templates {
		data := f.Body
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

// TestMonitorErrorCodesResolve — коды отказа валидации монитора попадают в
// ключ "error.monitor.<code>". Код без перевода показал бы пользователю сырой
// ключ вместо объяснения, что чинить.
func TestMonitorErrorCodesResolve(t *testing.T) {
	tree := Load(t)
	codes := monitorErrorCodes(t, tree)
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

// TestExportFailureReasonKeysResolve — E1 P2-UX-2: причина отказа выгрузки
// (Job.FailureReasonKey) приходит и на страницу «Выгрузки»
// (i18n.T(ctx, e.FailureReasonKey) в exports.templ), и в письмо автору
// (i18n.T(ctx, reasonKey) в export/notify.go) готовой СТРОКОЙ из БД — не
// литералом и не идентификатором, поэтому её не видит ни общий сканер
// каталога (i18n_keys_test.go: literalKeyRe требует литеральную кавычку
// вторым аргументом), ни группы TestDynamicKeysResolve выше (там ключ
// СОБИРАЕТСЯ конкатенацией по известному префиксу, а здесь три готовых
// значения целиком). export.FailureReasonKeys — тот же список, что проверяет
// export.KnownFailureReasonKey (приоритет №3 докблока TestDynamicKeysResolve:
// новая константа в пакете-владельце) — ключ без перевода в любом языке
// показал бы пользователю сырой exports.mail.failed.reason.* вместо причины
// отказа (находка волны 2 полного аудита, кластер 8/10 DEDUP-P1.md).
//
// Раньше единственной подстраховкой был рендер-ассерт
// TestExportsListShowsFailureReasonHintForKnownKey
// (internal/web/templates/exports_test.go) — он сравнивал вывод i18n.T с
// выводом ТОЙ ЖЕ i18n.T на том же ключе и оставался зелёным даже при
// отсутствующем переводе (i18n.T на промахе возвращает сам ключ, и обе
// стороны сравнения совпадали на этом сыром ключе). Тест починен отдельно
// (сравнение с пинованным ожидаемым текстом), а это правило — независимая
// проверка каталога, не завязанная на конкретный рендер одной страницы.
//
// Честно о границе гарантии (тот же класс вопроса, что уже раз был найден
// в этой самой задаче — докблок flash_test.go признавал дыру вместо того,
// чтобы её закрыть): цикл ниже надёжно ловит ключ, ОТСУТСТВУЮЩИЙ В ОБОИХ
// каталогах (по любому языку — got==key). Ключ, забытый ТОЛЬКО в en.json
// (RU при этом переведён), эта проверка не поймает: i18n.lookup (catalog.go)
// на промахе в запрошенной locale молча фолбэчит на Default ("ru") и
// возвращает РУССКИЙ перевод, а не сам ключ — got != key, ассерт зелёный.
// Тот же приём (и то же ограничение), что и у соседнего
// TestMonitorErrorCodesResolve этого файла — не изолированная недоработка
// именно этого правила, а сознательное разделение труда: односторонний
// пропуск (ключ есть в одном каталоге, забыт в другом) надёжно и НЕ через
// fallback ловит internal/i18n/catalog_test.go:TestCatalogsHaveIdenticalKeys
// (обязателен, часть общего гейта) — он сравнивает множества ключей
// каталогов напрямую, минуя i18n.T и её фолбэк. Разделение то же, что уже
// описано докблоком TestDynamicKeysResolve выше ("Проверяются ОБА языка:
// ключ, забытый только в одном каталоге, ловится паритетом... но ключ,
// забытый в обоих, — только этим тестом").
func TestExportFailureReasonKeysResolve(t *testing.T) {
	keys := export.FailureReasonKeys
	// Пустой список — не "нечего проверять", а сигнал, что сборка среза в
	// worker.go сломана (тот же приём, что у groups в TestDynamicKeysResolve
	// выше): KnownFailureReasonKey с пустым списком не подтверждал бы вообще
	// ничего, такой код не мог бы существовать в проде.
	if len(keys) == 0 {
		t.Fatal("export.FailureReasonKeys пуст — сборка списка в worker.go сломана, а не множество причин опустело по замыслу")
	}
	for _, lang := range []string{"ru", "en"} {
		ctx := i18n.WithLocale(context.Background(), i18n.Locale{Code: lang})
		for _, key := range keys {
			if got := i18n.T(ctx, key); got == key {
				t.Errorf("[%s] причина отказа выгрузки %q без перевода — на странице «Выгрузки» и в письме автору будет сырой ключ", lang, key)
			}
		}
	}
}

// invalidCallRe — invalid("field", "code", ...): второй аргумент и есть код.
var invalidCallRe = regexp.MustCompile(`invalid\("[^"]*",\s*"([^"]+)"`)

// monitorErrorCodes собирает коды из вызовов invalid(...) в авторских (не
// сгенерированных, не тестовых) файлах пакета internal/uptime: список обязан
// приходить из кода, иначе тест закрепляет вчерашний набор.
func monitorErrorCodes(t *testing.T, tree *Tree) []string {
	t.Helper()
	seen := map[string]bool{}
	for _, f := range tree.GoFiles {
		if f.Generated || !strings.HasPrefix(f.Path, "internal/uptime/") || strings.HasSuffix(f.Path, "_test.go") {
			continue
		}
		for _, m := range invalidCallRe.FindAllStringSubmatch(f.Body, -1) {
			seen[m[1]] = true
		}
	}
	out := make([]string, 0, len(seen))
	for c := range seen {
		out = append(out, c)
	}
	return out
}
