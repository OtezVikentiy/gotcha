package guards

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
)

// formatLiteralRe — вызов .Format(...) с литеральным макетом времени сразу
// после открывающей скобки (пробелы перед кавычкой не мешают). Группа 1 —
// сам макет, без кавычек, нужна только для текста сообщения об ошибке — не
// для ключа исключения (см. permanentFormatExemptions, почему ключ теперь
// path:line, а не сам макет).
var formatLiteralRe = regexp.MustCompile(`\.Format\(\s*"([^"]*)"`)

// formatNonLiteralRe — вызов .Format(...), чей аргумент НЕ начинается с
// кавычки: именованная константа (time.RFC3339), переменная (layout в
// internal/web/svgaxis.go:179) или другое поле/выражение. Раунд правок 1:
// изначально в правиле был только formatLiteralRe, и .Format(layout) с
// динамическим человекочитаемым макетом ("02.01"/"15:04", выбранным веткой
// switch чуть выше по файлу) проходил невидимым — ровно та дыра, которую
// правило обязано закрывать: автору достаточно присвоить макет переменной,
// без злого умысла, чтобы обойти сторожа. formatNonLiteralRe и
// formatLiteralRe взаимоисключающие по построению (решает первый непробельный
// символ после открывающей скобки — кавычка или нет), поэтому один вызов
// .Format(...) никогда не считается дважды.
//
// Не ловит вложенные скобки внутри аргумента (например
// .Format(foo(x))) — такого вызова в этом дереве нет, а обычные литералы,
// именованные константы и простые селекторы (a.b.c) все укладываются в
// "нет скобок до закрывающей", так что сужать `[^)]*` не пришлось.
//
// Признак текстовый, не типовой: .Format( — метод не только у time.Time (в
// стандартной библиотеке так же называется, например, метод
// fmt.Formatter.Format(f fmt.State, verb rune), но тот вызывается самим
// пакетом fmt, а не пишется в коде как `.Format(`, и в этом дереве прямых
// вызовов `.Format(` на не-время-подобном значении не встретилось ни разу —
// проверено полным перебором совпадений `\.Format\(` по всему дереву перед
// тем, как писать это правило: все они либо на time.Time/производных, либо
// literal-ветка формата выше. Если такой вызов появится на другом типе,
// правило потребует под него исключение так же, как и под любое другое
// совпадение — это ложное срабатывание, а не молчаливая дыра.
var formatNonLiteralRe = regexp.MustCompile(`\.Format\(\s*([^"\s][^)]*)\)`)

// durationSubStringRe — X.Sub(Y).String(): Sub у time.Time всегда
// возвращает time.Duration, поэтому цепочка однозначно вызывает String() на
// длительности — типы разбирать не нужно, признак текстовый и надёжный. Не
// ловит вложенные скобки внутри аргумента Sub (например .Sub(foo(x)).String()
// — такого вызова в этом дереве нет), усложнять регулярку ради
// гипотетического случая не стали.
var durationSubStringRe = regexp.MustCompile(`\.Sub\([^()]*\)\.String\(\)`)

// durationLiteralStringRe — буквальное "Duration.String()": поле или тип с
// именем ровно Duration перед .String() (time.Duration.String(),
// x.Duration.String()). \b перед "Duration" НЕ совпадёт внутри более
// длинного идентификатора вроде myDuration — граница слова не проходит между
// двумя буквами ("y" и "D" оба словообразующие), и это намеренно: иначе
// правило обвиняло бы в вызове на настоящем time.Duration любую переменную,
// в имени которой есть слово "duration" (retryIn, elapsedTotal — как угодно
// названную), хотя такой переменной оно не находит вовсе — только явное имя
// "Duration".
var durationLiteralStringRe = regexp.MustCompile(`\bDuration\.String\(\)`)

// exemptLoc строит ключ исключения — "path:line", 1-индексная строка. Раунд
// правок 1: изначально ключом был сам найденный макет/аргумент ("02.01",
// "2006-01-02T15:04" и т.п.) — компактно, но опасно неверно по смыслу этого
// же сторожа: исключение по значению разрешает МАКЕТ всюду в дереве, а не
// конкретное место, где он сейчас стоит. Новая копия форматирования,
// использующая уже разрешённый макет, проходила бы молча — ровно то, из-за
// чего разъехались шесть старых копий (см. докблок internal/humanize и
// TestNoRawTimeFormattingOutsideHumanize ниже). Ключ по месту устраняет это:
// каждая строка исходников — отдельная запись, которую нельзя случайно
// прикрыть чужим оправданием.
//
// Оборотная сторона, принятая осознанно: ключ по номеру строки хрупок к
// сдвигу строк (правка кода ВЫШЕ находки на той же странице сделает
// исключение "устаревшим", даже если сама находка никуда не делась) — тот же
// компромисс, на который явно пошёл автор задачи в разборе с ревьюером;
// стабильность ключа принесена в жертву точности того, ЧТО именно он
// разрешает. Если исключение стало "устаревшим" из-за сдвига строк, а не
// починки, — поправить номер строки, а не считать это нарушением само по
// себе.
func exemptLoc(path string, line int) string {
	return fmt.Sprintf("%s:%d", path, line)
}

