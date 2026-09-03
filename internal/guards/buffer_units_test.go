package guards

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Сторож на autoBufferCapUnits: константа в cmd/gotcha/main.go делит долю
// потолка кучи между буферами писателей, и её расхождение с реальным числом
// буферов тихо ломает авто-дефолт GOTCHA_MAX_WRITER_BUFFER_BYTES — каждый буфер
// получает больше, чем ему причитается, и сумма перерастает потолок ровно в
// том сценарии, ради которого потолок и заводился (долгий простой ClickHouse).
//
// Расхождение уже случалось и жило незамеченным: writer логов приехал с C1, а
// комментарии в docker-compose.small.yml и в шапке internal/memlimit
// продолжали перечислять пять буферов. Ни один гейт этого не ловил —
// поведение оставалось валидным Go и валидным YAML.
//
// Единица потолка — НЕ писатель, а независимый буфер: SpanWriter применяет
// один потолок к двум буферам (txBuf и spanBuf), которые в худшем случае
// заполнены одновременно, и потому считается за две единицы. Поэтому сторож
// считает не писателей, а места, где вес буфера сравнивается с maxBufBytes.

const (
	// setterName — метод, которым main проставляет потолок писателю.
	setterName = "SetMaxBufferBytes"
	// capFieldName — поле писателя, хранящее потолок.
	capFieldName = "maxBufBytes"
	// unitsConstName — проверяемая константа.
	unitsConstName = "autoBufferCapUnits"
	// wiringFile — файл, где писатели создаются и получают потолок.
	wiringFile = "cmd/gotcha/main.go"
	// shareConstName — доля потолка кучи, отдаваемая сумме буферов.
	shareConstName = "autoBufferSafeShare"
	// ratioConstName — доля лимита контейнера, отдаваемая куче.
	ratioConstName = "defaultRatio"
	// ratioFile — файл, где живёт ratioConstName.
	ratioFile = "internal/memlimit/memlimit.go"
)

func TestAutoBufferCapUnitsMatchesWriters(t *testing.T) {
	root, err := findRoot()
	if err != nil {
		t.Fatal(err)
	}

	fset := token.NewFileSet()
	wiring, err := parser.ParseFile(fset, filepath.Join(root, wiringFile), nil, 0)
	if err != nil {
		t.Fatalf("разбор %s: %v", wiringFile, err)
	}

	declared := intConst(wiring, unitsConstName)
	if declared == 0 {
		t.Fatalf("%s: не найдена константа %s — сторож ослеп, а не код исправился",
			wiringFile, unitsConstName)
	}

	wired := countSetterCalls(wiring)
	writers, units := scanWriters(t, root, fset)

	// Нижняя граница — только на то, что даёт обход дерева: скан, нашедший
	// меньше, сломан сам, и без этой проверки пустой результат совпал бы с
	// пустым ожиданием. wired сюда НЕ входит: снятый вызов в вайринге — это
	// регресс, а не слепота сторожа, и он обязан доехать до своего ассерта
	// ниже с внятным сообщением, а не утонуть здесь в «обход ослеп».
	if writers < 5 || units < 6 {
		t.Fatalf("обход ослеп: писателей с методом %s найдено %d, единиц потолка %d "+
			"(ожидалось не меньше 5 и 6) — сломан сам сторож, а не проверяемый код",
			setterName, writers, units)
	}

	if wired != writers {
		t.Errorf("писателей с методом %s: %d, а вызовов в %s: %d. "+
			"Писатель, объявивший метод и не подключённый в вайринге (или наоборот), "+
			"означает, что потолок до него не доезжает", setterName, writers, wiringFile, wired)
	}

	if units != declared {
		t.Errorf("%s = %d, а буферов под потолком %d.\n"+
			"Единица — независимый буфер, а не писатель: SpanWriter держит два "+
			"(txBuf и spanBuf) и считается за два.\n"+
			"Появился буфер — поднять константу в %s И выправить описание "+
			"авто-дефолта GOTCHA_MAX_WRITER_BUFFER_BYTES в internal/docs/{ru,en}/configuration.md, "+
			"иначе на каждый буфер выдаётся больше, чем есть.",
			unitsConstName, declared, units, wiringFile)
	}
}

// intConst достаёт значение целочисленной константы верхнего уровня. 0 —
// «не найдена»: у осмысленных констант этого сторожа нулевого значения нет.
func intConst(f *ast.File, name string) int {
	var out int
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, s := range gd.Specs {
			vs, ok := s.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, id := range vs.Names {
				if id.Name != name || i >= len(vs.Values) {
					continue
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.INT {
					continue
				}
				if v, err := strconv.Atoi(lit.Value); err == nil {
					out = v
				}
			}
		}
	}
	return out
}

