package guards

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSelfMetricsDocumented(t *testing.T) {
	tree := Load(t)
	scan := collectSelfMetrics(t, tree)
	if scan.callSites == 0 {
		t.Fatalf("blind guard: found 0 selfmetrics.Add/AddInt call-sites — the scan is looking at the wrong tree")
	}
	if len(scan.types) < 10 {
		t.Fatalf("collected only %d metrics — the scanner is broken", len(scan.types))
	}
	for _, lang := range []string{"en", "ru"} {
		doc, err := os.ReadFile(filepath.Join(tree.Root, "internal", "docs", lang, "self-monitoring.md"))
		if err != nil {
			t.Fatal(err)
		}
		for name := range scan.types {
			if !strings.Contains(string(doc), name) {
				t.Errorf("%s is registered (%s) but missing from %s/self-monitoring.md",
					name, strings.Join(scan.files[name], ", "), lang)
			}
		}
	}
}
