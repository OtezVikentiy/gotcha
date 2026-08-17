package log

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"
)

func ndjsonFallback() time.Time { return time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC) }

// Один JSON-объект без завершающего \n — тоже валидная строка (последняя
// строка тела запроса часто без перевода строки).
func TestParseNDJSONSingleObject(t *testing.T) {
	body := `{"message":"boom","level":"error"}`
	out, err := ParseNDJSON(strings.NewReader(body), ndjsonFallback())
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if len(out) != 1 {
		t.Fatalf("len(out) = %d, want 1", len(out))
	}
	if out[0].Body != "boom" {
		t.Errorf("Body = %q, want boom", out[0].Body)
	}
	if out[0].Severity != SevError {
		t.Errorf("Severity = %q, want %q", out[0].Severity, SevError)
	}
	if !out[0].ObservedTS.Equal(ndjsonFallback()) {
		t.Errorf("ObservedTS = %v, want fallback %v", out[0].ObservedTS, ndjsonFallback())
	}
}

// NDJSON — три строки, три записи.
func TestParseNDJSONThreeLines(t *testing.T) {
	body := `{"message":"a"}
{"message":"b"}
{"message":"c"}
`
	out, err := ParseNDJSON(strings.NewReader(body), ndjsonFallback())
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if len(out) != 3 {
		t.Fatalf("len(out) = %d, want 3", len(out))
	}
	for i, want := range []string{"a", "b", "c"} {
		if out[i].Body != want {
			t.Errorf("out[%d].Body = %q, want %q", i, out[i].Body, want)
		}
	}
}

// Пустой message пропускается — не роняя остальной батч.
func TestParseNDJSONEmptyMessageSkipped(t *testing.T) {
	body := `{"message":"a"}
{"message":""}
{"message":"c"}
`
	out, err := ParseNDJSON(strings.NewReader(body), ndjsonFallback())
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if len(out) != 2 {
		t.Fatalf("len(out) = %d, want 2 (пустой message пропущен)", len(out))
	}
	if out[0].Body != "a" || out[1].Body != "c" {
		t.Errorf("out = %+v, want a, c", out)
	}
}

// Строка не-JSON пропускается, остальной батч разбирается.
func TestParseNDJSONInvalidLineSkipped(t *testing.T) {
	body := `{"message":"a"}
not-json-at-all
{"message":"c"}
`
	out, err := ParseNDJSON(strings.NewReader(body), ndjsonFallback())
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if len(out) != 2 {
		t.Fatalf("len(out) = %d, want 2 (битая строка пропущена)", len(out))
	}
	if out[0].Body != "a" || out[1].Body != "c" {
		t.Errorf("out = %+v, want a, c", out)
	}
}

// Регресс-тест на главный риск задачи: bufio.Scanner на строке длиннее
// буфера теряет весь хвост. Строка длиннее maxNDJSONLineBytes (заведомо
// патологическая, на порядки больше капа тела — не спутать с обычным
// небольшим перебором из TestParseNDJSONBodyCapped) должна быть отброшена
// целиком, а СЛЕДУЮЩИЕ строки — по-прежнему разобраны.
func TestParseNDJSONLongLineDroppedTailPreserved(t *testing.T) {
	longMsg := strings.Repeat("x", maxNDJSONLineBytes+1000)
	longLine := `{"message":"` + longMsg + `"}`
	body := `{"message":"before"}
` + longLine + `
{"message":"after"}
`
	out, err := ParseNDJSON(strings.NewReader(body), ndjsonFallback())
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if len(out) != 2 {
		t.Fatalf("len(out) = %d, want 2 (длинная строка отброшена, хвост не потерян)", len(out))
	}
	if out[0].Body != "before" {
		t.Errorf("out[0].Body = %q, want before", out[0].Body)
	}
	if out[1].Body != "after" {
		t.Errorf("out[1].Body = %q, want after (хвост батча потерян)", out[1].Body)
	}
}

// timestamp строкой RFC3339 распознаётся.
func TestParseNDJSONTimestampRFC3339String(t *testing.T) {
	ts := ndjsonFallback().Add(-time.Hour)
	body := fmt.Sprintf(`{"message":"a","timestamp":%q}`, ts.Format(time.RFC3339))
	out, err := ParseNDJSON(strings.NewReader(body), ndjsonFallback())
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if !out[0].Timestamp.Equal(ts) {
		t.Errorf("Timestamp = %v, want %v", out[0].Timestamp, ts)
	}
}

// timestamp числом — unix-float секунды.
func TestParseNDJSONTimestampUnixFloat(t *testing.T) {
	ts := ndjsonFallback().Add(-2 * time.Hour)
	sec := float64(ts.UnixNano()) / float64(time.Second)
	body := fmt.Sprintf(`{"message":"a","timestamp":%s}`, strconv.FormatFloat(sec, 'f', 6, 64))
	out, err := ParseNDJSON(strings.NewReader(body), ndjsonFallback())
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if d := out[0].Timestamp.Sub(ts); d > time.Millisecond || d < -time.Millisecond {
		t.Errorf("Timestamp = %v, want ~%v (dt=%v)", out[0].Timestamp, ts, d)
	}
}

