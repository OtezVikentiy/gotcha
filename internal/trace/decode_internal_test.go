package trace

import "testing"

func TestDecodeSpanData(t *testing.T) {
	// строки как есть, числа/булевы — их текст, вложенные объекты/массивы — прочь.
	got := decodeSpanData(`{"db.system":"postgresql","code.lineno":42,"ok":true,"nested":{"x":1},"arr":[1,2]}`)
	if got["db.system"] != "postgresql" {
		t.Errorf("string value lost: %v", got)
	}
	if got["code.lineno"] != "42" {
		t.Errorf("number → text failed: %q", got["code.lineno"])
	}
	if got["ok"] != "true" {
		t.Errorf("bool → text failed: %q", got["ok"])
	}
	if _, has := got["nested"]; has {
		t.Errorf("nested object should be skipped: %v", got)
	}
	if _, has := got["arr"]; has {
		t.Errorf("array should be skipped: %v", got)
	}

	// пустое/битое/тривиальное → nil
	for _, in := range []string{"", "{}", "null", "not-json", `{"nested":{"x":1}}`} {
		if got := decodeSpanData(in); got != nil {
			t.Errorf("decodeSpanData(%q) = %v, want nil", in, got)
		}
	}
}
