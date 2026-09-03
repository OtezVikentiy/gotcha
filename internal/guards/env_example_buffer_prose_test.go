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

// TestEnvExampleBufferProseMatchesConstants — класс, не покрытый ни одним
// существующим сторожем (аудит W3-G #4/#5): проза .env.example про
// GOTCHA_MAX_WRITER_BUFFER_BYTES утверждала "five writers... 256 MiB... 1.25 GiB",
// хотя дефолт авто-выводится из cgroup, а единиц потолка шесть, а не пять
// (себестоимость ошибки — оператор читает .env.example, а не
// configuration.md, и рассчитывает память хоста по неверным числам).
//
// TestAutoBufferCapUnitsMatchesWriters (buffer_units_test.go) сверяет код с
// кодом, TestBufferShareConstantsPinned пином на float-константах прямо
// отказывается читать прозу ("Сторож намеренно ничего не разбирает в самих
// доках"). Этот тест — недостающее звено: пин ровно на два числа в прозе
// .env.example (число единиц и доля потолка кучи под буферы), сверенных с
// autoBufferCapUnits/autoBufferSafeShare (cmd/gotcha/main.go, wiringFile
// определён в buffer_units_test.go). Смена любой из констант без правки
// .env.example роняет тест.
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

	// flat-фолбэк — units умножить на flat-константу пакета-писателя (256 МиБ,
	// зафиксирована прозой .env.example выше по файлу как значение
	// GOTCHA_MAX_WRITER_BUFFER_BYTES=268435456 = 256 МиБ) — сумма должна оставаться
	// написанной как явное число, а не потеряться при правке units.
	flatTotalGiB := float64(units) * 256 / 1024
	flatPhrase := fmt.Sprintf("%g GiB across all %d buffers", flatTotalGiB, units)
	if !strings.Contains(text, flatPhrase) {
		t.Errorf(".env.example: не нашёл %q — сумма flat-фолбэка (256 МиБ × %s) разошлась с прозой.",
			flatPhrase, unitsConstName)
	}
}
