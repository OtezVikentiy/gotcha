package guards

import (
	"regexp"
	"strings"
	"testing"
)

// cyrillicLiteral — строковый литерал Go, содержащий хотя бы один
// кириллический символ. Диапазон [а-яА-Я] не включает «ё»/«Ё»: они стоят в
// Unicode отдельно, вне диапазона а-я, — добавлены явно. Приём перенесён из
// internal/web/i18nleak_test.go (там же и объяснение про «ё»).
//
// Для .go-файлов текст, который увидит посетитель, обязан быть Go-строковым
// литералом (Go не умеет вставлять «голый» текст вне строки), поэтому здесь
// достаточно искать кириллицу ВНУТРИ кавычек.
var cyrillicLiteral = regexp.MustCompile(`"[^"]*[а-яА-ЯёЁ][^"]*"`)

// anyCyrillic — просто кириллический символ, без требования кавычек.
//
// В .templ-шаблонах пользовательский текст сплошь и рядом лежит СЫРЫМ HTML
// между тегами (например <span>Gotcha</span>) — это не строковый литерал
// Go, и cyrillicLiteral его не увидит. Мутационная проверка задания (вставить
// <h2>Проблемы проекта</h2> в шаблон) как раз бьёт по этому месту: текст не в
// кавычках. Поэтому шаблоны сканируются этим более широким правилом, а не
// cyrillicLiteral. На .go-файлах так делать нельзя: раздел ниже показывает
// почему.
var anyCyrillic = regexp.MustCompile(`[а-яА-ЯёЁ]`)

// logCallRe — строка ли это вызова журналирования: аргументы log/slog — текст
// для оператора, а не для посетителя, его язык привязан к языку кодовой базы.
//
// Проверка по подстроке «log.» совпадала бы и с «catalog.», и с «dialog.» —
// совпадение должно начинаться на границе идентификатора. Перенесено из
// internal/web/i18nleak_test.go без изменений.
var logCallRe = regexp.MustCompile(`(^|[^\w.])s?log\.`)

// testAssertRe — вызов t.Fatalf/t.Errorf: сообщение об упавшем тесте читает
// разработчик, запустивший тесты, а не посетитель сайта.
//
// Получатель должен быть буквально «t» (а не любой идентификатор, кончающийся
// на «t», — иначе строка "fmt.Errorf(...)" совпала бы: «fmt» тоже оканчивается
// на «t»). Проверено по всему дереву (не только _test.go): единственные
// НЕ тестовые файлы, зовущие t.Fatalf/t.Errorf, — это internal/guards/tree.go,
// internal/guards/exempt.go (тот самый testingT из этого пакета) и
// internal/testenv/testenv.go, и везде получатель называется «t». Более
// широкий шаблон (\w*\.(Fatalf|Errorf)) молча проглотил бы куда больше —
// вообще любой fmt.Errorf/errors.New с кириллицой в оставшемся дереве, а это
// ровно те находки, которые вручную разбирает этот тест (см. legitExemptions
// и leakDebtExemptions).
var testAssertRe = regexp.MustCompile(`(^|[^\w.])t\.(Fatalf|Errorf)\(`)