// permanentFormatExemptions — машинные и намеренно-отдельные форматы
// времени: не форматирование ДЛЯ ЧЕЛОВЕКА либо осознанное дизайн-решение под
// конкретное техническое ограничение, а не случайная копия, — и то, и
// другое остаётся таким навсегда, а не до починки. Ключ — path:line
// (exemptLoc), не сам макет: см. её докблок, почему.
var permanentFormatExemptions = []Exemption{
	// <input type="datetime-local"> отдаёт и принимает значение только в
	// этом виде (без секунд, без зоны) — протокол HTML-формы, а не текст для
	// глаз. Восемь мест: сериализация value= (timerange.go, оба поля окна
	// обслуживания в maintenance.templ) и сборка входных данных в пяти
	// тестах (четыре в timerange_test.go, один в performance_test.go) тем
	// же машинным форматом.
	{Value: exemptLoc("internal/web/timerange.go", 216), Why: `timeRangeFieldValue: return t.UTC().Format("2006-01-02T15:04") — сериализация value= для <input type="datetime-local">, протокол HTML-формы`, Finding: "по замыслу"},
	{Value: exemptLoc("internal/web/templates/maintenance.templ", 53), Why: `f["starts_at"] = w.StartsAt.In(loc).Format("2006-01-02T15:04") — то же поле формы datetime-local для окна обслуживания`, Finding: "по замыслу"},
	{Value: exemptLoc("internal/web/templates/maintenance.templ", 56), Why: `f["ends_at"] = w.EndsAt.In(loc).Format("2006-01-02T15:04") — то же поле формы datetime-local`, Finding: "по замыслу"},
	{Value: exemptLoc("internal/web/timerange_test.go", 148), Why: `start := now.Add(-48 * time.Hour).Format("2006-01-02T15:04") — сборка входного параметра start= тем же машинным форматом, что и сама форма, не дублирование человекочитаемого (TestParseTimeRangeStartOnly)`, Finding: "по замыслу"},
	{Value: exemptLoc("internal/web/timerange_test.go", 178), Why: `future := now.Add(48 * time.Hour).Format("2006-01-02T15:04") — сборка входного параметра end= (TestParseCustomRangeClampsFutureEnd)`, Finding: "по замыслу"},
	{Value: exemptLoc("internal/web/timerange_test.go", 179), Why: `start := now.Add(-2 * time.Hour).Format("2006-01-02T15:04") — сборка входного параметра start= (TestParseCustomRangeClampsFutureEnd)`, Finding: "по замыслу"},
	{Value: exemptLoc("internal/web/timerange_test.go", 192), Why: `start := now.Add(-2 * time.Hour).Format("2006-01-02T15:04") — сборка входного параметра start= (TestParseTimeRangeCustomEndDefaultsToNow)`, Finding: "по замыслу"},
	{Value: exemptLoc("internal/web/performance_test.go", 415), Why: `start := now.Add(-45 * 24 * time.Hour).Format("2006-01-02T15:04") — сборка входного параметра ?start= тем же машинным форматом, что и сама форма (TestWebEndpointDetailSlowestExpiryConfigurable нужен custom-диапазон на 45 дней назад, дефолтные 24ч не захватили бы старые трейсы)`, Finding: "по замыслу"},
	{Value: exemptLoc("internal/web/exports_test.go", 505), Why: `"start": {start.Format("2006-01-02T15:04")} — сборка входного параметра start= тем же машинным форматом, что и TimeRangeVM.apply/<input type="datetime-local"> (TestExportsCreateHonorsCustomRangeQuery)`, Finding: "по замыслу"},
	{Value: exemptLoc("internal/web/exports_test.go", 506), Why: `"end": {end.Format("2006-01-02T15:04")} — тот же входной параметр end=, вторая граница диапазона`, Finding: "по замыслу"},

	// internal/uptime/window_dst_test.go: пять мест — все аргументы
	// многострочных t.Errorf/t.Fatalf в тесте перевода часов через DST, но на
	// строках-ПРОДОЛЖЕНИЯХ вызова, а не на строке самого t.Errorf(/t.Fatalf( —
	// testAssertRe (i18n_leak_test.go) построчная и такую конструкцию не
	// ловит, этот сканер тоже построчный и того же ограничения не избегает.
	// Текст читает разработчик, разбирающий упавший тест, а не посетитель
	// сайта.
	{Value: exemptLoc("internal/uptime/window_dst_test.go", 50), Why: `ivs[0].From.In(berlin).Format("15:04 MST") — аргумент t.Errorf на строке-продолжении (TestWeeklyWindowKeepsDurationAcrossDST), диагностика для разработчика`, Finding: "по замыслу"},
	{Value: exemptLoc("internal/uptime/window_dst_test.go", 51), Why: `ivs[0].To.In(berlin).Format("15:04 MST") — тот же вызов t.Errorf, второе значение`, Finding: "по замыслу"},
	{Value: exemptLoc("internal/uptime/window_dst_test.go", 77), Why: `firstPass.In(berlin).Format("15:04 MST") — аргумент t.Fatalf на строке-продолжении (TestAutumnWindowCoversFirstPass)`, Finding: "по замыслу"},
	{Value: exemptLoc("internal/uptime/window_dst_test.go", 78), Why: `ivs[0].From.In(berlin).Format("15:04 MST") — тот же вызов t.Fatalf`, Finding: "по замыслу"},
	{Value: exemptLoc("internal/uptime/window_dst_test.go", 79), Why: `ivs[0].To.In(berlin).Format("15:04 MST") — тот же вызов t.Fatalf, последнее значение`, Finding: "по замыслу"},

	// RFC3339/RFC3339Nano именованной константой — API/лог/телеметрия, ровно
	// категория из брифа задачи ("RFC3339 в API и логах"). otlp.go отдаёт
	// готовое поле JSON-экспорта события; sentry_test.go/transaction_test.go
	// строят тестовые payload'ы в формате, который реально шлют SDK Sentry
	// (sentry-python/sentry-php) — тем же машинным форматом, не дублируя
	// человекочитаемое форматирование.
	{Value: exemptLoc("internal/ingest/otlp.go", 1301), Why: `e["timestamp"] = ts.Format(time.RFC3339Nano) — поле экспортируемого OTLP JSON-события, машинный формат для API`, Finding: "по замыслу"},
	{Value: exemptLoc("internal/ingest/sentry_test.go", 137), Why: `want.Format(time.RFC3339Nano) — тестовый payload в формате Sentry API (TestParseEventMessageOnly)`, Finding: "по замыслу"},
	{Value: exemptLoc("internal/ingest/sentry_test.go", 173), Why: `now.Add(-200*24*time.Hour).Format(time.RFC3339Nano) — тестовый payload в формате Sentry API (TestParseEventClampsTimestampToWindow)`, Finding: "по замыслу"},
	{Value: exemptLoc("internal/ingest/sentry_test.go", 191), Why: `inWindow.Format(time.RFC3339Nano) — тестовый payload в формате Sentry API`, Finding: "по замыслу"},
	{Value: exemptLoc("internal/ingest/transaction_test.go", 69), Why: `base.Add(...).Format(time.RFC3339Nano) — testTransactionRFC3339JSON: часть SDK (sentry-python/старые sentry-php) шлёт timestamps в RFC3339, тестовый payload в том же формате`, Finding: "по замыслу"},
	{Value: exemptLoc("internal/log/parse_ndjson_test.go", 122), Why: `ts.Format(time.RFC3339) — сборка NDJSON-payload с timestamp в RFC3339 (TestParseNDJSONTimestampRFC3339String): вход API логов, машинный формат, не человекочитаемое дублирование`, Finding: "по замыслу"},
	{Value: exemptLoc("internal/log/parse_ndjson_test.go", 251), Why: `tooOld.Format(time.RFC3339) — NDJSON-payload на now-100d для проверки нижней границы окна (TestParseNDJSONTimestampWindowLowerBound), now-относительное время, литералом не заменить`, Finding: "по замыслу"},
	{Value: exemptLoc("internal/log/parse_ndjson_test.go", 265), Why: `tooNew.Format(time.RFC3339) — NDJSON-payload на now+48h для проверки верхней границы окна (TestParseNDJSONTimestampWindowUpperBound)`, Finding: "по замыслу"},

	// internal/web/statuspage.go: statusPageTimeLayout = "2006-01-02 15:04
	// UTC" — НЕ машинный формат (это тот же общий вид, что у humanize.Time),
	// а самостоятельное дизайн-решение под ограничение конкретной страницы:
	// публичная статус-страница без аутентификации, без запроса локали/пояса
	// посетителя и без JS для локализации на клиенте — часовой пояс всегда
	// фиксирован в UTC (см. докблок statusPageTimeLayout в statuspage.go).
	// humanize.Time сюда не годится буквально: она принимает *time.Location
	// конкретного пользователя и печатает НАЗВАНИЕ пояса, а тут пояс всегда
	// один и тот же и известен на этапе компиляции. Это тот случай, который
	// раунд правок 1 заставил переклассифицировать из "машинный" в "разумное
	// постоянное решение" — причина исключения та же (не долг), а
	// формулировка точнее.
	{Value: exemptLoc("internal/web/statuspage.go", 393), Why: `StartedAt: inc.StartedAt.UTC().Format(statusPageTimeLayout) — публичная статус-страница всегда в UTC без JS-локализации, самостоятельный формат по документированному дизайн-решению (см. const statusPageTimeLayout)`, Finding: "по замыслу"},
	{Value: exemptLoc("internal/web/statuspage.go", 457), Why: `From: ni.iv.From.UTC().Format(statusPageTimeLayout) — то же дизайн-решение, окно обслуживания`, Finding: "по замыслу"},
	{Value: exemptLoc("internal/web/statuspage.go", 458), Why: `To: ni.iv.To.UTC().Format(statusPageTimeLayout) — то же дизайн-решение`, Finding: "по замыслу"},

	// relativetime.templ: компонент relativeTime, задача C7 подпроекта единиц
	// ("точное время рядом с относительным"). datetime={ t.UTC().Format(time.RFC3339) }
	// — атрибут <time datetime="…">, машинночитаемый по HTML-спецификации
	// (микроформат, который парсят браузер/скринридер/тестовый ассерт), не
	// текст для человека — тот стоит рядом, в title, и собран через
	// humanize.Time (см. докблок relativeTime). Та же категория, что и
	// value= <input type="datetime-local"> выше (группа timerange.go), только
	// для другого HTML-атрибута.
	{Value: exemptLoc("internal/web/templates/relativetime.templ", 22), Why: `<time datetime={ t.UTC().Format(time.RFC3339) }> — машинночитаемый атрибут datetime, не текст для человека (title рядом уже собран через humanize.Time)`, Finding: "по замыслу"},

	// internal/web/eventdump.go: renderEventForLLM собирает контекст события
	// в текст для вставки в LLM — время в дампе должно быть машинным
	// (RFC3339 UTC), не human-readable: дамп читает модель, а не пользователь.
	{Value: exemptLoc("internal/web/eventdump.go", 105), Why: `ev.Timestamp.UTC().Format(time.RFC3339) — машинный timestamp в LLM-дампе события (текст для модели, не человеческий показ)`, Finding: "по замыслу"},

	// internal/log/query.go: chTimeArg — обход бага биндинга clickhouse-go
	// (задача 2, C2): позиционный "?" форматирует ЛЮБОЙ time.Time-аргумент с
	// жёстко зашитым TimeUnit=Seconds (bindPositional в драйвере), теряя
	// миллисекунды параметра молча — для точечного сравнения Before в
	// курсоре keyset-пагинации это роняло граничную строку. Строка с
	// миллисекундами — SQL-параметр для toDateTime64(?, 3), не текст для
	// человека, см. докблок chTimeArg.
	{Value: exemptLoc("internal/log/query.go", 668), Why: `t.UTC().Format("2006-01-02 15:04:05.000") — SQL-параметр toDateTime64(?, 3), обход бага биндинга clickhouse-go (TimeUnit=Seconds по умолчанию у позиционных "?"), не человекочитаемый вывод`, Finding: "по замыслу"},

	// Фича E1 (выгрузки ошибок/событий), задача 14 (долг гейтов): три
	// категории, все — машинный формат, не дублирование человекочитаемого.
	//
	// internal/export/writer.go: cell() — значение ячейки CSV/JSON/NDJSON
	// файла выгрузки. Файл читает внешний инструмент (Excel, jq, скрипт
	// импорта), не человек с экрана продукта — та же категория, что
	// otlp.go:1301 (поле экспортируемого JSON-события) выше.
	{Value: exemptLoc("internal/export/writer.go", 67), Why: `x.UTC().Format(time.RFC3339) — значение time.Time в ячейке файла выгрузки (cell), формат для внешнего парсера файла, не для чтения с экрана`, Finding: "по замыслу"},

	// internal/web/exports.go: exportDownloadFilename — имя файла при
	// скачивании, gotcha-<kind>-<project>-<YYYYMMDD-HHMM>.<ext>, фиксированный
	// технический конвент по спеке фичи (§10), тот же класс решения, что
	// statusPageTimeLayout (statuspage.go выше): не текст интерфейса, а
	// протокол имени файла, где нет места разделителям ":"/" ", которые ОС
	// либо запрещает в имени файла (Windows — ":"), либо экранирует.
	{Value: exemptLoc("internal/web/exports.go", 90), Why: `at.Format("20060102-1504") — компонент имени скачиваемого файла (спека §10), протокол именования, не текст на странице`, Finding: "по замыслу"},

	// internal/event/query_test.go: TestQueryStreamForExportOrdersByIssueThenTime
	// (или соседний тест того же файла) сравнивает строки экспорта событий по
	// стабильному ключу "issueID@RFC3339" — RFC3339 тут инструмент сравнения
	// (объединяет ID и момент в одну сравнимую строку для slices.Equal), не
	// текст для человека; та же природа, что RFC3339-payload'ы
	// sentry_test.go/parse_ndjson_test.go выше — тестовые данные, не UI.
	{Value: exemptLoc("internal/event/query_test.go", 331), Why: `ev.Timestamp.UTC().Format(time.RFC3339) — ключ сравнения "issueID@RFC3339" в срезе got, машинный формат для slices.Equal, не человекочитаемый вывод`, Finding: "по замыслу"},
	{Value: exemptLoc("internal/event/query_test.go", 338), Why: `t3.Format(time.RFC3339) — тот же ключ сравнения в срезе want, первая строка`, Finding: "по замыслу"},
	{Value: exemptLoc("internal/event/query_test.go", 339), Why: `t1.Format(time.RFC3339) — тот же ключ сравнения в срезе want, вторая строка`, Finding: "по замыслу"},
	{Value: exemptLoc("internal/event/query_test.go", 340), Why: `t2.Format(time.RFC3339) — тот же ключ сравнения в срезе want, третья строка`, Finding: "по замыслу"},
	{Value: exemptLoc("internal/event/query_test.go", 341), Why: `t1.Format(time.RFC3339) — тот же ключ сравнения в срезе want, четвёртая строка`, Finding: "по замыслу"},
}

