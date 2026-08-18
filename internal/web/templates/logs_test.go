package templates

import (
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/log"
)

// TestNewAttrFacetsExpandedKeyOutsideTop — carry-fix из ревью задачи T5
// (задача 6, C2): раскрытый в URL ключ (?facet=<key>), найденный автокомплитом
// (web.logsAttrKeys, задача 6), может не входить в топ-N сайдбара
// (log.Query.AttrKeys с ограниченной выборкой). До фикса цикл NewAttrFacets
// искал expandedKey ТОЛЬКО среди keys — если совпадения не было, посчитанные
// для него values нигде не оказывались: клик по такому ключу из автокомплита
// визуально ничего не раскрывал. Теперь для него добавляется синтетический
// элемент списка с values.
func TestNewAttrFacetsExpandedKeyOutsideTop(t *testing.T) {
	keys := []log.FacetValue{
		{Value: "http.method", Count: 100},
		{Value: "http.status_code", Count: 80},
	}
	values := []log.FacetValue{
		{Value: "prod", Count: 7},
		{Value: "staging", Count: 3},
	}

	got := NewAttrFacets(1, LogsFilter{}, keys, "environment.tier", values)

	if len(got.Keys) != 3 {
		t.Fatalf("Keys len = %d, want 3 (2 из топа + 1 синтетический): %+v", len(got.Keys), got.Keys)
	}

	// Синтетический элемент раскрытого ключа вне топа — с values и
	// Expanded=true, иначе то же самое, что "кликнул из автокомплита —
	// ничего не раскрылось".
	item := got.Keys[0]
	if item.Key != "environment.tier" {
		t.Fatalf("первый элемент Key = %q, want %q (синтетический элемент ожидается в начале списка): %+v", item.Key, "environment.tier", got.Keys)
	}
	if !item.Expanded {
		t.Fatalf("синтетический элемент должен быть Expanded=true: %+v", item)
	}
	if len(item.Values) != 2 {
		t.Fatalf("синтетический элемент: Values len = %d, want 2: %+v", len(item.Values), item)
	}
	if item.Values[0].Value != "prod" || item.Values[0].Count != 7 {
		t.Errorf("Values[0] = %+v, want {prod 7 ...}", item.Values[0])
	}
	// Count синтетического элемента — сумма values (10), а не 0 и не
	// придуманное число: точного счётчика AttrKeys для ключа вне выборки нет.
	if item.Count != 10 {
		t.Errorf("синтетический элемент Count = %d, want 10 (сумма values)", item.Count)
	}

	// Обычные ключи из топа остаются на месте, без values (не раскрыты).
	if got.Keys[1].Key != "http.method" || got.Keys[1].Expanded {
		t.Errorf("Keys[1] = %+v, want нераскрытый http.method", got.Keys[1])
	}
	if got.Keys[2].Key != "http.status_code" || got.Keys[2].Expanded {
		t.Errorf("Keys[2] = %+v, want нераскрытый http.status_code", got.Keys[2])
	}
}

// TestNewAttrFacetsExpandedKeyInsideTop — обычный случай (не carry-fix):
// раскрытый ключ найден среди keys — синтетический элемент не добавляется,
// длина списка не меняется.
func TestNewAttrFacetsExpandedKeyInsideTop(t *testing.T) {
	keys := []log.FacetValue{
		{Value: "http.method", Count: 100},
		{Value: "http.status_code", Count: 80},
	}
	values := []log.FacetValue{{Value: "GET", Count: 5}}

	got := NewAttrFacets(1, LogsFilter{}, keys, "http.method", values)

	if len(got.Keys) != 2 {
		t.Fatalf("Keys len = %d, want 2 (без синтетического элемента): %+v", len(got.Keys), got.Keys)
	}
	if !got.Keys[0].Expanded || got.Keys[0].Key != "http.method" {
		t.Fatalf("Keys[0] должен быть раскрытым http.method: %+v", got.Keys[0])
	}
	if got.Keys[0].Count != 100 {
		t.Errorf("Count раскрытого ключа из топа не должен подменяться суммой values: got %d, want 100", got.Keys[0].Count)
	}
	if len(got.Keys[0].Values) != 1 || got.Keys[0].Values[0].Value != "GET" {
		t.Errorf("Keys[0].Values = %+v, want [{GET 5 ...}]", got.Keys[0].Values)
	}
	if got.Keys[1].Expanded {
		t.Errorf("Keys[1] не должен быть раскрыт: %+v", got.Keys[1])
	}
}

// TestNewAttrFacetsNoExpandedKey — ?facet= отсутствует: ни один элемент не
// раскрыт, синтетический элемент не добавляется.
func TestNewAttrFacetsNoExpandedKey(t *testing.T) {
	keys := []log.FacetValue{{Value: "http.method", Count: 100}}

	got := NewAttrFacets(1, LogsFilter{}, keys, "", nil)

	if len(got.Keys) != 1 {
		t.Fatalf("Keys len = %d, want 1", len(got.Keys))
	}
	if got.Keys[0].Expanded {
		t.Errorf("без ?facet= ни один элемент не должен быть раскрыт: %+v", got.Keys[0])
	}
}
