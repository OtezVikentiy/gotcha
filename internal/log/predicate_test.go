package log

import "testing"

func TestPredicateValidate(t *testing.T) {
	cases := []struct {
		name string
		p    Predicate
		ok   bool
	}{
		{"тело без вхождения", Predicate{Field: FieldBody, Op: OpNotContains, Value: "noise"}, true},
		{"тело не знает eq", Predicate{Field: FieldBody, Op: OpEq, Value: "noise"}, false},
		{"уровень исключается", Predicate{Field: FieldSeverity, Op: OpNeq, Value: SevWarn}, true},
		{"неизвестный уровень", Predicate{Field: FieldSeverity, Op: OpNeq, Value: "verbose"}, false},
		{"атрибут требует ключ", Predicate{Field: FieldAttr, Op: OpNeq, Value: "nginx"}, false},
		{"атрибут с ключом", Predicate{Field: FieldAttr, Key: "source", Op: OpNeq, Value: "nginx"}, true},
		{"пустое значение", Predicate{Field: FieldService, Op: OpNeq, Value: ""}, false},
		{"пробельное значение", Predicate{Field: FieldService, Op: OpNeq, Value: "   "}, false},
		{"неизвестное поле", Predicate{Field: "level", Op: OpNeq, Value: "x"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.p.Validate()
			if c.ok && err != nil {
				t.Fatalf("ожидалось «годен», получено %v", err)
			}
			if !c.ok && err == nil {
				t.Fatalf("ожидалась ошибка, предикат принят")
			}
		})
	}
}

func TestNormalizePredicatesDropsEmptyAndDuplicates(t *testing.T) {
	in := []Predicate{
		{Field: FieldBody, Op: OpNotContains, Value: "noise"},
		{Field: FieldBody, Op: OpNotContains, Value: "noise"}, // дубль
		{Field: FieldService, Op: OpNeq, Value: ""},           // пустое
		{Field: FieldAttr, Key: "source", Op: OpNeq, Value: "nginx"},
	}
	got := NormalizePredicates(in)
	if len(got) != 2 {
		t.Fatalf("осталось %d предикатов, ожидалось 2: %#v", len(got), got)
	}
	if got[0].Value != "noise" || got[1].Key != "source" {
		t.Fatalf("порядок первых вхождений не сохранён: %#v", got)
	}
}