// maxPermanentFormatExemptions — потолок сознательно поднят с 21 до 22
// задачей 2 подпроекта event-llm-copy (eventdump.go добавил один машинный
// формат, RFC3339 timestamp в LLM-дампе события), затем с 22 до 23 фикс-
// раундом настраиваемого SpanRetentionDays: TestWebEndpointDetailSlowestExpiryConfigurable
// (performance_test.go) собирает ?start= тем же машинным форматом
// datetime-local, что и остальные семь мест выше, — восьмое такое место,
// см. запись в permanentFormatExemptions. Затем с 23 до 26 фичей C1 (приём
// логов): три теста NDJSON-парсера (parse_ndjson_test.go) собирают timestamp
// входного payload в RFC3339 из now-относительного времени (RFC3339-строка и
// обе границы окна ретенции) — тот же машинный формат API, что и тестовые
// payload'ы sentry_test.go/transaction_test.go выше, литералом не заменить.
// Затем с 26 до 27 задачей 2 подпроекта C2 (просмотрщик логов): chTimeArg в
// internal/log/query.go — SQL-параметр toDateTime64(?, 3), обход бага
// биндинга clickhouse-go (см. запись в permanentFormatExemptions). Затем с 27
// до 34 задачей 14 фичи E1 (выгрузки): cell() в internal/export/writer.go
// (значение времени в файле выгрузки, читает внешний инструмент, не экран
// продукта), exportDownloadFilename в internal/web/exports.go (протокол
// имени скачиваемого файла по спеке §10) и пять сравнений в
// internal/event/query_test.go (RFC3339 как ключ сравнения слайсов в тесте
// StreamForExport, не UI) — все семь машинного формата, ни одного
// человекочитаемого дублирования. Затем с 34 до 36 ревью веб-части E1:
// TestExportsCreateHonorsCustomRangeQuery (exports_test.go) собирает
// query-параметры start=/end= тем же машинным форматом datetime-local, что
// и остальные восемь мест выше (TimeRangeVM.apply/<input
// type="datetime-local">) — девятое и десятое такое место, не
// человекочитаемое дублирование.
const maxPermanentFormatExemptions = 36