// timestamp отсутствует → now (fallback).
func TestParseNDJSONTimestampMissingUsesNow(t *testing.T) {
	out, err := ParseNDJSON(strings.NewReader(`{"message":"a"}`), ndjsonFallback())
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if !out[0].Timestamp.Equal(ndjsonFallback()) {
		t.Errorf("Timestamp = %v, want fallback %v", out[0].Timestamp, ndjsonFallback())
	}
}

// trace_id/span_id/attributes опциональны — их отсутствие не ломает разбор.
func TestParseNDJSONOptionalFieldsAbsent(t *testing.T) {
	out, err := ParseNDJSON(strings.NewReader(`{"message":"a"}`), ndjsonFallback())
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if out[0].TraceID != "" || out[0].SpanID != "" {
		t.Errorf("TraceID/SpanID = %q/%q, want пустые", out[0].TraceID, out[0].SpanID)
	}
	if out[0].LogAttributes != nil {
		t.Errorf("LogAttributes = %+v, want nil", out[0].LogAttributes)
	}
}

// trace_id/span_id/attributes при наличии разбираются как есть.
func TestParseNDJSONOptionalFieldsPresent(t *testing.T) {
	body := `{"message":"a","trace_id":"0102030405060708090a0b0c0d0e0f10","span_id":"aabbccddeeff0011","attributes":{"k":"v"}}`
	out, err := ParseNDJSON(strings.NewReader(body), ndjsonFallback())
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if out[0].TraceID != "0102030405060708090a0b0c0d0e0f10" {
		t.Errorf("TraceID = %q", out[0].TraceID)
	}
	if out[0].SpanID != "aabbccddeeff0011" {
		t.Errorf("SpanID = %q", out[0].SpanID)
	}
	if out[0].LogAttributes["k"] != "v" {
		t.Errorf("LogAttributes[k] = %q, want v", out[0].LogAttributes["k"])
	}
}

// Потолок числа записей на запрос — стоп при достижении maxLogsPerRequest.
func TestParseNDJSONMaxPerRequestStop(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < maxLogsPerRequest+50; i++ {
		sb.WriteString(`{"message":"x"}` + "\n")
	}
	out, err := ParseNDJSON(strings.NewReader(sb.String()), ndjsonFallback())
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if len(out) != maxLogsPerRequest {
		t.Errorf("len(out) = %d, want %d", len(out), maxLogsPerRequest)
	}
}

// Тело сообщения >64КиБ обрезается с маркером усечения. Небольшой перебор
// (в отличие от TestParseNDJSONLongLineDroppedTailPreserved) — строка
// остаётся в пределах maxNDJSONLineBytes и не отбрасывается целиком, доходит
// до capBytes.
func TestParseNDJSONBodyCapped(t *testing.T) {
	huge := strings.Repeat("a", maxBodyBytes+1000)
	body := `{"message":"` + huge + `"}`
	out, err := ParseNDJSON(strings.NewReader(body), ndjsonFallback())
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if len(out) != 1 {
		t.Fatalf("len(out) = %d, want 1", len(out))
	}
	if len(out[0].Body) > maxBodyBytes {
		t.Fatalf("len(Body) = %d, want <= %d", len(out[0].Body), maxBodyBytes)
	}
	if !strings.HasSuffix(out[0].Body, "…(truncated)") {
		t.Errorf("Body не содержит маркер усечения")
	}
}

// Окно таймстемпов: значение старше now-90d подтягивается к нижней границе.
func TestParseNDJSONTimestampWindowLowerBound(t *testing.T) {
	tooOld := ndjsonFallback().Add(-100 * 24 * time.Hour)
	body := fmt.Sprintf(`{"message":"a","timestamp":%q}`, tooOld.Format(time.RFC3339))
	out, err := ParseNDJSON(strings.NewReader(body), ndjsonFallback())
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	wantLo := ndjsonFallback().Add(-90 * 24 * time.Hour)
	if !out[0].Timestamp.Equal(wantLo) {
		t.Errorf("Timestamp = %v, want нижняя граница %v", out[0].Timestamp, wantLo)
	}
}

// Окно таймстемпов: значение новее now+24h подтягивается к верхней границе.
func TestParseNDJSONTimestampWindowUpperBound(t *testing.T) {
	tooNew := ndjsonFallback().Add(48 * time.Hour)
	body := fmt.Sprintf(`{"message":"a","timestamp":%q}`, tooNew.Format(time.RFC3339))
	out, err := ParseNDJSON(strings.NewReader(body), ndjsonFallback())
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	wantHi := ndjsonFallback().Add(24 * time.Hour)
	if !out[0].Timestamp.Equal(wantHi) {
		t.Errorf("Timestamp = %v, want верхняя граница %v", out[0].Timestamp, wantHi)
	}
}

