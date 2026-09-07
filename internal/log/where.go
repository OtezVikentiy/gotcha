package log

// buildWhere собирает условия WHERE и аргументы к ним для всех запросов
// логов. Единственное место сборки: до него блок был скопирован в List,
// Histogram, Facet и AttrValues, и правка фильтров требовала синхронной
// правки четырёх копий — расхождение проявлялось как гистограмма, не
// совпадающая со списком.
//
// Возвращает условия БЕЗ ключевого слова WHERE и без хвостов, специфичных
// для вызывающего (курсор списка, "col != ”" фасета, LIMIT): их вызывающий
// дописывает сам, добавляя свои аргументы после возвращённых.
//
// opts.OmitPositive / OmitNegative — имена полей, условия по которым не
// включать. Нужны фасетам: фасет по полю не должен применять фильтр по себе
// же, иначе значение исчезает из собственного списка счётчиков вместе
// с возможностью его выбрать или снять.
func buildWhere(projectID int64, f ListFilter, opts whereOpts) (string, []any) {
	where := "project_id = ? AND timestamp >= toDateTime64(?, 3) AND timestamp < toDateTime64(?, 3)"
	args := []any{uint64(projectID), chTimeArg(f.From), chTimeArg(f.To)}

	if opts.BaseExtra != "" {
		where += " AND " + opts.BaseExtra
	}

	if len(f.Severity) > 0 && !opts.OmitPositive[FieldSeverity] {
		where += " AND severity IN (?)"
		args = append(args, f.Severity)
	}
	if f.Service != "" && !opts.OmitPositive[FieldService] {
		where += " AND service = ?"
		args = append(args, f.Service)
	}
	if f.Environment != "" && !opts.OmitPositive[FieldEnvironment] {
		where += " AND environment = ?"
		args = append(args, f.Environment)
	}
	if f.Query != "" && !opts.OmitPositive[FieldBody] {
		where += " AND positionCaseInsensitiveUTF8(body, ?) > 0"
		args = append(args, f.Query)
	}
	for _, a := range f.Attrs {
		if opts.OmitPositive[attrOmitKey(a)] {
			continue
		}
		where += " AND " + attrColumn(a.Resource) + "[?] = ?"
		args = append(args, a.Key, a.Value)
	}
	if f.TraceID != "" && !opts.OmitPositive[FieldTraceID] {
		where += " AND trace_id = ?"
		args = append(args, f.TraceID)
	}

	return where, args
}

// attrColumn — какая из двух карт атрибутов адресуется.
func attrColumn(resource bool) string {
	if resource {
		return "resource_attrs"
	}
	return "log_attributes"
}

// whereOpts — настройки buildWhere для конкретного вызывающего.
type whereOpts struct {
	OmitPositive map[string]bool
	OmitNegative map[string]bool
	// BaseExtra — условие без аргументов, дописываемое сразу после окна.
	// Нужно фасету: "col != ''" стоит в базовой части, до условий фильтров,
	// и порядок обязан сохраниться — его проверяет голден-тест задачи 1.
	BaseExtra string
}

// Имена полей — закрытый список, общий для предикатов, разбора URL и хранения.
const (
	FieldBody         = "body"
	FieldSeverity     = "severity"
	FieldService      = "service"
	FieldEnvironment  = "environment"
	FieldTraceID      = "trace_id"
	FieldAttr         = "attr"
	FieldResourceAttr = "resource_attr"
)

// attrOmitKey — ключ пропуска для условия по конкретному атрибуту:
// фасет значений одного ключа не применяет условия по этому же ключу.
func attrOmitKey(a AttrFilter) string {
	if a.Resource {
		return FieldResourceAttr + ":" + a.Key
	}
	return FieldAttr + ":" + a.Key
}