// debtFormatExemptions — человекочитаемые макеты времени вне
// internal/humanize: настоящие копии форматирования, ради поиска которых и
// писалось это правило. Не чинятся здесь — миграция этих мест предмет
// отдельных задач того же подпроекта: C7 «точное время рядом с
// относительным», C8 «формат дат и окно правила», C9 «сырые машинные
// значения на экране» (ни одна ещё не выполнена на момент написания этого
// правила, см. список задач подпроекта). Список и потолок — ВРЕМЕННЫЙ долг,
// как и leakDebtExemptions (i18n_leak_test.go) и debtCSSClassExemptions
// (css_classes_test.go): их обязаны СНИЖАТЬ по мере починки C7/C8/C9, а не
// пополнять при обычной правке. Ключ — тот же exemptLoc (path:line), что и у
// permanentFormatExemptions.
//
// Номера строк svg.go ниже сдвинуты задачей 13 (multiSeriesMarkup, вставлена
// ВЫШЕ всех этих мест): сама находка та же самая, содержимое не менялось,
// сдвинулась только позиция в файле — это НЕ новый долг и не починка старого.
var debtFormatExemptions = []Exemption{
	// internal/web/svg.go: подписи графиков (тултип точки, подписи оси X) —
	// три разных человекочитаемых макета "день.месяц час:минута" /
	// "день.месяц" / "час:минута" для одной и той же задачи в одном файле,
	// девять мест.
	{Value: exemptLoc("internal/web/svg.go", 344), Why: `p.T.UTC().Format("02.01 15:04") — подпись точки графика (тултип), человекочитаемый макет вне humanize`, Finding: "TBD (подпроект C, задача C8 «формат дат и окно правила»)"},
	{Value: exemptLoc("internal/web/svg.go", 772), Why: `return t.Format("02.01") — подпись оси X (короткая дата)`, Finding: "TBD (подпроект C, задача C8 «формат дат и окно правила»)"},
	{Value: exemptLoc("internal/web/svg.go", 774), Why: `return t.Format("15:04") — подпись оси X (только время)`, Finding: "TBD (подпроект C, задача C8 «формат дат и окно правила»)"},
	{Value: exemptLoc("internal/web/svg.go", 973), Why: `p.T.UTC().Format("02.01 15:04") — подпись точки графика перцентилей (p50)`, Finding: "TBD (подпроект C, задача C8 «формат дат и окно правила»)"},
	{Value: exemptLoc("internal/web/svg.go", 1038), Why: `p.T.UTC().Format("02.01 15:04") — подпись точки графика в HTML-экранированном тултипе`, Finding: "TBD (подпроект C, задача C8 «формат дат и окно правила»)"},
	{Value: exemptLoc("internal/web/svg.go", 1291), Why: `points[idx].T.UTC().Format("02.01") — подпись оси X во flame/vitals-графике`, Finding: "TBD (подпроект C, задача C8 «формат дат и окно правила»)"},
	{Value: exemptLoc("internal/web/svg.go", 1313), Why: `p.T.UTC().Format("02.01 15:04") — подпись точки в другом графике того же файла`, Finding: "TBD (подпроект C, задача C8 «формат дат и окно правила»)"},
	{Value: exemptLoc("internal/web/svg.go", 1746), Why: `points[0].T.UTC().Format("02.01") ... last.T.UTC().Format("02.01") — граница диапазона в заголовке <title>, два вызова на одной строке`, Finding: "TBD (подпроект C, задача C8 «формат дат и окно правила»)"},
	{Value: exemptLoc("internal/web/svg.go", 1882), Why: `title := p.T.UTC().Format("02.01 15:04") — заголовок точки графика`, Finding: "TBD (подпроект C, задача C8 «формат дат и окно правила»)"},
	// Гистограмма объёма логов (T3, C2) — тот же тултип-макет "день.месяц
	// час:минута", что и у остальных девяти мест выше в этом же файле: новый
	// график unavoidably повторяет открытый долг C8 (единого human-friendly
	// хелпера для этого макета в internal/humanize пока нет), а не изобретать
	// свой обходной путь для одного места, оставляя девять соседних как есть.
	// Потолок ниже поднят на 1 осознанно (не тихим "почините находки").
	{Value: exemptLoc("internal/web/svg.go", 1977), Why: `title := times[i].UTC().Format("02.01 15:04") — заголовок корзины гистограммы объёма логов`, Finding: "TBD (подпроект C, задача C8 «формат дат и окно правила»)"},

	// internal/web/templates/timerange.templ: prettyBound больше не находится
	// здесь — задача C8 починила его, переведя на humanize.Time (см. докблок
	// timeRangeLabel в timerange.templ). Запись удалена, а не оставлена
	// устаревшей: список долга обязаны СНИЖАТЬ по мере починки, а не только
	// добавлять.

	// internal/web/templates/monitordetail.templ: sslExpiryText показывает
	// дату истечения TLS-сертификата человеку.
	{Value: exemptLoc("internal/web/templates/monitordetail.templ", 75), Why: `sslExpiryText: date := t.Format("2006-01-02") — дата истечения TLS-сертификата человеку, вне humanize`, Finding: "TBD (подпроект C, задача C8 «формат дат и окно правила»)"},

	// Раунд правок 1 — найдено благодаря formatNonLiteralRe (аргумент не
	// литерал, а именованная константа/переменная):

	// internal/alert/digest.go:96 больше не находится здесь — задача C9
	// починила его, переведя на humanize.Time(ctx, b.Since, time.UTC) (см.
	// докблок в digest.go, место вызова send). Запись удалена, а не оставлена
	// устаревшей — тот же приём, что и с prettyBound выше (задача C8):
	// список долга обязаны СНИЖАТЬ по мере починки, а не только добавлять.

	// internal/web/svgaxis.go: timeAxis выбирает человекочитаемый макет
	// ("02.01"/"15:04", тот же смысл, что у долга в svg.go выше) в переменную
	// layout веткой switch, затем .Format(layout) — то же нарушение, что и у
	// svg.go, но через переменную, а не литерал. Ровно тот случай, который
	// раунд правок 1 попросил закрыть formatNonLiteralRe.
	{Value: exemptLoc("internal/web/svgaxis.go", 181), Why: `ticks = append(ticks, xTick{..., text: t.UTC().Format(layout)}) — layout выбран веткой switch из "02.01"/"15:04" (человекочитаемые макеты, та же природа, что долг в svg.go), передан переменной, а не литералом`, Finding: "TBD (подпроект C, задача C8 «формат дат и окно правила»)"},

	// internal/web/svgaxis.go: writeDeployMarker собирает <title> маркера
	// деплоя «{версия} · {время}» тем же человекочитаемым макетом "02.01 15:04",
	// что и соседние подписи точек графиков в svg.go выше — та же природа долга
	// C8, одно новое место того же уже открытого нарушения.
	{Value: exemptLoc("internal/web/svgaxis.go", 260), Why: `html.EscapeString(d.Version + " · " + d.DeployedAt.UTC().Format("02.01 15:04")) — время в <title> маркера деплоя, человекочитаемый макет вне humanize (тот же, что тултипы точек графиков)`, Finding: "TBD (подпроект C, задача C8 «формат дат и окно правила»)"},
}

