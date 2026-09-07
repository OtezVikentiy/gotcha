package log

import (
	"fmt"
	"slices"
	"strings"
)

type Op string

const (
	OpEq          Op = "eq"
	OpNeq         Op = "neq"
	OpContains    Op = "contains"
	OpNotContains Op = "not_contains"
)

// Predicate — одно условие отбора. Это модель ХРАНЕНИЯ и обмена: в таком виде
// условия лежат в сохранённом фильтре и разбираются из URL. Модель ЗАПРОСА —
// ListFilter: положительные условия там плоскими полями, отрицательные —
// срезом Not. Разделение намеренное: плоские поля читает весь веб-слой,
// переводить их на предикаты значило бы переписать его без всякой выгоды.
type Predicate struct {
	Field string
	Key   string // только для attr и resource_attr
	Op    Op
	Value string
}

// allowedOps — закрытая таблица «поле → допустимые операторы». Всё, чего
// в ней нет, отвергается: это и есть граница, за которой начинается язык
// запросов, которого у нас нет.
var allowedOps = map[string][]Op{
	FieldBody:         {OpContains, OpNotContains},
	FieldSeverity:     {OpEq, OpNeq},
	FieldService:      {OpEq, OpNeq},
	FieldEnvironment:  {OpEq, OpNeq},
	FieldTraceID:      {OpEq},
	FieldAttr:         {OpEq, OpNeq},
	FieldResourceAttr: {OpEq, OpNeq},
}

// maxPredicateValueLen — потолок длины значения условия в символах.
const maxPredicateValueLen = 200

func (p Predicate) Validate() error {
	ops, ok := allowedOps[p.Field]
	if !ok {
		return fmt.Errorf("log: predicate: unknown field %q", p.Field)
	}
	if !slices.Contains(ops, p.Op) {
		return fmt.Errorf("log: predicate: operator %q is not allowed for field %q", p.Op, p.Field)
	}
	if strings.TrimSpace(p.Value) == "" {
		// Пустое значение по телу обнулило бы выдачу целиком: position(body, '')
		// в ClickHouse возвращает 1, поэтому "= 0" ложно для любой строки.
		return fmt.Errorf("log: predicate: empty value for field %q", p.Field)
	}
	if len([]rune(p.Value)) > maxPredicateValueLen {
		return fmt.Errorf("log: predicate: value too long for field %q", p.Field)
	}
	isAttr := p.Field == FieldAttr || p.Field == FieldResourceAttr
	if isAttr && p.Key == "" {
		return fmt.Errorf("log: predicate: attribute field requires a key")
	}
	if !isAttr && p.Key != "" {
		return fmt.Errorf("log: predicate: field %q does not take a key", p.Field)
	}
	if p.Field == FieldSeverity && !slices.Contains(Severities, p.Value) {
		return fmt.Errorf("log: predicate: unknown severity %q", p.Value)
	}
	return nil
}

// NormalizePredicates отбрасывает негодные условия и схлопывает дубли,
// сохраняя порядок первых вхождений: два клика по «исключить» на одном
// значении обязаны дать один предикат и один чип, а не два одинаковых.
func NormalizePredicates(in []Predicate) []Predicate {
	seen := make(map[Predicate]bool, len(in))
	out := make([]Predicate, 0, len(in))
	for _, p := range in {
		if p.Validate() != nil || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

// ApplyPredicates раскладывает список предикатов в фильтр: положительные —
// в плоские поля, отрицательные — в Not. Обратная операция к сборке предикатов
// из ListFilter (web.filterToPredicates); одиночные поля замещаются,
// мультивыбор и атрибуты накапливаются.
//
// Живёт в internal/log (а не в internal/web, откуда переехала при устранении
// находки финального ревью C4), потому что нужна ДВУМ пакетам: web —
// применению сохранённого фильтра и фильтру по умолчанию (задачи 9/10),
// templates/logsavedfilters.templ — построению самодостаточной ссылки
// применения сохранённого фильтра (шаблоны не могут звать web — web и так
// импортирует templates, обратный импорт дал бы цикл). До переезда шаблон
// держал собственную копию того же switch — расхождение с этой функцией по
// severity/service/environment/trace_id/attr/resource_attr прошло бы молча,
// проверенной осталась только ветка q_not. Единственная функция, вызываемая
// из обоих мест, устраняет самую возможность разойтись.
func ApplyPredicates(f *ListFilter, preds []Predicate) {
	for _, p := range preds {
		switch {
		case p.Op == OpNeq || p.Op == OpNotContains:
			f.Not = append(f.Not, p)
		case p.Field == FieldBody:
			f.Query = p.Value
		case p.Field == FieldSeverity:
			if !slices.Contains(f.Severity, p.Value) {
				f.Severity = append(f.Severity, p.Value)
			}
		case p.Field == FieldService:
			f.Service = p.Value
		case p.Field == FieldEnvironment:
			f.Environment = p.Value
		case p.Field == FieldTraceID:
			f.TraceID = p.Value
		case p.Field == FieldAttr, p.Field == FieldResourceAttr:
			af := AttrFilter{Resource: p.Field == FieldResourceAttr, Key: p.Key, Value: p.Value}
			if !slices.Contains(f.Attrs, af) {
				f.Attrs = append(f.Attrs, af)
			}
		}
	}
	f.Not = NormalizePredicates(f.Not)
}
