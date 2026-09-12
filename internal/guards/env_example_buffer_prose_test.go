package guards

import (
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Пин на два числа в прозе .env.example (число единиц и доля потолка кучи
// под буферы) против autoBufferCapUnits/autoBufferSafeShare в cmd/gotcha/main.go.
func TestEnvExampleBufferProseMatchesConstants(t *testing.T) {
	root, err := findRoot()
	if err != nil {
		t.Fatal(err)
	}

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filepath.Join(root, wiringFile), nil, 0)
	if err != nil {
		t.Fatalf("разбор %s: %v", wiringFile, err)
	}

	units := intConst(f, unitsConstName)
	if units == 0 {
		t.Fatalf("%s: не найдена константа %s", wiringFile, unitsConstName)
	}
	share, ok := floatConst(f, shareConstName)
	if !ok {
		t.Fatalf("%s: не найдена константа %s", wiringFile, shareConstName)
	}

	example, err := os.ReadFile(filepath.Join(root, ".env.example"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(example)

	unitsPhrase := fmt.Sprintf("%d independent buffers", units)
	if !strings.Contains(text, unitsPhrase) {
		t.Errorf(".env.example: не нашёл %q рядом с описанием GOTCHA_MAX_WRITER_BUFFER_BYTES.\n"+
			"%s = %d (cmd/gotcha/main.go) разошлась с числом единиц в прозе .env.example — "+
			"поправить прозу под текущую константу.", unitsPhrase, unitsConstName, units)
	}

	sharePhrase := fmt.Sprintf("%d%% of the heap ceiling", int(share*100))
	if !strings.Contains(text, sharePhrase) {
		t.Errorf(".env.example: не нашёл %q рядом с описанием GOTCHA_MAX_WRITER_BUFFER_BYTES.\n"+
			"%s = %g (cmd/gotcha/main.go) разошлась с долей в прозе .env.example — "+
			"поправить прозу под текущую константу.", sharePhrase, shareConstName, share)
	}

	// 256 — flat-константа буфера в МиБ, зафиксированная прозой .env.example выше по файлу.
	flatTotalGiB := float64(units) * 256 / 1024
	flatPhrase := fmt.Sprintf("%g GiB across all %d buffers", flatTotalGiB, units)
	if !strings.Contains(text, flatPhrase) {
		t.Errorf(".env.example: не нашёл %q — сумма flat-фолбэка (256 МиБ × %s) разошлась с прозой.",
			flatPhrase, unitsConstName)
	}
}
