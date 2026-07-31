package guards

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// TestLoadSeesEveryKindOfSource — снимок обязан видеть все виды исходников,
// по которым работают правила.
//
// Существует потому, что расхождение обходов и было причиной дыры: один
// сторож сканировал *.go в своей директории, другой — рекурсивно, а шаблоны
// .templ не сканировал никто, хотя оба считали, что покрывают «весь код».
func TestLoadSeesEveryKindOfSource(t *testing.T) {
	tree := Load(t)

	if tree.Root == "" {
		t.Fatal("корень репозитория не определён")
	}
	if !strings.Contains(tree.CSS.Body, ":root") {
		t.Error("app.css не прочитан")
	}
	for name, want := range map[string]int{
		"GoFiles":      200,
		"Templates":    30,
		"MigrationsPG": 25,
		"MigrationsCH": 15,
	} {
		if got := countOf(tree, name); got < want {
			t.Errorf("%s: найдено %d, ожидалось не меньше %d — обход что-то пропускает",
				name, got, want)
		}
	}
	for _, loc := range []string{"ru", "en"} {
		if len(tree.Catalogs[loc]) < 500 {
			t.Errorf("каталог %s: %d ключей, ожидалось не меньше 500", loc, len(tree.Catalogs[loc]))
		}
	}

	// Порог в 500 ключей сам по себе не различает «plurals разобраны» и
	// «plurals не разобраны вовсе»: одних messages уже больше 500 в каждой
	// локали. Прямая проверка конкретного известного плюрального ключа —
	// единственное, что красит тест при отключении разбора plurals.
	for _, loc := range []string{"ru", "en"} {
		forms, ok := tree.Plurals[loc]["chart.bar.transactions"]
		if !ok {
			t.Errorf("каталог %s: плюральный ключ chart.bar.transactions не найден в Plurals", loc)
			continue
		}
		if forms["one"] == "" {
			t.Errorf("каталог %s: форма one у chart.bar.transactions пустая", loc)
		}
	}

	// internal/docs — реальный пакет продукта, соседствующий по имени с
	// корневым docs (markdown-документация), который в обход не входит.
	// Пропуск по имени каталога срезал бы оба разом — этот файл ловит именно
	// такую регрессию.
	if !containsPath(tree.GoFiles, "internal/docs/docs.go") {
		t.Error("internal/docs/docs.go не найден — обход путает продуктовый internal/docs с корневым docs")
	}

	// Сгенерированные файлы помечены: правила про авторский код обязаны их
	// отличать, иначе _templ.go утопит любую проверку шумом.
	var gen, hand int
	for _, f := range tree.GoFiles {
		if f.Generated {
			gen++
		} else {
			hand++
		}
	}
	if gen == 0 || hand == 0 {
		t.Errorf("пометка Generated не работает: сгенерированных %d, авторских %d", gen, hand)
	}
	for _, f := range tree.GoFiles {
		if strings.HasSuffix(f.Path, "_templ.go") && !f.Generated {
			t.Errorf("%s не помечен как сгенерированный", f.Path)
		}
	}

	// Пути относительны корню — правила печатают их в сообщениях, и
	// абсолютный путь машины разработчика в выводе бесполезен.
	for _, f := range tree.Templates {
		if filepath.IsAbs(f.Path) {
			t.Errorf("путь %s абсолютный, ожидался относительный корню", f.Path)
			break
		}
	}
}

// containsPath проверяет, есть ли среди файлов путь, точно равный want.
func containsPath(files []File, want string) bool {
	for _, f := range files {
		if f.Path == want {
			return true
		}
	}
	return false
}

// countOf достаёт длину среза Tree по имени поля — маленький разбор вместо
// рефлексии, чтобы тест на пороговые числа читался как таблица.
func countOf(tree *Tree, name string) int {
	switch name {
	case "GoFiles":
		return len(tree.GoFiles)
	case "Templates":
		return len(tree.Templates)
	case "MigrationsPG":
		return len(tree.MigrationsPG)
	case "MigrationsCH":
		return len(tree.MigrationsCH)
	default:
		panic("countOf: неизвестное поле " + name)
	}
}

// TestCheckExemptionsRatchet — механизм исключений обязан ловить три вещи:
// строку без причины, превышение потолка и устаревшее исключение.
//
// Третье — главное. Без него список не уменьшается сам: подпроект чинит
// нарушение, строка про него остаётся навсегда и продолжает прикрывать
// будущие такие же.
func TestCheckExemptionsRatchet(t *testing.T) {
	seen := map[string]bool{"alive": true}

	t.Run("строка без причины", func(t *testing.T) {
		ft := &fakeT{}
		CheckExemptions(ft, "проба", []Exemption{{Value: "alive", Finding: "№1"}}, 5, seen)
		ft.requireFailure(t, "причин")
	})
	t.Run("превышение потолка", func(t *testing.T) {
		ft := &fakeT{}
		list := []Exemption{
			{Value: "alive", Why: "причина", Finding: "№1"},
			{Value: "alive", Why: "причина", Finding: "№1"},
		}
		CheckExemptions(ft, "проба", list, 1, seen)
		ft.requireFailure(t, "потолок")
	})
	t.Run("устаревшее исключение", func(t *testing.T) {
		ft := &fakeT{}
		CheckExemptions(ft, "проба", []Exemption{{Value: "dead", Why: "причина", Finding: "№1"}}, 5, seen)
		ft.requireFailure(t, "удалить")
	})
	t.Run("здоровый список", func(t *testing.T) {
		ft := &fakeT{}
		CheckExemptions(ft, "проба", []Exemption{{Value: "alive", Why: "причина", Finding: "№1"}}, 5, seen)
		if ft.failed {
			t.Fatalf("здоровый список забракован: %v", ft.msgs)
		}
	})
}

// fakeT — минимальная подмена testingT: без неё пришлось бы верить на слово,
// что CheckExemptions действительно проваливает тест в нужных случаях,
// вместо того чтобы это проверить.
type fakeT struct {
	failed bool
	msgs   []string
}

func (f *fakeT) Helper() {}

func (f *fakeT) Errorf(format string, args ...any) {
	f.failed = true
	f.msgs = append(f.msgs, fmt.Sprintf(format, args...))
}

// requireFailure проверяет, что fakeT провалился и среди накопленных
// сообщений есть хотя бы одно, содержащее substr.
func (f *fakeT) requireFailure(t *testing.T, substr string) {
	t.Helper()
	if !f.failed {
		t.Fatalf("ожидался провал, тест прошёл молча")
	}
	for _, m := range f.msgs {
		if strings.Contains(m, substr) {
			return
		}
	}
	t.Fatalf("не найдено сообщение с подстрокой %q среди %v", substr, f.msgs)
}