// severity/severity_text выводятся из level, severity_number всегда 0
// (у NDJSON нет числового кода, в отличие от OTLP).
func TestParseNDJSONSeverityFromLevel(t *testing.T) {
	out, err := ParseNDJSON(strings.NewReader(`{"message":"a","level":"WARNING"}`), ndjsonFallback())
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if out[0].Severity != SevWarn {
		t.Errorf("Severity = %q, want %q", out[0].Severity, SevWarn)
	}
	if out[0].SeverityText != "WARNING" {
		t.Errorf("SeverityText = %q, want WARNING", out[0].SeverityText)
	}
	if out[0].SeverityNumber != 0 {
		t.Errorf("SeverityNumber = %d, want 0", out[0].SeverityNumber)
	}
}

// Атрибуты сверх maxAttrKeys каппятся тем же приёмом, что attrsToMap.
func TestParseNDJSONAttributesCapped(t *testing.T) {
	attrs := make([]string, 0, maxAttrKeys+10)
	for i := 0; i < maxAttrKeys+10; i++ {
		attrs = append(attrs, fmt.Sprintf(`"attr-%03d":"v"`, i))
	}
	body := `{"message":"a","attributes":{` + strings.Join(attrs, ",") + `}}`
	out, err := ParseNDJSON(strings.NewReader(body), ndjsonFallback())
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if len(out[0].LogAttributes) != maxAttrKeys {
		t.Errorf("len(LogAttributes) = %d, want %d", len(out[0].LogAttributes), maxAttrKeys)
	}
}

// Значение атрибута длиннее 200 рун каппится.
func TestParseNDJSONAttributeValueCapped(t *testing.T) {
	long := strings.Repeat("x", 250)
	body := `{"message":"a","attributes":{"k":"` + long + `"}}`
	out, err := ParseNDJSON(strings.NewReader(body), ndjsonFallback())
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if got := out[0].LogAttributes["k"]; len(got) != 200 {
		t.Errorf("len(LogAttributes[k]) = %d, want 200", len(got))
	}
}

// timestamp явным null (не только отсутствующее поле) → now.
func TestParseNDJSONTimestampNullUsesNow(t *testing.T) {
	out, err := ParseNDJSON(strings.NewReader(`{"message":"a","timestamp":null}`), ndjsonFallback())
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if !out[0].Timestamp.Equal(ndjsonFallback()) {
		t.Errorf("Timestamp = %v, want fallback %v", out[0].Timestamp, ndjsonFallback())
	}
}

// Нестроковые значения атрибутов (число/bool — обычное дело у JSON-логеров)
// раньше роняли json.Unmarshal всей строки типовой ошибкой (Attributes был
// map[string]string) и теряли валидный message вместе с ней. Атрибут-null
// пропускается как ключ, а не превращается в строку "null".
func TestParseNDJSONAttributeNonStringValuesDoNotDropRecord(t *testing.T) {
	body := `{"message":"ok","attributes":{"n":3,"b":true,"s":"x","z":null}}`
	out, err := ParseNDJSON(strings.NewReader(body), ndjsonFallback())
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if len(out) != 1 {
		t.Fatalf("len(out) = %d, want 1 (message не должен теряться из-за нестрокового атрибута)", len(out))
	}
	if out[0].Body != "ok" {
		t.Errorf("Body = %q, want ok", out[0].Body)
	}
	want := map[string]string{"n": "3", "b": "true", "s": "x"}
	for k, v := range want {
		if got := out[0].LogAttributes[k]; got != v {
			t.Errorf("LogAttributes[%q] = %q, want %q", k, got, v)
		}
	}
	if _, ok := out[0].LogAttributes["z"]; ok {
		t.Errorf("LogAttributes[z] присутствует, want отсутствие ключа (null-атрибут пропущен)")
	}
}

// Вложенный объект/массив в значении атрибута сериализуется в JSON-строку —
// та же семантика, что anyValueToString для тела OTLP-лога с kvlist/array.
func TestParseNDJSONAttributeStructuredValueMarshaled(t *testing.T) {
	body := `{"message":"a","attributes":{"ctx":{"k":"v"}}}`
	out, err := ParseNDJSON(strings.NewReader(body), ndjsonFallback())
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if got := out[0].LogAttributes["ctx"]; got != `{"k":"v"}` {
		t.Errorf("LogAttributes[ctx] = %q, want {\"k\":\"v\"}", got)
	}
}

// errReader — недоверенное тело, которое всегда падает на чтении (эмуляция
// оборванного соединения). Единственный случай, когда ParseNDJSON обязан
// вернуть ошибку — битая строка внутри тела на неё не похожа.
type errReader struct{}

func (errReader) Read(p []byte) (int, error) { return 0, errors.New("boom: connection reset") }

func TestParseNDJSONUnreadableBodyReturnsError(t *testing.T) {
	_, err := ParseNDJSON(errReader{}, ndjsonFallback())
	if err == nil {
		t.Fatal("err = nil, want ошибку на полностью нечитаемом теле")
	}
}
