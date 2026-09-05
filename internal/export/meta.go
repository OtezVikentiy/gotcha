package export

// Meta — машиночитаемые метаданные заявки на выгрузку: числовой id области,
// код фильтра и (для events без ПДн) пометка о псевдонимах.
//
// Контрактная уборка 2026-08-28 (F5/F1′, CONTRACT-DECISIONS.md): человекочитаемая
// сводка заявки (exports.summary.* — internal/web/exports.go, FilterSummary)
// остаётся локализуемой и живёт в веб-странице/письме, но получателю самого
// ФАЙЛА, которому нужен только числовой id или код фильтра, не должно
// приходиться парсить человеческий текст сводки на языке инстанса — иначе
// правка формулировки перевода становится ломающим изменением для любого,
// кто так делает.
//
// Первый проход клал Meta ВНУТРЬ потока данных (комментарий в шапке CSV,
// первый элемент JSON-массива/первая строка NDJSON) — ревью проверило это
// реальными читателями и показало, что так файл ломается: encoding/csv с
// настройками по умолчанию падает "wrong number of fields" на строке
// колонок (сдвинутой комментарием), csv.DictReader/pandas.read_csv
// разваливают заголовок, наивный `for row in json.load(f): row["id"]`
// падает на элементе 0. Файлы выгрузки (CSV/JSON/NDJSON) остаются ЧИСТЫМИ —
// Meta туда не пишется вовсе (writer.go больше не знает о её существовании).
// Вместо этого Meta — соседний ресурс, доставляемый ТРЕМЯ путями, каждый
// своей аудитории:
//
//  1. Программный получатель файла — GET того же маршрута скачивания с
//     query-параметром meta=1 (см. exportsDownload, internal/web/exports.go):
//     та же авторизация, что и у самого файла, тело ответа — Meta в JSON.
//     Отдельного маршрута не заведено сознательно — Meta описывает уже
//     существующий файл, а не самостоятельную сущность продукта.
//  2. Человек на странице «Выгрузки» — атрибуты data-scope-issue-id/
//     data-filter-code/data-pseudonym-masked на ячейке «Фильтры»
//     (templates.ExportView, exports.templ): те же значения рядом с
//     локализованной строкой FilterSummary, без необходимости парсить её.
//  3. Автор в письме о готовности — non-localized строка "gotcha-export-meta:
//     ..." и (при необходимости) PseudonymUniquenessNote добавляются к телу
//     письма отдельно от локализованной фразы (mailPayload, notify.go).
//
// BuildMeta — единственное место, решающее содержимое Meta; все три пути
// зовут именно её, а не собирают поля заново.
//
// K4-7 аудита: у формата выгрузки не было версии схемы, а значит ломающая
// правка ЛЮБОЙ части этого формата после 1.0 всплыла бы у потребителя только
// сломанным парсингом, без единого различимого признака «это старый файл» —
// SchemaVersion ниже даёт этот признак.
type Meta struct {
	// SchemaVersion — версия КОНТРАКТА выгрузки целиком (см. MetaSchemaVersion):
	// не только полей ЭТОЙ структуры, но и набора колонок/ключей файлов
	// выгрузки (CSV/JSON/NDJSON, оба вида — events/issues, см. EventColumns/
	// IssueColumns и TestExportContract* в contract_version_test.go). Из
	// трёх каналов доставки Meta (докблок выше) это единственный, где версия
	// вообще появляется, — потребитель ФАЙЛА обязан сверяться с ней через
	// meta=1, а не искать признак версии в самом файле (см. ниже, почему).
	//
	// Поле стоит в структуре первым не случайно: потребитель, парсящий JSON
	// вручную по ключам (а не строгой структурой), обязан иметь возможность
	// прочитать именно его раньше, чем упасть на незнакомом или
	// отсутствующем поле где-то дальше.
	SchemaVersion int `json:"schema_version"`
	// ScopeIssueID — Job.ScopeIssueID как есть: 0, если выгрузка не
	// ограничена одной группой (тот же ноль-как-признак, что и в самом поле
	// Job.ScopeIssueID, job.go). Числовой id из exports.summary.issue
	// ("issue #{id}") — то, ради чего заведено это поле.
	ScopeIssueID int64 `json:"scope_issue_id"`
	// FilterCode — закрытый набор FilterCodeIssue/FilterCodeFiltered/
	// FilterCodeAll (см. их докблоки). Период (Params.Since/Until) в код
	// сознательно не входит: в снимке заявки он всегда развёрнут в
	// абсолютные границы (см. докблок Params, job.go) — «весь период» такое
	// же валидное значение, как и любой конкретный диапазон, и не о наличии
	// сужающего фильтра, в отличие от status/level/environment/query.
	FilterCode string `json:"filter_code"`
	// PseudonymNote — F1′: непусто (и равно PseudonymUniquenessNote,
	// pii.go), когда user_id ЭТОГО файла заменён одноразовым псевдонимом
	// (Kind=events, IncludePII=false — см. PseudonymizeUserID/NewExportSalt
	// в pii.go). Пусто в остальных случаях: у kind=issues колонки user_id
	// вообще нет (см. докблок IssueSource), а IncludePII=true отдаёт
	// user_id сырым — предупреждать о псевдониме, которого нет, нечего.
	PseudonymNote string `json:"pseudonym_note,omitempty"`
}

