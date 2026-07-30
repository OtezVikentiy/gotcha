package ingest

import (
	"runtime"
	"strconv"
	"testing"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
)

// manyAttrs строит список атрибутов длиной n — то, что приезжает из тела,
// в котором отправитель повторил один короткий ключ миллион раз.
func manyAttrs(n int) []*commonpb.KeyValue {
	out := make([]*commonpb.KeyValue, n)
	for i := range out {
		out[i] = &commonpb.KeyValue{
			Key:   "k" + strconv.Itoa(i),
			Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "v" + strconv.Itoa(i)}},
		}
	}
	return out
}

// TestOTLPAttrsCappedBeforeMap: число атрибутов ограничивается ДО построения
// карт, а не после.
//
// Раньше карта строилась из всех атрибутов, ключи сортировались, и только потом
// оставались первые 64: тело в 10 МиБ несёт около 1.2 млн атрибутов, и на них
// уходило порядка 100 МБ сверх тела — усиление, управляемое отправителем.
func TestOTLPAttrsCappedBeforeMap(t *testing.T) {
	const n = 300_000
	attrs := manyAttrs(n)

	if got := len(capAttrs(attrs)); got != maxSpanAttrs {
		t.Fatalf("capAttrs вернул %d атрибутов, want %d", got, maxSpanAttrs)
	}
	if got := len(capAttrs(attrs[:10])); got != 10 {
		t.Errorf("короткий список обрезан: %d, want 10", got)
	}

	tags := otlpTags(attrs)
	if len(tags) > maxSpanAttrs {
		t.Errorf("тегов %d — карта построена из всего списка", len(tags))
	}
	data := otlpAttrMap(attrs)
	if len(data) > maxDataKeys {
		t.Errorf("span.data содержит %d ключей, want не больше %d", len(data), maxDataKeys)
	}
}

// TestOTLPAttrsDoNotAmplifyMemory меряет фактическую цену разбора: работа по
// атрибутам обязана быть ограничена потолком, а не длиной списка.
//
// Проверка именно измерением, а не «мы поставили кап»: цена этого дефекта
// выражается в мегабайтах, и только мегабайты её и подтверждают.
func TestOTLPAttrsDoNotAmplifyMemory(t *testing.T) {
	const n = 1_000_000
	attrs := manyAttrs(n)

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)

	tags := otlpTags(attrs)
	data := otlpAttrMap(attrs)

	runtime.ReadMemStats(&after)
	grew := int64(after.TotalAlloc - before.TotalAlloc)

	// Потолок с запасом: 256 атрибутов — это десятки килобайт вместе со
	// строками. 8 МиБ отделяют «ограниченную работу» от «карты на миллион
	// записей» (там речь о сотне мегабайт) и не ловят шум аллокатора.
	const limit = 8 << 20
	if grew > limit {
		t.Errorf("разбор %d атрибутов выделил %d байт (> %d): работа идёт по всему списку, а не по потолку",
			n, grew, limit)
	}
	if len(tags) == 0 || len(data) == 0 {
		t.Error("ничего не разобрано — тест проверял бы пустоту")
	}
	runtime.KeepAlive(attrs)
}