// legitExemptions — построчные исключения, законные ПО ЗАМЫСЛУ: русский
// текст для ОПЕРАТОРА/РАЗРАБОТЧИКА, а не для посетителя сайта или клиента
// API, и чинить здесь нечего — источник и должен оставаться таким. Поэтому у
// каждой записи Finding — буквально "по замыслу", а не номер находки: если
// сюда попадёт номер находки, кто-то впоследствии решит, что строку надо
// «починить» правкой источника, хотя правка не нужна (см. раунд правок 1 в
// task-4-report.md — это явное требование владельца после решения по
// восьми настоящим утечкам, см. leakDebtExemptions ниже).
var legitExemptions = []Exemption{
	// internal/db/compat.go и migrate.go: ошибки проверки схемы БД при
	// старте бинаря. Единственные вызывающие — MigratePG/MigrateCH/
	// CheckSchemaCurrent(CH) из cmd/gotcha/main.go при запуске процесса —
	// HTTP не поднят, посетителя ещё физически нет. Сообщение читает
	// оператор в консоли/журнале systemd, решая, что делать (накатить
	// миграции руками, поднять force).
	{Value: `return nil, fmt.Errorf("schema compat: имя миграции %s без номера версии "+`, Why: "ошибка проверки схемы БД при старте бинаря — читает оператор в консоли, HTTP ещё не поднят", Finding: "по замыслу"},
	{Value: `"(ожидается <номер>_<имя>.up.sql, номер не больше %d)", name, maxSchemaVersion)`, Why: "продолжение того же сообщения (see выше)", Finding: "по замыслу"},
	{Value: `return nil, fmt.Errorf("schema compat: миграция %s без маркера "+`, Why: "ошибка проверки схемы БД при старте бинаря — читает оператор в консоли, HTTP ещё не поднят", Finding: "по замыслу"},
	{Value: `"«-- backward-compatible: yes|no» в первой строке", name)`, Why: "продолжение того же сообщения (see выше)", Finding: "по замыслу"},
	{Value: `return "", fmt.Errorf("schema check: несовместимая %s-схема: база версии %d впереди "+`, Why: "ошибка проверки схемы БД при старте бинаря — читает оператор в консоли, HTTP ещё не поднят", Finding: "по замыслу"},
	{Value: `"встроенной %d, и версия %s меняет схему обратно-несовместимо — "+`, Why: "продолжение того же сообщения (see выше)", Finding: "по замыслу"},
	{Value: `"обновите бинарь gotcha или восстановите базу из бэкапа", label, got, want,`, Why: "продолжение того же сообщения (see выше)", Finding: "по замыслу"},
	{Value: `"встроенной %d, а о версии %s в schema_compat нет записи — "+`, Why: "продолжение соседнего сообщения о несовместимой схеме", Finding: "по замыслу"},
	{Value: `"признак совместимости неизвестен, старт запрещён; обновите бинарь gotcha", label, got, want,`, Why: "продолжение соседнего сообщения о несовместимой схеме", Finding: "по замыслу"},
	{Value: `return fmt.Sprintf("schema check: %s-схема версии %d впереди встроенной %d; "+`, Why: "предупреждение о схеме при старте бинаря — читает оператор в консоли, HTTP ещё не поднят", Finding: "по замыслу"},
	{Value: `"версия %s помечена обратно-совместимой, работаем на ней",`, Why: "продолжение того же сообщения (see выше)", Finding: "по замыслу"},
	{Value: `return "", fmt.Errorf("schema check: %s-база в состоянии dirty на версии %d — "+`, Why: "ошибка проверки схемы БД при старте бинаря — читает оператор в консоли, HTTP ещё не поднят", Finding: "по замыслу"},
	{Value: `"снимите флаг перед стартом: docker compose run --rm gotcha --migrate-force%s=%d "+`, Why: "продолжение того же сообщения (see выше)", Finding: "по замыслу"},
	{Value: `"(подробности: /docs/upgrade, раздел про dirty)",`, Why: "продолжение того же сообщения (see выше)", Finding: "по замыслу"},
	{Value: `return "", fmt.Errorf("schema check: версия %s-схемы %d отстаёт от встроенной %d — "+`, Why: "ошибка проверки схемы БД при старте бинаря — читает оператор в консоли, HTTP ещё не поднят", Finding: "по замыслу"},
	{Value: `"примените миграции (AUTO_MIGRATE=true или migrate up) перед стартом", label, got, want)`, Why: "продолжение того же сообщения (see выше)", Finding: "по замыслу"},
	{Value: `return 0, errors.New("schema check: не найдено ни одной встроенной PG-миграции")`, Why: "ошибка проверки схемы БД при старте бинаря — читает оператор в консоли, HTTP ещё не поднят", Finding: "по замыслу"},
	{Value: `return 0, errors.New("schema check: не найдено ни одной встроенной CH-миграции")`, Why: "ошибка проверки схемы БД при старте бинаря — читает оператор в консоли, HTTP ещё не поднят", Finding: "по замыслу"},
	{Value: `return fmt.Errorf("migrate up %s: база в состоянии dirty на версии %d — "+`, Why: "ошибка проверки схемы при старте бинаря — читает оператор в консоли, HTTP ещё не поднят", Finding: "по замыслу"},
	{Value: `"предыдущая миграция оборвалась; проверьте схему и снимите флаг: "+`, Why: "продолжение сообщения об оборвавшейся миграции ниже", Finding: "по замыслу"},
	{Value: `"docker compose run --rm gotcha %s=%d (подробности: /docs/upgrade, "+`, Why: "продолжение сообщения об оборвавшейся миграции — читает оператор в консоли", Finding: "по замыслу"},
	{Value: `"раздел про dirty): %w", dir, derr.Version, flag, derr.Version, err)`, Why: "продолжение сообщения об оборвавшейся миграции — читает оператор в консоли", Finding: "по замыслу"},
	{Value: `return fmt.Errorf("migrate force %s: миграции ещё не применялись — снимать нечего", dir)`, Why: "ошибка --migrate-force при старте бинаря — читает оператор в консоли, HTTP не поднят", Finding: "по замыслу"},
	{Value: `return fmt.Errorf("migrate force %s: схема на версии %d not dirty — снимать нечего", dir, version)`, Why: "ошибка --migrate-force — читает оператор в консоли", Finding: "по замыслу"},
	{Value: `return fmt.Errorf("migrate force %s: запрошена версия %d, а dirty-схема стоит на %d — "+`, Why: "ошибка --migrate-force — читает оператор в консоли", Finding: "по замыслу"},
	{Value: `"разрешены только %d (миграция доделана руками) и %d (миграция откачена руками)",`, Why: "продолжение того же сообщения (see выше)", Finding: "по замыслу"},
	{Value: `"(system.tables.uuid пуст — движок Ordinary не поддерживается)", mv)`, Why: "ошибка проверки схемы ClickHouse при старте бинаря — читает оператор в консоли", Finding: "по замыслу"},

	// internal/docs/anchors.go: таблица транслитерации кириллица→латиница для
	// якорей документации (rune → string). Кириллица здесь — КЛЮЧИ карты,
	// однорунные литералы в ОДИНАРНЫХ кавычках ('а', 'б', ...), а не строки в
	// двойных; значения карты — чистый ASCII ("a", "b", ...). Сканер видит
	// кириллицу не в кавычках самого литерала со значением, а между кавычками
	// СОСЕДНИХ, никак не связанных строк на той же строке файла (наивная
	// регулярка не умеет отличить "закрывающую кавычку строки A" от
	// "открывающей кавычки строки B") — то же по духу ограничение, что и у
	// буквы «ё» в исходном тесте: сканер не разбирает Go-синтаксис, только
	// текст строки целиком.
	{Value: `'а': "a", 'б': "b", 'в': "v", 'г': "g", 'д': "d", 'е': "e", 'ё': "e",`, Why: "таблица транслитерации: кириллица — ключи-руны в одинарных кавычках, значения — чистый ASCII; совпадение ложное (см. комментарий выше группы)", Finding: "по замыслу"},
	{Value: `'ж': "zh", 'з': "z", 'и': "i", 'й': "y", 'к': "k", 'л': "l", 'м': "m",`, Why: "продолжение той же таблицы транслитерации", Finding: "по замыслу"},
	{Value: `'н': "n", 'о': "o", 'п': "p", 'р': "r", 'с': "s", 'т': "t", 'у': "u",`, Why: "продолжение той же таблицы транслитерации", Finding: "по замыслу"},
	{Value: `'ф': "f", 'х': "h", 'ц': "c", 'ч': "ch", 'ш': "sh", 'щ': "sch",`, Why: "продолжение той же таблицы транслитерации", Finding: "по замыслу"},
	{Value: `'ъ': "", 'ы': "y", 'ь': "", 'э': "e", 'ю': "yu", 'я': "ya",`, Why: "продолжение той же таблицы транслитерации", Finding: "по замыслу"},

	// internal/guards/tree.go: Load паникует t.Fatalf-ом, если findRoot не
	// нашёл go.mod. Сообщение читает разработчик, запускающий этот же
	// guards-тест (или любой другой в пакете) — не посетитель сайта: пакет
	// guards не участвует в обслуживании HTTP-запросов вообще.
	{Value: `return "", fmt.Errorf("go.mod не найден ни в одном из родительских каталогов от %s", dir)`, Why: "ошибка инструментария тестов (findRoot) — читает разработчик, запустивший go test, пакет guards HTTP не обслуживает", Finding: "по замыслу"},

	// Фича E1 (выгрузки), задача 14 (долг гейтов): весь internal/export —
	// технические ошибки обвязки (БД, advisory-лок, файловая система,
	// сериализация), которые НИКОГДА не долетают до посетителя буквально.
	// Проверено по всем трём путям, которыми что-либо из этого пакета может
	// добраться до человека:
	//  1. internal/web/exports.go — единственный вызывающий с HTTP-ответом.
	//     Каждый обработчик сверяет ошибку ТОЛЬКО через errors.Is с
	//     сентинелом (ErrActiveLimitReached/ErrNotFound/ErrNotDeletable) и
	//     отвечает готовым ключом i18n.T(...) — сырой err.Error() в ответ
	//     не попадает НИ РАЗУ (см. exportsCreate/exportsDownload/exportsDelete).
	//  2. Страница «Выгрузки» (templates/exports.templ) не показывает
	//     Job.LastError вообще — только статус, переведённый через
	//     "exports.status."+Status.
	//  3. Письмо о неудаче (internal/export/notify.go, mailPayload) раньше
	//     подставляло job.LastError в {cause} НАПРЯМУЮ — вот это была
	//     настоящая утечка (англоязычный автор получал русский обрывок),
	//     найденная этим же прогоном гейта. Починено тем же коммитом: письмо
	//     теперь берёт Job.FailureReasonKey — три новых переведённых ключа
	//     exports.mail.failed.reason.{disk_full,too_many_groups,internal},
	//     которые Worker.fail/failPermanent проставляют ПО ТИПУ ошибки (см.
	//     reasonDiskFull/reasonTooManyGroups/reasonInternal и их докблок в
	//     worker.go). Job.LastError остался тем, чем был всегда для этого
	//     пакета — технический текст для БД (last_error) и лога, читает
	//     оператор, разбирающий инцидент, не автор письма.
	// Без этой переклассификации из четырёх сентинелов internal/export
	// (ErrNotFound/ErrNotDeletable/ErrActiveLimitReached — путь 1;
	// ErrStaleClaim нигде дальше worker.go не покидает пакет вовсе) и всех
	// error-обёрток store.go/janitor.go/worker.go/source_events.go/writer.go
	// ни один не проходит дальше error-значения, которое разбирает Go-код,
	// либо строки last_error/slog, которую видит только оператор.

	// internal/export/janitor.go: Tick — все семь ошибок возвращаются в
	// Run(), который зовёт только slog.Warn("export: джанитор: тик", ...) —
	// фоновый цикл без HTTP, без Notify, без записи в last_error заявки.
	{Value: `return fmt.Errorf("export: джанитор: получение соединения: %w", err)`, Why: "Janitor.Tick: ошибка инфраструктуры, наружу — только slog.Warn в Run(), фоновый цикл без HTTP/email", Finding: "по замыслу"},
	{Value: `return fmt.Errorf("export: джанитор: advisory lock: %w", err)`, Why: "Janitor.Tick: та же категория (см. комментарий выше группы)", Finding: "по замыслу"},
	{Value: `return fmt.Errorf("export: джанитор: чистка старых заявок: %w", err)`, Why: "Janitor.Tick: та же категория", Finding: "по замыслу"},
	{Value: `return fmt.Errorf("export: джанитор: заявки на истечение срока: %w", err)`, Why: "Janitor.expireDue: та же категория", Finding: "по замыслу"},
	{Value: `return fmt.Errorf("export: джанитор: пометка истёкших заявок: %w", err)`, Why: "Janitor.expireDue: та же категория", Finding: "по замыслу"},
	{Value: `return fmt.Errorf("export: джанитор: чтение каталога выгрузок: %w", err)`, Why: "Janitor.removeOrphans: та же категория", Finding: "по замыслу"},
	{Value: `return fmt.Errorf("export: джанитор: проверка сирот: %w", err)`, Why: "Janitor.removeOrphans: та же категория", Finding: "по замыслу"},

	// internal/export/source_events.go: ErrTooManyIssues/ErrMaxIssueIDsNotConfigured
	// — сентинелы, которых web/exports.go НЕ знает (единственный потребитель
	// с HTTP — воркер, worker.go:238 сравнивает их errors.Is и переводит в
	// reasonTooManyGroups/reasonInternal ДО письма, см. группу выше).
	// resolveIssueIDs (строка 118) — обёртка ошибки резолва, тем же путём.
	{Value: `var ErrTooManyIssues = errors.New("экспорт: фильтр резолвится в слишком много групп, сузьте условия")`, Why: "сентинел: единственный потребитель — worker.go (errors.Is), не HTTP; текст в письмо не идёт — идёт reasonTooManyGroups", Finding: "по замыслу"},
	{Value: `var ErrMaxIssueIDsNotConfigured = errors.New("экспорт: eventSource собран без потолка id групп (используйте NewEventSource)")`, Why: "сентинел программной ошибки конфигурации (обход NewEventSource) — тот же путь, что ErrTooManyIssues, в проде недостижим при штатной сборке", Finding: "по замыслу"},
	{Value: `return nil, fmt.Errorf("экспорт событий: резолв групп: %w", err)`, Why: "eventSource.Stream: обёртка ошибки резолва, доезжает только до worker.go (errors.Is/reasonKey), не до письма/страницы", Finding: "по замыслу"},

	// internal/export/store.go: четыре сентинела и ~30 error-обёрток SQL.
	// ErrNotFound/ErrNotDeletable/ErrActiveLimitReached — единственные,
	// которые видит web/exports.go, и там они ТОЛЬКО errors.Is + i18n.T
	// (см. группу выше). ErrStaleClaim читает исключительно worker.go
	// (errors.Is в fail/failPermanent/process) — из пакета export наружу не
	// выходит вовсе. Остальные — обёртки ошибок SQL/сериализации, каждая
	// либо возвращается вызывающему Go-коду (worker.go/web/exports.go — тот
	// же путь sentinel-проверки/generic error.internal), либо утекает в
	// last_error (см. общий разбор выше).
	{Value: `ErrNotFound = errors.New("export: заявка не найдена")`, Why: "сентинел: web/exports.go — errors.Is → notFound (404), текст ErrNotFound.Error() в ответ не идёт", Finding: "по замыслу"},
	{Value: `ErrNotDeletable = errors.New("export: заявка ещё выполняется")`, Why: "сентинел: web/exports.go — errors.Is → err.export.not_deletable, текст в ответ не идёт", Finding: "по замыслу"},
	{Value: `ErrStaleClaim = errors.New("export: заявка перехвачена другой попыткой")`, Why: "сентинел: читает только worker.go (errors.Is), из пакета export наружу к посетителю не выходит", Finding: "по замыслу"},
	{Value: `ErrActiveLimitReached = errors.New("export: лимит активных заявок исчерпан")`, Why: "сентинел: web/exports.go — errors.Is → err.export.limit_reached, текст в ответ не идёт", Finding: "по замыслу"},
	{Value: `return Job{}, fmt.Errorf("разбор params: %w", err)`, Why: "scanJob: ошибка десериализации jsonb, возвращается вызывающему Go-коду (Store-методы), не пользователю", Finding: "по замыслу"},
	{Value: `return 0, fmt.Errorf("export: сериализация params: %w", err)`, Why: "insertJob: та же категория (см. комментарий к группе store.go)", Finding: "по замыслу"},
	{Value: `return 0, fmt.Errorf("export: постановка заявки: %w", err)`, Why: "insertJob: та же категория", Finding: "по замыслу"},
	{Value: `return 0, fmt.Errorf("export: постановка заявки: begin: %w", err)`, Why: "EnqueueLimited: та же категория", Finding: "по замыслу"},
	{Value: `return 0, fmt.Errorf("export: постановка заявки: advisory lock: %w", err)`, Why: "EnqueueLimited: та же категория", Finding: "по замыслу"},
	{Value: `return 0, fmt.Errorf("export: постановка заявки: подсчёт активных: %w", err)`, Why: "EnqueueLimited: та же категория", Finding: "по замыслу"},
	{Value: `return 0, fmt.Errorf("export: постановка заявки: commit: %w", err)`, Why: "EnqueueLimited: та же категория", Finding: "по замыслу"},
	{Value: `return Job{}, fmt.Errorf("export: чтение заявки %d: %w", id, err)`, Why: "Get: та же категория — web/exports.go разбирает только errors.Is(ErrNotFound), иначе generic error.internal", Finding: "по замыслу"},
	{Value: `return nil, fmt.Errorf("export: список заявок проекта %d: %w", projectID, err)`, Why: "ByProject: та же категория, читает страница списка через generic error.internal", Finding: "по замыслу"},
	{Value: `return nil, fmt.Errorf("export: разбор заявки проекта %d: %w", projectID, err)`, Why: "ByProject: та же категория", Finding: "по замыслу"},
	{Value: `return nil, fmt.Errorf("export: список заявок проекта %d автора %d: %w", projectID, uid, err)`, Why: "ByProjectForUser: та же категория, что ByProject — читает страница списка через generic error.internal", Finding: "по замыслу"},
	{Value: `return nil, fmt.Errorf("export: разбор заявки проекта %d автора %d: %w", projectID, uid, err)`, Why: "ByProjectForUser: та же категория, что ByProject", Finding: "по замыслу"},
	{Value: `return Job{}, false, fmt.Errorf("export: клейм заявки: %w", err)`, Why: "Claim: та же категория, читает только worker.go (Tick → slog.Warn)", Finding: "по замыслу"},
	{Value: `return nil, fmt.Errorf("export: снятие зависших заявок: %w", err)`, Why: "SweepStale: та же категория, читает только worker.go (Tick → slog.Warn); RETURNING добавлен задачей 2 фикса P0 (письмо на зависших заявках), сигнатура сменилась на []Job — текст обёртки не изменился", Finding: "по замыслу"},
	{Value: `return nil, fmt.Errorf("export: разбор зависшей заявки: %w", err)`, Why: "SweepStale: разбор строки RETURNING (scanJob) — та же категория, что и у остальных Store-методов со списком (см. DueForExpiry/ByProject выше)", Finding: "по замыслу"},
	{Value: `return fmt.Errorf("export: отметка неудачи заявки %d: %w", id, err)`, Why: "Fail: ошибка САМОГО SQL UPDATE (не cause попытки) — читает worker.fail через slog.Warn, в письмо/last_error не попадает", Finding: "по замыслу"},
	{Value: `return fmt.Errorf("export: завершение заявки %d: %w", id, err)`, Why: "Done: та же категория", Finding: "по замыслу"},
	{Value: `return fmt.Errorf("export: постоянный отказ заявки %d: %w", id, err)`, Why: "FailPermanent: та же категория", Finding: "по замыслу"},
	{Value: `return fmt.Errorf("export: удаление заявки %d: %w", id, err)`, Why: "Delete: та же категория, web/exports.go — только errors.Is(ErrNotDeletable), иначе generic error.internal", Finding: "по замыслу"},
	{Value: `return nil, fmt.Errorf("export: заявки на истечение срока: %w", err)`, Why: "DueForExpiry: та же категория, читает только Janitor (Tick → slog.Warn)", Finding: "по замыслу"},
	{Value: `return nil, fmt.Errorf("export: разбор заявки на истечение срока: %w", err)`, Why: "DueForExpiry: та же категория", Finding: "по замыслу"},
	{Value: `return fmt.Errorf("export: пометка истёкших заявок: %w", err)`, Why: "MarkExpired: та же категория, читает только Janitor", Finding: "по замыслу"},
	{Value: `return total, fmt.Errorf("export: чистка старых заявок: %w", err)`, Why: "PurgeRows: та же категория, читает только Janitor", Finding: "по замыслу"},
	{Value: `return "", fmt.Errorf("export: пользователь %d не найден", id)`, Why: "AuthorEmail: читает только notify.go, где ошибка ЛОГИРУЕТСЯ (slog.Warn) и письмо тихо не отправляется — текст в письмо не попадает никогда", Finding: "по замыслу"},
	{Value: `return "", fmt.Errorf("export: адрес автора %d: %w", id, err)`, Why: "AuthorEmail: та же категория", Finding: "по замыслу"},
	{Value: `return nil, fmt.Errorf("export: проверка существующих заявок: %w", err)`, Why: "ExistingIDs: та же категория, читает только Janitor.removeOrphans", Finding: "по замыслу"},
	{Value: `return nil, fmt.Errorf("export: разбор существующих заявок: %w", err)`, Why: "ExistingIDs: та же категория", Finding: "по замыслу"},

	// internal/export/worker.go: конфигурация/инфраструктура тика (Tick,
	// Validate) — читает main.go через slog.Warn, HTTP не поднят на этом
	// пути. process()/writeFile()/stream() — источник cause для
	// fail()/failPermanent(): попадает в last_error (техника для БД/лога) и
	// в reasonKey письма (уже переведённый — см. группу выше), сам текст
	// cause.Error() в письмо не идёт. ErrPermanent/errLimitReached —
	// внутренние сентинелы (errLimitReached наружу не выходит вовсе, см. её
	// докблок).
	{Value: `panic("export: defaultJobTimeout обязан быть строго меньше leaseTTL")`, Why: "init(): паника при старте бинаря на неверной константе — читает разработчик/CI, посетителя ещё нет", Finding: "по замыслу"},
	{Value: `var ErrPermanent = errors.New("export: постоянный отказ сборки выгрузки")`, Why: "сентинел: errors.Is в process(), текст ErrPermanent.Error() сам по себе в письмо не идёт (обёртки ниже дают reasonInternal/reasonTooManyGroups)", Finding: "по замыслу"},
	{Value: `var errLimitReached = errors.New("export: достигнут потолок заявки")`, Why: "внутренний сентинел остановки потока (см. её докблок): writeFile разбирает его сам, наружу как error не возвращает", Finding: "по замыслу"},
	{Value: `return fmt.Errorf("export: конфигурация: MaxRows (%d) обязан быть строго меньше защитного предела потока событий (%d) — иначе усечение по этому пределу проходит без Truncated=true",`, Why: "Config.Validate: ошибка конфигурации из окружения (GOTCHA_*), читает оператор при старте/в логе Tick, не посетитель", Finding: "по замыслу"},
	{Value: `return fmt.Errorf("export: конфигурация: JobTimeout (%s) обязан быть строго меньше leaseTTL (%s)", jt, leaseTTL)`, Why: "Config.Validate: та же категория", Finding: "по замыслу"},
	{Value: `return fmt.Errorf("export: воркер: %w", err)`, Why: "Tick: обёртка Config.Validate, читает main.go через slog.Warn", Finding: "по замыслу"},
	{Value: `return fmt.Errorf("export: воркер: получение соединения: %w", err)`, Why: "Tick: та же категория", Finding: "по замыслу"},
	{Value: `return fmt.Errorf("export: воркер: advisory lock: %w", err)`, Why: "Tick: та же категория", Finding: "по замыслу"},
	{Value: `return fmt.Errorf("export: воркер: клейм заявки: %w", err)`, Why: "Tick: та же категория", Finding: "по замыслу"},
	{Value: `w.fail(ctx, job, fmt.Errorf("подсчёт занятого места в каталоге выгрузок: %w", err), reasonInternal)`, Why: "process(): cause попадает в last_error (техника для БД/лога); письмо получает reasonInternal — уже переведённый текст, см. группу выше", Finding: "по замыслу"},
	{Value: `w.failPermanent(ctx, job, "на диске не осталось места под выгрузку: исчерпан общий бюджет каталога", reasonDiskFull)`, Why: "process(): та же категория, письмо получает reasonDiskFull", Finding: "по замыслу"},
	{Value: `w.fail(ctx, job, fmt.Errorf("переименование файла выгрузки: %w", err), reasonInternal)`, Why: "process(): та же категория, письмо получает reasonInternal", Finding: "по замыслу"},
	{Value: `return writeResult{}, fmt.Errorf("создание временного файла выгрузки: %w", err)`, Why: "writeFile: та же категория — доезжает до last_error/reasonInternal через process(), не до письма дословно", Finding: "по замыслу"},
	{Value: `return writeResult{}, fmt.Errorf("создание писателя выгрузки: %w", err)`, Why: "writeFile: та же категория", Finding: "по замыслу"},
	{Value: `return fmt.Errorf("запись строки выгрузки: %w", err)`, Why: "writeFile (замыкание Write): та же категория", Finding: "по замыслу"},
	{Value: `return writeResult{}, fmt.Errorf("чтение источника выгрузки: %w", streamErr)`, Why: "writeFile: та же категория", Finding: "по замыслу"},
	{Value: `return writeResult{}, fmt.Errorf("закрытие писателя выгрузки: %w", err)`, Why: "writeFile: та же категория", Finding: "по замыслу"},
	{Value: `return writeResult{}, fmt.Errorf("fsync временного файла выгрузки: %w", err)`, Why: "writeFile: та же категория", Finding: "по замыслу"},
	{Value: `return writeResult{}, fmt.Errorf("закрытие временного файла выгрузки: %w", err)`, Why: "writeFile: та же категория", Finding: "по замыслу"},
	{Value: `return fmt.Errorf("%w: источник групп не настроен", ErrPermanent)`, Why: "stream(): ошибка связки cmd/ (Issues не задан) — та же категория, доходит до reasonInternal, не до письма дословно", Finding: "по замыслу"},
	{Value: `return fmt.Errorf("%w: источник событий не настроен", ErrPermanent)`, Why: "stream(): та же категория", Finding: "по замыслу"},
	{Value: `return fmt.Errorf("%w: неизвестный вид выгрузки %q", ErrPermanent, job.Kind)`, Why: "stream(): та же категория", Finding: "по замыслу"},

	// internal/export/writer.go: единственный вызывающий NewWriter —
	// writeFile (см. группу worker.go выше), та же цепочка last_error/
	// reasonInternal.
	{Value: `return nil, fmt.Errorf("экспорт: неизвестный формат %q", f)`, Why: "NewWriter: доезжает до last_error/reasonInternal через writeFile/process(), не до письма дословно", Finding: "по замыслу"},
}

