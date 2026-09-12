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

// Регресс на bufio.Scanner (ErrTooLong теряет хвост батча) — длиннее maxNDJSONLineBytes, не спутать
// с обычным перебором из TestParseNDJSONBodyCapped.
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

func TestParseNDJSONTimestampMissingUsesNow(t *testing.T) {
	out, err := ParseNDJSON(strings.NewReader(`{"message":"a"}`), ndjsonFallback())
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if !out[0].Timestamp.Equal(ndjsonFallback()) {
		t.Errorf("Timestamp = %v, want fallback %v", out[0].Timestamp, ndjsonFallback())
	}
}

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

// В отличие от TestParseNDJSONLongLineDroppedTailPreserved: длина в пределах maxNDJSONLineBytes,
// строка не отбрасывается целиком, а доходит до capBytes.
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

func TestParseNDJSONTraceSpanIDCapped(t *testing.T) {
	huge := strings.Repeat("f", 500)
	body := fmt.Sprintf(`{"message":"a","trace_id":%q,"span_id":%q}`, huge, huge)
	out, err := ParseNDJSON(strings.NewReader(body), ndjsonFallback())
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if len(out) != 1 {
		t.Fatalf("len(out) = %d, want 1", len(out))
	}
	if len(out[0].TraceID) != 64 {
		t.Errorf("len(TraceID) = %d, want 64", len(out[0].TraceID))
	}
	if len(out[0].SpanID) != 64 {
		t.Errorf("len(SpanID) = %d, want 64", len(out[0].SpanID))
	}
}

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

func TestParseNDJSONTimestampNullUsesNow(t *testing.T) {
	out, err := ParseNDJSON(strings.NewReader(`{"message":"a","timestamp":null}`), ndjsonFallback())
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if !out[0].Timestamp.Equal(ndjsonFallback()) {
		t.Errorf("Timestamp = %v, want fallback %v", out[0].Timestamp, ndjsonFallback())
	}
}

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

// Единственный случай, когда ParseNDJSON возвращает ошибку — не битая строка, а обрыв самого чтения.
type errReader struct{}

func (errReader) Read(p []byte) (int, error) { return 0, errors.New("boom: connection reset") }

func TestParseNDJSONUnreadableBodyReturnsError(t *testing.T) {
	_, err := ParseNDJSON(errReader{}, ndjsonFallback())
	if err == nil {
		t.Fatal("err = nil, want ошибку на полностью нечитаемом теле")
	}
}