// MetaSchemaVersion — текущая версия КОНТРАКТА выгрузки целиком (K4-7
// аудита): И структуры Meta, И набора колонок/ключей самих файлов
// (CSV/JSON/NDJSON, оба вида заявки). Версия одна на оба фронта сознательно
// — потребителю, которому нужен единственный признак «это старый формат»,
// не нужно сверяться с двумя независимо меняющимися числами; и Meta, и
// файлы одной заявки собираются из одного и того же кода одной и той же
// версии приложения, так что они версионируются синхронно как один продукт.
//
// Значение 1 — первая версия, заведённая этой правкой ДО релиза 1.0: у всех
// Meta, выпущенных раньше, поля schema_version не было вовсе (потребитель
// отличает «до» по отсутствию ключа, а не по значению 0 — считать
// отсутствующее поле version=0 нельзя, json.Unmarshal в структуру
// потребителя даст тот же 0 при ОШИБКЕ разбора числа, и различить эти два
// случая по значению будет невозможно).
//
// Любая последующая несовместимая правка ЛЮБОЙ из двух частей контракта
// обязана поднять эту константу — то, ради чего она заведена:
//   - Meta: переименование/удаление поля, смена типа;
//   - файлы выгрузки: переименование/удаление/смена типа колонки CSV
//     (EventColumns/IssueColumns) или ключа Record, попадающего в
//     JSON/NDJSON целиком (toRecord у eventSource/issueSource) — ДОБАВЛЕНИЕ
//     новой колонки/ключа в конец списка несовместимым не считается (тот же
//     принцип, что у остального контракта продукта, CONTRACT-DECISIONS.md),
//     переименование или изменение порядка/типа существующих — считается.
//
// TestExportContract* (contract_version_test.go) прогоняют писатели на
// фикстуре и сверяют реальный вывод с зафиксированным списком колонок/
// ключей для КАЖДОГО формата и вида заявки — падение теста прямо говорит,
// что нужно поднять эту константу.
const MetaSchemaVersion = 1

const (
	// FilterCodeIssue — заявка ограничена одной группой (ScopeIssueID != 0);
	// человекочитаемый аналог — ключ i18n exports.summary.issue.
	FilterCodeIssue = "issue"
	// FilterCodeFiltered — область «проект», сужена хотя бы одним из
	// status/level/environment/query.
	FilterCodeFiltered = "filtered"
	// FilterCodeAll — область «проект» целиком, без единого фильтра сверх
	// периода; человекочитаемый аналог — exports.summary.no_filters.
	FilterCodeAll = "all"
)

// BuildMeta собирает Meta по заявке — единственное место, решающее, что из
// заявки становится машиночитаемыми метаданными. Зовётся из трёх мест
// (exportsDownload и exportViewRow в internal/web/exports.go, mailPayload в
// notify.go), теми же полями Job, что exportFilterSummary
// (internal/web/exports.go) использует для человеческой сводки, — все они
// читают один и тот же снимок Job.ScopeIssueID/Params, не разные копии.
func BuildMeta(job Job) Meta {
	m := Meta{
		SchemaVersion: MetaSchemaVersion,
		ScopeIssueID:  job.ScopeIssueID,
		FilterCode:    filterCode(job.ScopeIssueID, job.Params),
	}
	if job.Kind == KindEvents && !job.IncludePII {
		m.PseudonymNote = PseudonymUniquenessNote
	}
	return m
}

// filterCode — см. докблоки FilterCodeIssue/FilterCodeFiltered/FilterCodeAll.
func filterCode(scopeIssueID int64, p Params) string {
	if scopeIssueID != 0 {
		return FilterCodeIssue
	}
	if p.Status != "" || p.Level != "" || p.Environment != "" || p.Query != "" {
		return FilterCodeFiltered
	}
	return FilterCodeAll
}