// Потолок сознательно опущен с 13 до 12 задачей C8 (prettyBound), затем с 12
// до 11 задачей C9: internal/alert/digest.go:96 (тело письма-сводки) переехал
// на humanize.Time, его запись выше удалена — потолок долгового списка
// двигается только вниз, вслед за реальной починкой, а не остаётся прежним
// "про запас".
// Потолок поднят с 11 до 12 задачей 3 подпроекта C2 (гистограмма объёма
// логов): новый график добавил ОДНО место того же уже открытого долга C8
// (см. запись svg.go:1946 выше) — список долга по-прежнему обязаны СНИЖАТЬ по
// мере починки C7/C8/C9, разовый +1 здесь — не тихое пополнение "про запас",
// а тот самый предусмотренный правилом путь "осознанно поднимите потолок".
// Потолок поднят с 12 до 13 фичей C5 (маркеры деплоев): <title> маркера
// добавил ОДНО место того же уже открытого долга C8 (человекочитаемый макет
// "02.01 15:04" в svgaxis.go), путь тот же осознанный «+1 под починку», не
// тихое пополнение про запас.
const maxDebtFormatExemptions = 13

// minFormatCallsFound — нижний порог: сколько вызовов форматирования времени
// (все четыре признака — .Format("литерал"), .Format(нелитерал),
// .Sub(...).String(), буквальное Duration.String()) правило обязано найти во
// всём дереве, exempted или нет. Без порога сломанный разбор (например, если
// кто-то по невнимательности сузит одну из регулярок или ограничит обход
// только .go-файлами) даёт пустой список найденного — ноль нарушений,
// зелёный тест, неотличимо от дерева, где всё уже переехало в humanize. Тем
// же приёмом защищены minKeysFound (i18n_keys_test.go) и minBlocksInsideMedia
// (css_border_test.go).
//
// Фактически на этом дереве — 34 совпадения на 33 местах (пересчитано после
// задачи 3 подпроекта C2: гистограмма объёма логов добавила одно литеральное
// место, svg.go:1946, в debtFormatExemptions — см. запись выше; свежий
// пересчёт вместо унаследованной истории до C7/C8/C9 см. в git-истории этого
// файла): 24 литеральных макета (formatLiteralRe) + 10
// нелитеральных аргументов (formatNonLiteralRe, раунд правок 1). Нелитеральных
// именно 10, а не 9: макеты, вынесенные в константы (time.RFC3339Nano,
// statusPageTimeLayout), — нелитералы, их легко просчитать как литералы, глядя
// на строку глазами. Совпадений
// на одно больше, чем мест: internal/web/svg.go:1537 несёт два вызова
// .Format("02.01") на одной строке (граница диапазона в заголовке <title>) —
// exemptLoc считает по строке, а не по вызову, поэтому это ОДНО место с
// ДВУМЯ находками. .Sub(...).String()/буквальное Duration.String() — 0 (см.
// комментарий у TestNoRawTimeFormattingOutsideHumanize).
// Из 32 мест в .go-файлах — 28, в .templ — 4 (maintenance.templ:2,
// monitordetail.templ:1, relativetime.templ:1 — последний въехал с задачей
// C7, докблок писался раньше). Порог поставлен ВЫШЕ фактических 28 .go-мест,
// чтобы поломка обхода, смотрящая только Tree.GoFiles (мутационная проверка
// брифа), проваливала именно порог, а не тихо проезжала на находках одних
// .go-файлов, оставив CheckExemptions разбираться с внезапно "устаревшими"
// .templ-исключениями.
//
// Небольшой запас (изначально 34→31, а не с большим отступом, как у
// minBlocksInsideMedia) — намеренно: задачи C7/C8/C9 этого же подпроекта
// переводят долговые находки в internal/humanize, и тогда реальное число
// закономерно падает — это ожидаемое уменьшение долга, а не регрессия
// сканера. Порог здесь оставлен на 31; C7 отклонился от этого хода вещей в
// другую сторону — добавил relativetime.templ:22 в permanentFormatExemptions
// (машинный datetime=, не долг), и фактическое число выросло, а не упало (33
// ≥ 31, запас 2). Снижать порог нужно вместе со списком долга ровно тогда, когда он перестанет
// быть НИЖЕ фактического числа находок, тем же приёмом, каким
// leakDebtExemptions/maxLeakDebtExemptions снижают по мере починки в
// i18n_leak_test.go — большой запас вниз здесь маскировал бы собственную
// поломку обхода не хуже, чем её маскирует пропавший .templ.
const minFormatCallsFound = 31