// maxLegitExemptions — потолок списка исключений «по замыслу»: 33 записи
// ушли на многострочные сообщения проверки схемы БД и --migrate-force в
// internal/db (27 строк — ошибки --migrate-force добавили шесть: тот же
// класс «читает оператор в консоли, HTTP не поднят»), одну таблицу
// транслитерации в internal/docs/anchors.go (5 строк) и одну ошибку
// инструментария самого пакета guards (1 строка). Список постоянный, а не
// долг — расти он должен только если появится ещё один законный по замыслу
// случай, и осознанной правкой числа, а не сам по себе.
//
// С 33 до 94 задачей 14 фичи E1 (выгрузки): весь internal/export — долг
// T1/T7, не разобранный на своих задачах (см. групповой комментарий над
// записями). Каждая строка — техническая ошибка обвязки (БД/файл/лок),
// либо сентинел, проверяемый только errors.Is (web/exports.go), либо
// причина, отправляемая в письмо уже ПЕРЕВЕДЁННОЙ через новый
// Job.FailureReasonKey — ни одна не долетает до посетителя буквально; одна
// настоящая утечка (сырой job.LastError в {cause} письма) найдена этим же
// прогоном и починена кодом, а не занесена в исключения.
//
// С 94 до 95 финревью фичи E1 (устранение находки P0 «SweepStale добивает
// заявки мимо Worker.fail — письмо автору не уходит»): SweepStale стала
// возвращать []Job (RETURNING), у обёртки ошибки Query сменился только тип
// нулевого значения (0 → nil, текст тот же), плюс одна новая строка на
// разбор строки RETURNING (scanJob) — та же категория, что и у остальных
// Store-методов со списком.
//
// С 95 до 97 той же волной финревью (находка P2-1, часть в сторе): новый
// Store.ByProjectForUser — точная копия ByProject с предикатом по автору,
// две новые строки обёрток ошибок той же категории.
const maxLegitExemptions = 97