// countSetterCalls считает вызовы x.SetMaxBufferBytes(...) — по одному на
// подключённого писателя. Комментарии и строки сюда не попадают: обход идёт
// по дереву, а не по строкам.
func countSetterCalls(f *ast.File) int {
	var n int
	ast.Inspect(f, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == setterName {
			n++
		}
		return true
	})
	return n
}

// scanWriters обходит internal/ и возвращает (число типов с методом
// SetMaxBufferBytes, число сравнений веса буфера с maxBufBytes). Второе и есть
// число единиц потолка: на каждый независимый буфер приходится своя проверка
// в цикле подрезки.
func scanWriters(t *testing.T, root string, fset *token.FileSet) (writers, units int) {
	t.Helper()
	internal := filepath.Join(root, "internal")
	err := filepath.WalkDir(internal, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Сам пакет guards исключается: его фикстуры и примеры в
			// комментариях иначе обманут правило (см. шапку tree.go).
			if path == filepath.Join(internal, "guards") {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		f, err := parser.ParseFile(fset, path, src, 0)
		if err != nil {
			return err
		}
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if ok && fd.Recv != nil && fd.Name.Name == setterName {
				writers++
			}
		}
		units += countCapComparisons(f)
		return nil
	})
	if err != nil {
		t.Fatalf("обход internal/: %v", err)
	}
	return writers, units
}

// countCapComparisons считает сравнения вида <вес> > w.maxBufBytes. Присвоение
// в сеттере и инициализация в конструкторе — не сравнения и не считаются.
func countCapComparisons(f *ast.File) int {
	var n int
	ast.Inspect(f, func(node ast.Node) bool {
		be, ok := node.(*ast.BinaryExpr)
		if !ok || (be.Op != token.GTR && be.Op != token.GEQ) {
			return true
		}
		if sel, ok := be.Y.(*ast.SelectorExpr); ok && sel.Sel.Name == capFieldName {
			n++
		}
		return true
	})
	return n
}

// Пин на доли, из которых выводится авто-дефолт. В отличие от
// autoBufferCapUnits их расхождение с описанием ничего не ломает в рантайме —
// но делает неверными сразу четыре предложения справочника на двух языках:
// потолок кучи (80% от mem_limit ≈ 819 МиБ на 1g), доля под буферы (60%),
// объём на буфер (≈82 МиБ) и сравнение с flat-константой (256 МиБ × 6 = 1.5
// ГиБ). Все четыре — следствия этих двух чисел, и правка любого из них
// оставляет прозу утверждать арифметику, которой больше нет.
//
// Сторож намеренно ничего не разбирает в самих доках: сверять прозу дороже,
// чем она того стоит, а число внутри вывода («шесть единиц → 60% → 82 МиБ»)
// нельзя оставить нетронутым, не сломав читаемость абзаца. Задача пина —
// не проверить доки, а не дать поменять долю молча.
func TestBufferShareConstantsPinned(t *testing.T) {
	root, err := findRoot()
	if err != nil {
		t.Fatal(err)
	}

	fset := token.NewFileSet()
	for _, c := range []struct {
		file string
		name string
		want float64
	}{
		{wiringFile, shareConstName, 0.6},
		{ratioFile, ratioConstName, 0.8},
	} {
		f, err := parser.ParseFile(fset, filepath.Join(root, c.file), nil, 0)
		if err != nil {
			t.Fatalf("разбор %s: %v", c.file, err)
		}
		got, ok := floatConst(f, c.name)
		if !ok {
			t.Errorf("%s: не найдена константа %s — сторож ослеп, а не код исправился",
				c.file, c.name)
			continue
		}
		if got != c.want {
			t.Errorf("%s = %g, а сторож ждёт %g (%s).\n"+
				"Доля изменилась осознанно? Тогда пройти по обоим языкам "+
				"internal/docs/{ru,en}/configuration.md: описание авто-дефолта "+
				"GOTCHA_MAX_WRITER_BUFFER_BYTES и строку GOTCHA_COMPOSE_MEM_LIMIT в таблице. "+
				"Там четыре числа — потолок кучи, доля под буферы, объём на буфер "+
				"и сумма flat-константы, — и все они следствия этих долей. "+
				"Выправив прозу, поднять значение здесь.",
				c.name, got, c.want, c.file)
		}
	}
}

// floatConst достаёт значение константы верхнего уровня с плавающей точкой.
// Второе значение — «нашлась ли»: ноль здесь осмысленное значение, отличать
// его от отсутствия обязательно.
func floatConst(f *ast.File, name string) (float64, bool) {
	var (
		out   float64
		found bool
	)
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, sp := range gd.Specs {
			vs, ok := sp.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, id := range vs.Names {
				if id.Name != name || i >= len(vs.Values) {
					continue
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok || (lit.Kind != token.FLOAT && lit.Kind != token.INT) {
					continue
				}
				if v, err := strconv.ParseFloat(lit.Value, 64); err == nil {
					out, found = v, true
				}
			}
		}
	}
	return out, found
}
