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

// Единица потолка — независимый буфер, а не писатель: SpanWriter применяет
// один потолок к двум буферам (txBuf и spanBuf), заполняемым одновременно.

const (
	setterName     = "SetMaxBufferBytes"
	capFieldName   = "maxBufBytes"
	unitsConstName = "autoBufferCapUnits"
	wiringFile     = "cmd/gotcha/main.go"
	shareConstName = "autoBufferSafeShare"
	ratioConstName = "defaultRatio"
	ratioFile      = "internal/memlimit/memlimit.go"
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

// 0 значит «не найдена»: у констант этого сторожа нулевого значения нет.
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

func scanWriters(t *testing.T, root string, fset *token.FileSet) (writers, units int) {
	t.Helper()
	internal := filepath.Join(root, "internal")
	err := filepath.WalkDir(internal, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// пакет guards исключён: свои фикстуры обманут подсчёт
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

// Присвоение в сеттере и инициализация в конструкторе не считаются.
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

// Второй результат отличает «нашлась» от «не нашлась»: ноль — валидное значение.
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