// leakDebtExemptions — временный долг правила: настоящие утечки, найденные
// первым прогоном на расширенном охвате (восемь записей, находки №132–137
// реестра аудита cld/audit/2026-07-30-full-product-audit.md). Выжжен
// подпроектом H 2026-08-03: №132 — kind+description в perf_issues (миграция
// 0058) и заголовок на рендере; №133–136 — уведомители строят тексты из
// каталога i18n по локали инстанса (GOTCHA_LOCALE); №137 — подпись
// провайдера через ключ oauth.provider.<name>, DisplayName Яндекса — латиница.
//
// Список намеренно пуст и расти не должен: новая русская строка вне каталога
// — это баг, а не кандидат сюда (по образцу выжженных
// debtControlBorderExemptions).
var leakDebtExemptions = []Exemption{}

// maxLeakDebtExemptions — потолок долга: ноль, достигнут подпроектом H.
// Роста нет ни при какой правке.
const maxLeakDebtExemptions = 0

// TestNoCyrillicUserFacingLiterals — user-facing текст должен жить в
// каталоге i18n, а не быть зашит буквально в код или шаблон. Русская строка в
// хендлере, детекторе или .templ-файле не ломает ни сборку, ни тесты — она
// просто показывается посетителю с любой локалью как есть.
//
// Финдинг №56 (QA-13): старый тест (internal/web/i18nleak_test.go) сканировал
// только *.go в internal/web, нерекурсивно. Дыра: ни один .templ-шаблон
// (а user-facing текст в основном живёт именно там), ни один другой пакет
// (internal/trace, internal/oauth и т.д., чьи сообщения тоже долетают до
// экрана — заголовки perf-issues, подписи OAuth-кнопок) не проверялись
// вовсе. Этот тест сканирует ВСЁ дерево через guards.Tree.
//
// _test.go исключены из обхода целиком, а не только по вызовам
// t.Fatalf/t.Errorf: черновой прогон без этого исключения даёт около 560
// находок почти сплошь в internal/web/templates/*_test.go и
// internal/web/*_test.go — это литералы вида
// strings.Contains(rendered, "Ключ ещё не создан") (проверка, что каталог
// подставился в рендер) и русскоязычные описания test-case'ов в table-driven
// тестах ({"пусто — на главную", ...}) — они разработчик видит в выводе
// `go test -v`, а не посетитель на странице. То же решение и по той же
// причине уже принято рядом, в TestEveryKeyInCodeExistsInCatalog
// (i18n_keys_test.go) — там _test.go исключён по аналогичным основаниям
// (намеренно неверные ключи в dynamic_keys_test.go). Здесь основание другое
// (объём и природа совпадений), решение то же.
//
// Раунд правок 1 (2026-07-31): при первом прогоне на расширенном охвате
// правило нашло 8 настоящих утечек (см. task-4-report.md). Владелец решил не
// чинить источники сейчас, а занести их в leakDebtExemptions с номерами
// находок — отдельно от legitExemptions (тот список законен по замыслу и не
// является долгом). Два списка проверяются двумя вызовами CheckExemptions —
// с разными потолками и разными именами в сообщении об ошибке, чтобы не
// смешивать «постоянно законное» и «временный долг» в одном отчёте.
func TestNoCyrillicUserFacingLiterals(t *testing.T) {
	tree := Load(t)
	exempt := ExemptedValues(legitExemptions)
	for v := range ExemptedValues(leakDebtExemptions) {
		exempt[v] = true
	}
	seen := map[string]bool{}

	report := func(path string, i int, trimmed string) {
		seen[trimmed] = true
		if exempt[trimmed] {
			return
		}
		t.Errorf("%s:%d: русский литерал вне каталога i18n: %s", path, i+1, trimmed)
	}

	scanLines := func(path, body string, isLeak func(checked string) bool) {
		for i, line := range strings.Split(body, "\n") {
			trimmed := strings.TrimSpace(line)
			switch {
			case strings.HasPrefix(trimmed, "//"):
				continue
			case logCallRe.MatchString(line):
				continue
			case testAssertRe.MatchString(line):
				continue
			}
			checked := stripTrailingComment(line)
			if !isLeak(checked) {
				continue
			}
			report(path, i, trimmed)
		}
	}

	for _, f := range tree.GoFiles {
		// _templ.go дублирует свой .templ-исходник (см. Generated в tree.go),
		// который сканируется отдельно ниже широким правилом — сканировать
		// его ещё и здесь значило бы дважды считать одну и ту же находку под
		// разными путями. _test.go — см. комментарий к тесту выше.
		if f.Generated || strings.HasSuffix(f.Path, "_test.go") {
			continue
		}
		scanLines(f.Path, f.Body, cyrillicLiteral.MatchString)
	}
	for _, f := range tree.Templates {
		scanLines(f.Path, f.Body, anyCyrillic.MatchString)
	}

	CheckExemptions(t, "TestNoCyrillicUserFacingLiterals (по замыслу)", legitExemptions, maxLegitExemptions, seen)
	CheckExemptions(t, "TestNoCyrillicUserFacingLiterals (долг подпроекта H)", leakDebtExemptions, maxLeakDebtExemptions, seen)
}

// stripTrailingComment убирает хвост строки после "//", если это настоящий
// комментарий, а не часть строкового литерала (например URL "https://...").
//
// Ищет ПЕРВОЕ вхождение "//", перед которым чётное число кавычек (значит вне
// строки) — а не просто первое "//" на строке: URL-литерал сам содержит "//"
// (schema "https://"), и наивная проверка по первому вхождению принимала бы
// его за начало комментария, хотя чётность кавычек ПЕРЕД НИМ нечётная (мы
// внутри строки). Без этого цикла строка вида
// `authSvc.Secure = strings.HasPrefix(cfg.BaseURL, "https://") // RA-L1: ...`
// (cmd/gotcha/main.go) не отсекала бы настоящий комментарий после URL.
func stripTrailingComment(line string) string {
	for i := 0; i+1 < len(line); i++ {
		if line[i] == '/' && line[i+1] == '/' && strings.Count(line[:i], `"`)%2 == 0 {
			return line[:i]
		}
	}
	return line
}
