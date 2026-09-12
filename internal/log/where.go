package log

// Единственное место сборки WHERE для List/Histogram/Facet/AttrValues: иначе гистограмма
// разойдётся со списком. Возвращает условия без WHERE и хвостов вызывающего (курсор/LIMIT).
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

	// Severity собирается в одно NOT IN — симметрично IN(?), один аргумент вместо N. Остальные отрицания идут
	// по порядку среза — детерминированный текст запроса важен для голден-тестов.
	var sevNot []string
	for _, p := range f.Not {
		if p.Field == FieldSeverity && !opts.OmitNegative[FieldSeverity] {
			sevNot = append(sevNot, p.Value)
		}
	}
	if len(sevNot) > 0 {
		where += " AND severity NOT IN (?)"
		args = append(args, sevNot)
	}

	for _, p := range f.Not {
		switch p.Field {
		case FieldSeverity:
			continue // уже собраны выше
		case FieldBody:
			if opts.OmitNegative[FieldBody] {
				continue
			}
			where += " AND positionCaseInsensitiveUTF8(body, ?) = 0"
			args = append(args, p.Value)
		case FieldService, FieldEnvironment:
			if opts.OmitNegative[p.Field] {
				continue
			}
			where += " AND " + p.Field + " != ?"
			args = append(args, p.Value)
		case FieldAttr, FieldResourceAttr:
			if opts.OmitNegative[p.Field+":"+p.Key] {
				continue
			}
			// NOT (col[?] = ?), не col[?] != ?: отсутствующий ключ в ClickHouse читается как map[...]="",
			// и его отрицание должно быть истинным — строки без ключа обязаны остаться.
			where += " AND NOT (" + attrColumn(p.Field == FieldResourceAttr) + "[?] = ?)"
			args = append(args, p.Key, p.Value)
		}
	}

	return where, args
}

func attrColumn(resource bool) string {
	if resource {
		return "resource_attrs"
	}
	return "log_attributes"
}

// OmitPositive/OmitNegative — поля, условия по которым не включать: фасет по полю не фильтрует по себе же.
type whereOpts struct {
	OmitPositive map[string]bool
	OmitNegative map[string]bool
	// Условие без аргументов, дописываемое сразу после окна (фасет: "col != ''") — порядок обязан
	// сохраниться, его проверяет голден-тест.
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

func attrOmitKey(a AttrFilter) string {
	if a.Resource {
		return FieldResourceAttr + ":" + a.Key
	}
	return FieldAttr + ":" + a.Key
}
