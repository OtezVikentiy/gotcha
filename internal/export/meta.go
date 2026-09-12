package export

// Не пишется в сам файл выгрузки: encoding/csv и наивные JSON-парсеры ломаются на такой
// примеси — доставляется отдельно (meta=1, письмо, атрибуты страницы).
type Meta struct {
	// Версия контракта всей выгрузки, не только этой структуры. Стоит первым
	// полем: ручной построчный JSON-парсер должен прочитать его до незнакомого поля дальше.
	SchemaVersion int `json:"schema_version"`
	// 0 — выгрузка не ограничена одной группой (тот же ноль-как-признак,
	// что в Job.ScopeIssueID).
	ScopeIssueID int64 `json:"scope_issue_id"`
	// Период (Since/Until) сознательно не входит в код: развёрнутый период —
	// валидное значение, не сужающий фильтр.
	FilterCode string `json:"filter_code"`
	// Непусто только когда user_id этого файла заменён псевдонимом
	// (kind=events, IncludePII=false).
	PseudonymNote string `json:"pseudonym_note,omitempty"`
}

// Бампить при любой несовместимой правке Meta или колонок/ключей файлов выгрузки —
// добавление в конец не считается несовместимым.
const MetaSchemaVersion = 1

const (
	// Заявка ограничена одной группой (ScopeIssueID != 0).
	FilterCodeIssue = "issue"
	// Область «проект», сужена хотя бы одним из status/level/environment/query.
	FilterCodeFiltered = "filtered"
	// Область «проект» целиком, без единого фильтра сверх периода.
	FilterCodeAll = "all"
)

// Единственное место, решающее содержимое Meta — все вызывающие читают тот
// же снимок Job, не копии.
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

func filterCode(scopeIssueID int64, p Params) string {
	if scopeIssueID != 0 {
		return FilterCodeIssue
	}
	if p.Status != "" || p.Level != "" || p.Environment != "" || p.Query != "" {
		return FilterCodeFiltered
	}
	return FilterCodeAll
}
