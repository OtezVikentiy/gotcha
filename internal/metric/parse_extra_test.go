package metric

import (
	"strconv"
	"testing"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
)

// TestAttrString проходит все скалярные представления AnyValue плюс nil и
// неподдержанный тип (bytes) — на них attrString возвращает "".
func TestAttrString(t *testing.T) {
	cases := []struct {
		name string
		v    *commonpb.AnyValue
		want string
	}{
		{"nil", nil, ""},
		{"string", &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "hi"}}, "hi"},
		{"bool_true", &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: true}}, "true"},
		{"bool_false", &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: false}}, "false"},
		{"int", &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: -42}}, "-42"},
		{"double", &commonpb.AnyValue{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: 1.5}}, "1.5"},
		{"unsupported_bytes", &commonpb.AnyValue{Value: &commonpb.AnyValue_BytesValue{BytesValue: []byte{1}}}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := attrString(c.v); got != c.want {
				t.Errorf("attrString(%s) = %q, want %q", c.name, got, c.want)
			}
		})
	}
}

// TestAttrsToMap закрывает три ветки: пустой вход → nil; пропуск пустого ключа;
// кап maxAttrKeys с детерминированным отбором первых ключей по сортировке.
func TestAttrsToMap(t *testing.T) {
	if got := attrsToMap(nil); got != nil {
		t.Errorf("attrsToMap(nil) = %v, want nil", got)
	}

	// Пустой ключ пропускается, остальные сохраняются.
	m := attrsToMap([]*commonpb.KeyValue{
		{Key: "", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "skip"}}},
		{Key: "a", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "1"}}},
		{Key: "b", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: 2}}},
	})
	if len(m) != 2 || m["a"] != "1" || m["b"] != "2" {
		t.Fatalf("attrsToMap = %v, want {a:1,b:2}", m)
	}
	if _, ok := m[""]; ok {
		t.Error("empty key must be skipped")
	}

	// Больше maxAttrKeys ключей → усечение ровно до maxAttrKeys, оставляются
	// первые по сортировке (k0000..k0063 из k0000..k0099).
	var many []*commonpb.KeyValue
	for i := 0; i < maxAttrKeys+36; i++ {
		key := "k" + strconv.FormatInt(int64(1000+i), 10) // k1000..k1099 — сортируемо
		many = append(many, &commonpb.KeyValue{Key: key,
			Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "v"}}})
	}
	capped := attrsToMap(many)
	if len(capped) != maxAttrKeys {
		t.Fatalf("capped len = %d, want %d", len(capped), maxAttrKeys)
	}
	if _, ok := capped["k1000"]; !ok {
		t.Error("k1000 (first by sort) must survive the cap")
	}
	if _, ok := capped["k1099"]; ok {
		t.Error("k1099 (last by sort) must be dropped by the cap")
	}
}

// TestTemporalityString проходит все три ветки перечисления temporality.
func TestTemporalityString(t *testing.T) {
	cases := []struct {
		in   metricspb.AggregationTemporality
		want string
	}{
		{metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE, "cumulative"},
		{metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA, "delta"},
		{metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_UNSPECIFIED, ""},
	}
	for _, c := range cases {
		if got := temporalityString(c.in); got != c.want {
			t.Errorf("temporalityString(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}