// TestNoRawTimeFormattingOutsideHumanize — форматирование времени для
// человека разрешено только в internal/humanize (см. докблок
// internal/humanize/humanize.go): раньше шесть независимых копий разошлись
// друг с другом, и одна из них молча занижала конвертацию длительности в
// 1000 раз в письмах-уведомлениях о регрессиях — три недели, пока не
// заметили.
//
// Честно о границе того, что правило видит: оно закрывает класс «свой макет
// ВРЕМЕНИ» — литеральный или вынесенный в переменную .Format(...),
// .Sub(...).String() и буквальное Duration.String() — а не «любая копия
// форматирования вообще». Числовой форматтер ВЕЛИЧИНЫ (готовое число плюс
// fmt.Sprintf/strconv.FormatFloat в ручную склеенную строку, без .Format( и
// без Duration.String()) ни один из четырёх признаков не найдёт. Прямо
// сейчас в дереве живут четыре такие невидимые правилу функции:
// formatDurationUS (templates/performance.templ), waterfallMS
// (internal/web/svg.go) и две независимые копии formatDuration(seconds) —
// internal/trace/regression_notify.go и internal/uptime/notifier.go. Седьмая
// копия правило не поймает, если она такого вида, — это тот класс ошибки, на
// который правило защиты не даёт, а не подстраховка, на которую можно
// положиться.
//
// Четыре признака находки:
//   - .Sub(...).String() и буквальное Duration.String() — вызов String() на
//     значении time.Duration отдаёт машинный вид ("23m0s", "1h30m0s"), а не
//     то, что отдаёт humanize.Duration;
//   - .Format("литерал") — сериализация времени по конкретному макету;
//   - .Format(не-литерал) — раунд правок 1: тот же вызов, но макет спрятан
//     за именованной константой или переменной (typeless макет-в-переменную
//     — ровно то, чем можно было бы тихо обойти литеральную ветку, см.
//     formatNonLiteralRe).
//
// "Человечность" макета/аргумента правило различает через список исключений,
// а не угадыванием по виду строки: в самом тексте макета нет ничего, что
// отличало бы "2006-01-02T15:04" датовика формы от "02.01.2006 15:04"
// подписи для человека, кроме контекста употребления, который сканер
// построчно не видит. Ключ исключения — path:line (exemptLoc), не сам
// макет/аргумент: см. её докблок и докблок permanentFormatExemptions —
// раунд правок 1 сузил охват именно потому, что ключ по значению разрешал
// найденный МАКЕТ везде в дереве, а не то конкретное место, где он сейчас
// оправдан.
//
// internal/guards/ исключён из обхода обеих веток (докблок tree.go, пункт
// 2) — иначе собственные строки Value этого же файла (буквально содержащие
// текст вида ".Format(\"") нашли бы сами себя. internal/humanize/ исключён
// отдельно — это и есть разрешённое место, сканировать его незачем.
func TestNoRawTimeFormattingOutsideHumanize(t *testing.T) {
	tree := Load(t)

	permanentExempt := ExemptedValues(permanentFormatExemptions)
	debtExempt := ExemptedValues(debtFormatExemptions)
	seen := map[string]bool{}
	total := 0

	report := func(path string, line int, snippet, why string) {
		loc := exemptLoc(path, line)
		seen[loc] = true
		total++
		if permanentExempt[loc] || debtExempt[loc] {
			return
		}
		t.Errorf("%s: %s вне internal/humanize: %s", loc, why, snippet)
	}

	scan := func(path, body string) {
		for i, line := range strings.Split(body, "\n") {
			// Пункт 1 докблока tree.go: комментарий отсекается ПЕРВЫМ шагом.
			// Полностью закомментированная строка (doc-комментарии этого же
			// файла и соседей цитируют находки буквально, включая
			// "time.Duration.String()" в humanize.go/regressions.templ) не
			// разбирается вовсе — не только вычищается хвост.
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") {
				continue
			}
			checked := stripTrailingComment(line)

			for _, m := range formatLiteralRe.FindAllStringSubmatch(checked, -1) {
				report(path, i+1, fmt.Sprintf(".Format(%q)", m[1]), "литеральный макет в .Format(")
			}
			for _, m := range formatNonLiteralRe.FindAllStringSubmatch(checked, -1) {
				report(path, i+1, fmt.Sprintf(".Format(%s)", m[1]), "нелитеральный аргумент в .Format(")
			}
			for _, m := range durationSubStringRe.FindAllString(checked, -1) {
				report(path, i+1, m, ".String() на Duration (X.Sub(Y).String())")
			}
			for _, m := range durationLiteralStringRe.FindAllString(checked, -1) {
				report(path, i+1, m, ".String() на Duration (буквальное Duration.String())")
			}
		}
	}

	for _, f := range tree.GoFiles {
		// _templ.go дублирует свой .templ-исходник (сканируется он сам,
		// ниже) — считать находки дважды под разными путями незачем.
		if f.Generated {
			continue
		}
		if strings.HasPrefix(f.Path, "internal/guards/") || strings.HasPrefix(f.Path, "internal/humanize/") {
			continue
		}
		scan(f.Path, f.Body)
	}
	for _, f := range tree.Templates {
		if strings.HasPrefix(f.Path, "internal/guards/") || strings.HasPrefix(f.Path, "internal/humanize/") {
			continue
		}
		scan(f.Path, f.Body)
	}

	if total < minFormatCallsFound {
		t.Fatalf("сканер нашёл %d вызовов форматирования времени, ожидалось не меньше %d — либо поломан сам сканер (сузили регулярку, урезали обход), либо дерево честно почищено настолько, что порог пора снижать вместе со списком долга; в обоих случаях сначала разберитесь, какой из двух случаев это, прежде чем трогать minFormatCallsFound", total, minFormatCallsFound)
	}

	CheckExemptions(t, "TestNoRawTimeFormattingOutsideHumanize (по замыслу)", permanentFormatExemptions, maxPermanentFormatExemptions, seen)
	CheckExemptions(t, "TestNoRawTimeFormattingOutsideHumanize (долг задач C7/C8/C9)", debtFormatExemptions, maxDebtFormatExemptions, seen)
}

