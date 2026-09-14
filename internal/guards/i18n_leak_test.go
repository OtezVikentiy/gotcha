package guards

import (
	"regexp"
	"strings"
	"testing"
)

// [а-яА-Я] не включает «ё»/«Ё» — они стоят в Unicode отдельно, добавлены явно.
var cyrillicLiteral = regexp.MustCompile(`"[^"]*[а-яА-ЯёЁ][^"]*"`)

// В .templ пользовательский текст лежит сырым HTML между тегами, не в
// Go-строковом литерале — cyrillicLiteral его не увидит, нужна кириллица без кавычек.
var anyCyrillic = regexp.MustCompile(`[а-яА-ЯёЁ]`)

// Группа 2 — само имя вызова, отдельно от границы идентификатора в группе 1:
// иначе «log.» совпало бы и с «catalog.», и с «dialog.».
var logCallOpenRe = regexp.MustCompile(`(^|[^\w.])(s?log\.\w+)\(`)

// Маскирует пробелами только текст вызова s?log.Xxx(...), не всю строку.
// Незакрытая на этой строке скобка маскирует до конца строки.
func maskLogCalls(line string) string {
	out := []byte(line)
	offset := 0
	for offset < len(out) {
		loc := logCallOpenRe.FindStringSubmatchIndex(string(out[offset:]))
		if loc == nil {
			break
		}
		callStart := offset + loc[4]
		parenStart := offset + loc[1] - 1
		depth := 1
		end := parenStart + 1
		for end < len(out) {
			switch out[end] {
			case '(':
				depth++
			case ')':
				depth--
			}
			end++
			if depth == 0 {
				break
			}
		}
		for i := callStart; i < end && i < len(out); i++ {
			out[i] = ' '
		}
		offset = end
	}
	return string(out)
}

// Получатель обязан быть буквально «t», не любым идентификатором на «t» —
// иначе "fmt.Errorf(...)" тоже совпало бы («fmt» оканчивается на «t»).
var testAssertRe = regexp.MustCompile(`(^|[^\w.])t\.(Fatalf|Errorf)\(`)

