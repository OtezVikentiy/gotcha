package ingest

import (
	"strings"
	"testing"
)

func TestCapDataMapKeys(t *testing.T) {
	m := make(map[string]any, maxDataKeys+50)
	for i := 0; i < maxDataKeys+50; i++ {
		m[string(rune('a'+i%26))+"-"+strings.Repeat("k", i)] = "v"
	}
	out := capDataMap(m)
	if len(out) > maxDataKeys {
		t.Fatalf("keys = %d, want <= %d", len(out), maxDataKeys)
	}
}

func TestCapDataMapValueLen(t *testing.T) {
	longVal := strings.Repeat("x", maxDataValue+500)
	longKey := strings.Repeat("k", 200)
	out := capDataMap(map[string]any{longKey: longVal, "n": 42})

	var gotKey string
	for k := range out {
		if k != "n" {
			gotKey = k
		}
	}
	if len([]rune(gotKey)) != 64 {
		t.Fatalf("key len = %d, want 64", len([]rune(gotKey)))
	}
	if got := out[gotKey].(string); len([]rune(got)) != maxDataValue {
		t.Fatalf("value len = %d, want %d", len([]rune(got)), maxDataValue)
	}
	if out["n"] != 42 {
		t.Fatalf("numeric value = %v, want 42 preserved", out["n"])
	}
}

func TestCapDataMapEmpty(t *testing.T) {
	if capDataMap(nil) != nil {
		t.Fatal("nil map must stay nil")
	}
	out := capDataMap(map[string]any{})
	if len(out) != 0 {
		t.Fatalf("empty map len = %d, want 0", len(out))
	}
}