// TestFormatCallPatternsRecognizeShapes закрепляет распознавание всех
// четырёх признаков находки отдельно от факта, что дерево уже содержит
// примеры — без этого теста сужение любой из регулярок осталось бы
// незамеченным до тех пор, пока кто-то не удалит соответствующую строку
// исходников и не заметит, что minFormatCallsFound почему-то не упал вместе
// с ней.
func TestFormatCallPatternsRecognizeShapes(t *testing.T) {
	t.Run("formatLiteralRe", func(t *testing.T) {
		cases := []struct {
			name string
			line string
			want string
			ok   bool
		}{
			{"литерал — да", `return t.Format("02.01.2006")`, "02.01.2006", true},
			{"литерал внутри конкатенации — да", `p.T.UTC().Format("02.01 15:04") + " — " + x`, "02.01 15:04", true},
			{"переменная-макет — нет (это ловит formatNonLiteralRe)", `ticks = append(ticks, xTick{x: x, text: t.UTC().Format(layout)})`, "", false},
			{"именованная константа — нет (это тоже ловит formatNonLiteralRe)", `ts.Format(time.RFC3339Nano)`, "", false},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				m := formatLiteralRe.FindStringSubmatch(tc.line)
				if tc.ok != (m != nil) {
					t.Fatalf("FindStringSubmatch(%q) match=%v, want %v", tc.line, m != nil, tc.ok)
				}
				if tc.ok && m[1] != tc.want {
					t.Errorf("макет = %q, want %q", m[1], tc.want)
				}
			})
		}
	})

	t.Run("formatNonLiteralRe", func(t *testing.T) {
		cases := []struct {
			name string
			line string
			want string
			ok   bool
		}{
			{"переменная-макет — да (internal/web/svgaxis.go:179)", `ticks = append(ticks, xTick{x: x, text: t.UTC().Format(layout)})`, "layout", true},
			{"именованная константа time.RFC3339Nano — да", `ts.Format(time.RFC3339Nano)`, "time.RFC3339Nano", true},
			{"именованный пакетный уровень statusPageTimeLayout — да", `StartedAt: inc.StartedAt.UTC().Format(statusPageTimeLayout),`, "statusPageTimeLayout", true},
			{"литерал — нет (это ловит formatLiteralRe, не должно дублировать находку)", `return t.Format("02.01.2006")`, "", false},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				m := formatNonLiteralRe.FindStringSubmatch(tc.line)
				if tc.ok != (m != nil) {
					t.Fatalf("FindStringSubmatch(%q) match=%v, want %v", tc.line, m != nil, tc.ok)
				}
				if tc.ok && m[1] != tc.want {
					t.Errorf("аргумент = %q, want %q", m[1], tc.want)
				}
			})
		}
	})

	t.Run("durationSubStringRe", func(t *testing.T) {
		cases := []struct {
			name string
			line string
			want bool
		}{
			{"Sub().String() — да (мутация задачи: отменённый humanize.Duration)", `return r.ResolvedAt.Sub(r.StartedAt).String()`, true},
			{"Sub() без String() — нет (тот самый безопасный вызов humanize.Duration)", `return humanize.Duration(ctx, r.ResolvedAt.Sub(r.StartedAt))`, false},
			{"String() без Sub() — нет (не имеет отношения к Duration)", `return id.String()`, false},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				if got := durationSubStringRe.MatchString(tc.line); got != tc.want {
					t.Errorf("MatchString(%q) = %v, want %v", tc.line, got, tc.want)
				}
			})
		}
	})

	t.Run("durationLiteralStringRe", func(t *testing.T) {
		cases := []struct {
			name string
			line string
			want bool
		}{
			{"time.Duration.String() — да", `d := time.Duration.String()`, true},
			{"поле Duration — да", `return x.Duration.String()`, true},
			{"переменная myDuration — нет (нет границы слова перед Duration)", `return myDuration.String()`, false},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				if got := durationLiteralStringRe.MatchString(tc.line); got != tc.want {
					t.Errorf("MatchString(%q) = %v, want %v", tc.line, got, tc.want)
				}
			})
		}
	})
}
