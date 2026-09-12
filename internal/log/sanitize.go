package log

import (
	"sort"
	"strconv"
	"strings"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
)

// Атрибуты ресурса, промотируемые в поля модели — остальное едет в LogRecord.ResourceAttrs как есть.
const (
	attrServiceName   = "service.name"
	attrDeployEnv     = "deployment.environment"      // старая семконвенция
	attrDeployEnvName = "deployment.environment.name" // текущая
)

// Защита от неограниченной кардинальности — первые maxAttrKeys в отсортированном порядке, детерминированно.
const maxAttrKeys = 64

// Локальная копия metric.capRunes/ingest.capRunes — разные капы для разных колонок, не переиспользуются.
// NUL вырезается отдельно: ClickHouse принимает его, PostgreSQL на text — нет.
func capRunes(s string, n int) string {
	if strings.IndexByte(s, 0) >= 0 {
		s = strings.ReplaceAll(s, "\x00", "")
	}
	// Быстрый путь без аллокации: в UTF-8 байт всегда не меньше рун.
	if len(s) <= n {
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// Вес маркера учитывается в capBytes при расчёте лимита: итоговая строка вместе с ним не больше n байт.
const truncMarker = "…(truncated)"

// В отличие от capRunes (рунный кап для коротких строк) — байтовый: тело до 64 КиБ, счёт по рунам стоил бы
// аллокаций. Режем по границе руны, чтобы не разорвать multi-byte символ.
func capBytes(s string, n int) string {
	if strings.IndexByte(s, 0) >= 0 {
		s = strings.ReplaceAll(s, "\x00", "")
	}
	if len(s) <= n {
		return s
	}
	limit := n - len(truncMarker)
	if limit < 0 {
		limit = 0
	}
	if limit > len(s) {
		limit = len(s)
	}
	for limit > 0 && !utf8RuneStart(s[limit]) {
		limit--
	}
	return s[:limit] + truncMarker
}

// Локальная копия utf8.RuneStart — не тащить лишний импорт ради одной проверки.
func utf8RuneStart(b byte) bool {
	return b&0xC0 != 0x80
}

func promote(res *resourcepb.Resource) (service, environment string) {
	for _, kv := range res.GetAttributes() {
		switch kv.GetKey() {
		case attrServiceName:
			service = attrString(kv.GetValue())
		case attrDeployEnvName:
			environment = attrString(kv.GetValue())
		case attrDeployEnv:
			if environment == "" {
				environment = attrString(kv.GetValue())
			}
		}
	}
	return capRunes(service, 200), capRunes(environment, 200)
}

// Кап ключ 64/значение 200, maxAttrKeys записей, детерминированно по отсортированным ключам — калька metric.attrsToMap.
func attrsToMap(attrs []*commonpb.KeyValue) map[string]string {
	if len(attrs) == 0 {
		return nil
	}
	m := make(map[string]string, len(attrs))
	for _, kv := range attrs {
		if kv.GetKey() == "" {
			continue
		}
		m[capRunes(kv.GetKey(), 64)] = capRunes(attrString(kv.GetValue()), 200)
	}
	if len(m) <= maxAttrKeys {
		return m
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	capped := make(map[string]string, maxAttrKeys)
	for _, k := range keys[:maxAttrKeys] {
		capped[k] = m[k]
	}
	return capped
}

// Калька metric.attrString. Структурные значения (kvlist/array) не нужны — это лейбл, не тело лога.
func attrString(v *commonpb.AnyValue) string {
	switch x := v.GetValue().(type) {
	case *commonpb.AnyValue_StringValue:
		return x.StringValue
	case *commonpb.AnyValue_BoolValue:
		if x.BoolValue {
			return "true"
		}
		return "false"
	case *commonpb.AnyValue_IntValue:
		return strconv.FormatInt(x.IntValue, 10)
	case *commonpb.AnyValue_DoubleValue:
		return strconv.FormatFloat(x.DoubleValue, 'f', -1, 64)
	}
	return ""
}