// У каждой записи Finding — буквально "по замыслу", а не номер находки:
// иначе кто-то решит, что строку надо «починить» правкой источника.
var legitExemptions = []Exemption{
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

	// Кириллица тут — ключи-руны в одинарных кавычках; регулярка путает конец
	// и начало соседних, не связанных строк на одной строке файла.
	{Value: `'а': "a", 'б': "b", 'в': "v", 'г': "g", 'д': "d", 'е': "e", 'ё': "e",`, Why: "таблица транслитерации: кириллица — ключи-руны в одинарных кавычках, значения — чистый ASCII; совпадение ложное (см. комментарий выше группы)", Finding: "по замыслу"},
	{Value: `'ж': "zh", 'з': "z", 'и': "i", 'й': "y", 'к': "k", 'л': "l", 'м': "m",`, Why: "продолжение той же таблицы транслитерации", Finding: "по замыслу"},
	{Value: `'н': "n", 'о': "o", 'п': "p", 'р': "r", 'с': "s", 'т': "t", 'у': "u",`, Why: "продолжение той же таблицы транслитерации", Finding: "по замыслу"},
	{Value: `'ф': "f", 'х': "h", 'ц': "c", 'ч': "ch", 'ш': "sh", 'щ': "sch",`, Why: "продолжение той же таблицы транслитерации", Finding: "по замыслу"},
	{Value: `'ъ': "", 'ы': "y", 'ь': "", 'э': "e", 'ю': "yu", 'я': "ya",`, Why: "продолжение той же таблицы транслитерации", Finding: "по замыслу"},

	{Value: `return "", fmt.Errorf("go.mod не найден ни в одном из родительских каталогов от %s", dir)`, Why: "ошибка инструментария тестов (findRoot) — читает разработчик, запустивший go test, пакет guards HTTP не обслуживает", Finding: "по замыслу"},

	{Value: `return fmt.Errorf("export: джанитор: получение соединения: %w", err)`, Why: "Janitor.Tick: ошибка инфраструктуры, наружу — только slog.Warn в Run(), фоновый цикл без HTTP/email", Finding: "по замыслу"},
	{Value: `return fmt.Errorf("export: джанитор: advisory lock: %w", err)`, Why: "Janitor.Tick: та же категория (см. комментарий выше группы)", Finding: "по замыслу"},
	{Value: `return fmt.Errorf("export: джанитор: чистка старых заявок: %w", err)`, Why: "Janitor.Tick: та же категория", Finding: "по замыслу"},
	{Value: `return fmt.Errorf("export: джанитор: заявки на истечение срока: %w", err)`, Why: "Janitor.expireDue: та же категория", Finding: "по замыслу"},
	{Value: `return fmt.Errorf("export: джанитор: пометка истёкших заявок: %w", err)`, Why: "Janitor.expireDue: та же категория", Finding: "по замыслу"},
	{Value: `return fmt.Errorf("export: джанитор: чтение каталога выгрузок: %w", err)`, Why: "Janitor.removeOrphans: та же категория", Finding: "по замыслу"},
	{Value: `return fmt.Errorf("export: джанитор: проверка сирот: %w", err)`, Why: "Janitor.removeOrphans: та же категория", Finding: "по замыслу"},

	{Value: `var ErrTooManyIssues = errors.New("экспорт: фильтр резолвится в слишком много групп, сузьте условия")`, Why: "сентинел: единственный потребитель — worker.go (errors.Is), не HTTP; текст в письмо не идёт — идёт reasonTooManyGroups", Finding: "по замыслу"},
	{Value: `var ErrMaxIssueIDsNotConfigured = errors.New("экспорт: eventSource собран без потолка id групп (используйте NewEventSource)")`, Why: "сентинел программной ошибки конфигурации (обход NewEventSource) — тот же путь, что ErrTooManyIssues, в проде недостижим при штатной сборке", Finding: "по замыслу"},
	{Value: `return nil, fmt.Errorf("экспорт событий: резолв групп: %w", err)`, Why: "eventSource.Stream: обёртка ошибки резолва, доезжает только до worker.go (errors.Is/reasonKey), не до письма/страницы", Finding: "по замыслу"},

	{Value: `ErrNotFound = errors.New("export: заявка не найдена")`, Why: "сентинел: web/exports.go — errors.Is → notFound (404), текст ErrNotFound.Error() в ответ не идёт", Finding: "по замыслу"},
	{Value: `ErrNotDeletable = errors.New("export: заявка ещё выполняется")`, Why: "сентинел: web/exports.go — errors.Is → err.export.not_deletable, текст в ответ не идёт", Finding: "по замыслу"},
	{Value: `ErrStaleClaim         = errors.New("export: заявка перехвачена другой попыткой")`, Why: "сентинел: читает только worker.go (errors.Is), из пакета export наружу к посетителю не выходит", Finding: "по замыслу"},
	{Value: `ErrActiveLimitReached = errors.New("export: лимит активных заявок исчерпан")`, Why: "сентинел: web/exports.go — errors.Is → err.export.limit_reached, текст в ответ не идёт", Finding: "по замыслу"},
	{Value: `return Job{}, fmt.Errorf("разбор params: %w", err)`, Why: "scanJob: ошибка десериализации jsonb, возвращается вызывающему Go-коду (Store-методы), не пользователю", Finding: "по замыслу"},
	{Value: `return 0, fmt.Errorf("export: сериализация params: %w", err)`, Why: "insertJob: та же категория (см. комментарий к группе store.go)", Finding: "по замыслу"},
	{Value: `return 0, fmt.Errorf("export: постановка заявки: %w", err)`, Why: "insertJob: та же категория", Finding: "по замыслу"},
	{Value: `return 0, fmt.Errorf("export: постановка заявки: begin: %w", err)`, Why: "EnqueueLimited: та же категория", Finding: "по замыслу"},
	{Value: `return 0, fmt.Errorf("export: постановка заявки: advisory lock (user): %w", err)`, Why: "EnqueueLimited: та же категория, лок по created_by (P2-SEC-2 аудита)", Finding: "по замыслу"},
	{Value: `return 0, fmt.Errorf("export: постановка заявки: advisory lock (project): %w", err)`, Why: "EnqueueLimited: та же категория, лок по project_id", Finding: "по замыслу"},
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
	{Value: `return time.Time{}, fmt.Errorf("export: завершение заявки %d: %w", id, err)`, Why: "Done: та же категория (сигнатура сменилась на (time.Time, error) — RETURNING expires_at, единый источник срока с письмом)", Finding: "по замыслу"},
	{Value: `return fmt.Errorf("export: постоянный отказ заявки %d: %w", id, err)`, Why: "FailPermanent: та же категория", Finding: "по замыслу"},
	{Value: `return fmt.Errorf("export: возврат заявки %d в очередь: %w", id, err)`, Why: "Release: та же категория (P2-OPS-5) — ошибка САМОГО SQL UPDATE, читает worker.release через slog.Warn, автору письмо не идёт (release не отказ)", Finding: "по замыслу"},
	{Value: `return fmt.Errorf("export: удаление заявки %d: %w", id, err)`, Why: "Delete: та же категория, web/exports.go — только errors.Is(ErrNotDeletable), иначе generic error.internal", Finding: "по замыслу"},
	{Value: `return nil, fmt.Errorf("export: заявки на истечение срока: %w", err)`, Why: "DueForExpiry: та же категория, читает только Janitor (Tick → slog.Warn)", Finding: "по замыслу"},
	{Value: `return nil, fmt.Errorf("export: разбор заявки на истечение срока: %w", err)`, Why: "DueForExpiry: та же категория", Finding: "по замыслу"},
	{Value: `return fmt.Errorf("export: пометка истёкших заявок: %w", err)`, Why: "MarkExpired: та же категория, читает только Janitor", Finding: "по замыслу"},
	{Value: `return total, fmt.Errorf("export: чистка старых заявок: %w", err)`, Why: "PurgeRows: та же категория, читает только Janitor", Finding: "по замыслу"},
	{Value: `return "", fmt.Errorf("export: пользователь %d не найден", id)`, Why: "AuthorEmail: читает только notify.go, где ошибка ЛОГИРУЕТСЯ (slog.Warn) и письмо тихо не отправляется — текст в письмо не попадает никогда", Finding: "по замыслу"},
	{Value: `return "", fmt.Errorf("export: адрес автора %d: %w", id, err)`, Why: "AuthorEmail: та же категория", Finding: "по замыслу"},
	{Value: `return "", fmt.Errorf("export: локаль автора %d: %w", id, err)`, Why: "AuthorLocale: читает только notify.go, где ошибка МОЛЧА игнорируется (fallback на локаль инстанса) — не логируется и в письмо не попадает", Finding: "по замыслу"},
	{Value: `return nil, fmt.Errorf("export: проверка существующих заявок: %w", err)`, Why: "ExistingIDs: та же категория, читает только Janitor.removeOrphans", Finding: "по замыслу"},
	{Value: `return nil, fmt.Errorf("export: разбор существующих заявок: %w", err)`, Why: "ExistingIDs: та же категория", Finding: "по замыслу"},

	{Value: `panic("export: defaultJobTimeout обязан быть строго меньше leaseTTL")`, Why: "init(): паника при старте бинаря на неверной константе — читает разработчик/CI, посетителя ещё нет", Finding: "по замыслу"},
	{Value: `var ErrPermanent = errors.New("export: постоянный отказ сборки выгрузки")`, Why: "сентинел: errors.Is в process(), текст ErrPermanent.Error() сам по себе в письмо не идёт (обёртки ниже дают reasonInternal/reasonTooManyGroups)", Finding: "по замыслу"},
	{Value: `var errLimitReached = errors.New("export: достигнут потолок заявки")`, Why: "внутренний сентинел остановки потока (см. её докблок): writeFile разбирает его сам, наружу как error не возвращает", Finding: "по замыслу"},
	{Value: `return fmt.Errorf("export: конфигурация: MaxRows (%d) обязан быть строго меньше защитного предела потока событий (%d) — иначе усечение по этому пределу проходит без Truncated=true",`, Why: "Config.Validate: ошибка конфигурации из окружения (GOTCHA_*), читает оператор при старте/в логе Tick, не посетитель", Finding: "по замыслу"},
	{Value: `return fmt.Errorf("export: конфигурация: MaxRows (%d) обязан быть положительным — здесь 0 не значит «без лимита», а тихо включает усечение по защитному пределу потока событий без Truncated=true",`, Why: "Config.Validate: та же категория (P2-OPS-1 аудита: MaxRows<=0 тихо включало усечение без Truncated=true)", Finding: "по замыслу"},
	{Value: `return fmt.Errorf("export: конфигурация: MaxBytes (%d) обязан быть положительным — здесь 0 не значит «без лимита», а выключает собственный потолок размера файла",`, Why: "Config.Validate: та же категория (P2-OPS-1 аудита)", Finding: "по замыслу"},
	{Value: `return fmt.Errorf("export: конфигурация: DiskBudget (%d) обязан быть положительным — при 0 или отрицательном значении «занято >= бюджет» истинно на пустом каталоге, и каждая заявка отказывает без единой попытки",`, Why: "Config.Validate: та же категория (P2-OPS-2 аудита: DiskBudget<=0 отказывало каждой заявке без единой попытки)", Finding: "по замыслу"},
	{Value: `return fmt.Errorf("export: конфигурация: TTL (%s) обязан быть положительным — при 0 файл считается истёкшим сразу после сборки, и ближайший тик джанитора сносит его раньше, чем автор успеет скачать",`, Why: "Config.Validate: та же категория (P2-OPS-2 аудита)", Finding: "по замыслу"},
	{Value: `return fmt.Errorf("export: конфигурация: JobTimeout (%s) обязан быть строго меньше leaseTTL (%s)", jt, leaseTTL)`, Why: "Config.Validate: та же категория", Finding: "по замыслу"},
	{Value: `return fmt.Errorf("export: воркер: %w", err)`, Why: "Tick: обёртка Config.Validate, читает main.go через slog.Warn", Finding: "по замыслу"},
	{Value: `return fmt.Errorf("export: воркер: получение соединения: %w", err)`, Why: "Tick: та же категория", Finding: "по замыслу"},
	{Value: `return fmt.Errorf("export: воркер: advisory lock: %w", err)`, Why: "Tick: та же категория", Finding: "по замыслу"},
	{Value: `return fmt.Errorf("export: воркер: клейм заявки: %w", err)`, Why: "Tick: та же категория", Finding: "по замыслу"},
	{Value: `w.fail(ctx, job, fmt.Errorf("подсчёт занятого места в каталоге выгрузок: %w", err), reasonInternal)`, Why: "process(): cause попадает в last_error (техника для БД/лога); письмо получает reasonInternal — уже переведённый текст, см. группу выше", Finding: "по замыслу"},
	{Value: `w.fail(ctx, job, errors.New("на диске не осталось места под выгрузку: исчерпан общий бюджет каталога"), reasonDiskFull)`, Why: "process(): та же категория, письмо получает reasonDiskFull (P2-OPS-4 аудита: бюджет резервируется под текущую заявку — used+MaxBytes>DiskBudget, retryable fail вместо failPermanent)", Finding: "по замыслу"},
	{Value: `w.fail(ctx, job, fmt.Errorf("подсчёт свободного места на файловой системе: %w", err), reasonInternal)`, Why: "process(): cause попадает в last_error, письмо получает reasonInternal — та же категория (P2-OPS-4 аудита: реальное свободное место на ФС хоста)", Finding: "по замыслу"},
	{Value: `w.fail(ctx, job, errors.New("на диске не осталось места под выгрузку: не хватает свободного места на файловой системе хоста"), reasonDiskFull)`, Why: "process(): та же категория, письмо получает reasonDiskFull (P2-OPS-4 аудита)", Finding: "по замыслу"},
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

	{Value: `return nil, fmt.Errorf("экспорт: неизвестный формат %q", f)`, Why: "NewWriter: доезжает до last_error/reasonInternal через writeFile/process(), не до письма дословно", Finding: "по замыслу"},

	{Value: `return QueueSnapshot{}, fmt.Errorf("export: снимок очереди: %w", err)`, Why: "Store.QueueSnapshot: обёртка ошибки SQL, читает только Stats.refresh (RunSnapshots → slog.Warn) — та же категория, что у остальных Store-методов в группе store.go выше", Finding: "по замыслу"},

	{Value: `panic("export: crypto/rand недоступен: " + err.Error())`, Why: "NewExportSalt: паника на отказе crypto/rand.Read — та же категория, что panic() в worker.go:init() (проверка инварианта), recover() в пакете нет, наружу как HTTP-ответ не идёт никогда", Finding: "по замыслу"},
}

const maxLegitExemptions = 108

// Список намеренно пуст и расти не должен: новая русская строка вне каталога
// — это баг, а не кандидат сюда.
var leakDebtExemptions = []Exemption{}

const maxLeakDebtExemptions = 0

// _test.go исключены целиком: иначе черновой прогон тонет в литералах render-проверок
// и русскоязычных описаниях test-case'ов, которые видит только разработчик в `go test -v`.
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
			case testAssertRe.MatchString(line):
				continue
			}
			checked := maskLogCalls(stripTrailingComment(line))
			if !isLeak(checked) {
				continue
			}
			report(path, i, trimmed)
		}
	}

	for _, f := range tree.GoFiles {
		// _templ.go дублирует находки своего .templ-исходника, сканируемого ниже.
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

// Ищет "//" не по первому вхождению, а по первому с чётным числом кавычек
// перед ним — иначе резало бы на "//" внутри URL-литерала.
func stripTrailingComment(line string) string {
	for i := 0; i+1 < len(line); i++ {
		if line[i] == '/' && line[i+1] == '/' && strings.Count(line[:i], `"`)%2 == 0 {
			return line[:i]
		}
	}
	return line
}
