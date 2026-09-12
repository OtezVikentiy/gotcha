package ingest

import (
	"runtime"
	"strconv"
	"testing"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
)

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

	// с запасом: отделяет ограниченную работу от карты на миллион записей
	// (там счёт на сотни мегабайт) и не ловит шум аллокатора.
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
