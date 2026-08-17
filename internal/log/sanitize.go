package log

import (
	"sort"
	"strconv"
	"strings"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
)

// Атрибуты ресурса, которые промотируем в поля модели (та же семантика, что у
// spans/metric_points, см. metric/parse.go): всё остальное едет в
// LogRecord.ResourceAttrs как есть.
const (
	attrServiceName   = "service.name"
	attrDeployEnv     = "deployment.environment"      // старая семконвенция
	attrDeployEnvName = "deployment.environment.name" // текущая
)

// maxAttrKeys — кап числа атрибутов записи лога (тот же приём, что у метрик):
// защита от неограниченной кардинальности. Берём первые maxAttrKeys в
// отсортированном порядке — детерминированно.
const maxAttrKeys = 64

// capRunes вырезает NUL и обрезает s до n рун. Локальная копия
// metric.capRunes/ingest.capRunes — эти пакеты друг у друга не
// переиспользуют (несут разные капы для разных колонок), см. их докблоки.
// NUL вырезается по той же причине: ClickHouse его принимает, а PostgreSQL на
// text падает; промотированные service/environment могут долетать до PG.
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

// truncMarker — маркер усечения тела лога. Вес маркера учитывается в capBytes
// при расчёте лимита: итоговая строка вместе с ним не должна превышать n байт.
const truncMarker = "…(truncated)"

// capBytes вырезает NUL и обрезает s до n байт, добавляя truncMarker, если
// пришлось резать. В отличие от capRunes (рунный кап для коротких недоверенных
// строк типа атрибутов), тело лога может быть большим (до 64 КиБ), поэтому
// кап байтовый — счёт по рунам на такой строке сам по себе стоил бы заметных
// аллокаций. Режем по границе руны, чтобы не разорвать multi-byte символ на
// стыке. NUL вырезаем по той же причине, что в capRunes: ClickHouse его
// принимает, PostgreSQL на text — нет.
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

// utf8RuneStart — является ли байт началом руны в UTF-8 (не continuation-байт
// 10xxxxxx). Локальная копия единственной нужной функции utf8.RuneStart,
// чтобы не тащить лишний импорт ради одной проверки.
func utf8RuneStart(b byte) bool {
	return b&0xC0 != 0x80
}

// promote вытаскивает service.name и environment из ресурсных атрибутов
// (аналог metric.promote, без host.name — у LogRecord такого поля нет).
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

// attrsToMap собирает атрибуты в строковый Map (кап ключ 64/значение 200,
// maxAttrKeys записей, детерминированно по отсортированным ключам при
// переполнении) — калька metric.attrsToMap.
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

// attrString читает скалярное представление AnyValue (для лейблов/ресурса) —
// калька metric.attrString. Структурные значения (kvlist/array) тут не нужны:
// это не тело лога, а лейбл, для него достаточно скаляра.
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
