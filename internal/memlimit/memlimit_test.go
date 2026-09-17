package memlimit

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// Обе формы записи «ограничения нет» — ошибиться значит выставить потолок кучи в 7 эксабайт (v1) или
// уронить старт на слове «max» (v2).
func TestParseLimit(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		want    int64
		wantErr error
	}{
		{"v2 обычный лимит", "1073741824\n", 1 << 30, nil},
		{"v2 без лимита", "max\n", 0, ErrNoLimit},
		{"v1 без лимита — огромное число", "9223372036854771712\n", 0, ErrNoLimit},
		{"v1 обычный лимит", "268435456", 1 << 28, nil},
		{"пустой файл", "\n", 0, ErrNoLimit},
		{"ноль", "0", 0, ErrNoLimit},
		{"мусор", "не число", 0, errParse},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseLimit(tc.raw)
			switch {
			case tc.wantErr == nil && err != nil:
				t.Fatalf("parseLimit(%q) = ошибка %v, want %d", tc.raw, err, tc.want)
			case errors.Is(tc.wantErr, ErrNoLimit) && !errors.Is(err, ErrNoLimit):
				t.Fatalf("parseLimit(%q) = (%d, %v), want ErrNoLimit", tc.raw, got, err)
			case tc.wantErr == errParse && err == nil:
				t.Fatalf("parseLimit(%q) = %d, want ошибку разбора", tc.raw, got)
			}
			if tc.wantErr == nil && got != tc.want {
				t.Errorf("parseLimit(%q) = %d, want %d", tc.raw, got, tc.want)
			}
		})
	}
}

// Маркер «ожидается ошибка разбора»; сравнивается по значению.
var errParse = errors.New("ожидается ошибка разбора")

// Значение, написанное оператором руками, важнее вычисленного — иначе GOMEMLIMIT из small-оверлея молча
// перестал бы работать.
func TestDecideKeepsExplicitEnv(t *testing.T) {
	_, apply, err := decide("200MiB", true, 1<<30, nil)
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if apply {
		t.Errorf("явный GOMEMLIMIT переопределён вычисленным значением")
	}
}

func TestDecideAppliesContainerLimit(t *testing.T) {
	target, apply, err := decide("", false, 1<<30, nil)
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if !apply {
		t.Fatal("потолок не выставлен при заданном лимите контейнера — рантайм продолжит " +
			"ориентироваться на память хоста и перерастёт лимит")
	}
	if target != heapTarget(1<<30) {
		t.Errorf("target = %d, want %d", target, heapTarget(1<<30))
	}
}

// Вне контейнера и в контейнере без лимита продукт не выдумывает потолок за оператора.
func TestDecideWithoutLimitDoesNothing(t *testing.T) {
	_, apply, err := decide("", false, 0, ErrNoLimit)
	if apply {
		t.Errorf("потолок выставлен при отсутствующем лимите контейнера")
	}
	if !errors.Is(err, ErrNoLimit) {
		t.Errorf("err = %v, want ErrNoLimit", err)
	}
}

// GOMEMLIMIT="" — не «оператор так решил», а пустая переменная из compose; не должна отключать
// автоопределение.
func TestDecideEmptyEnvIsNotExplicit(t *testing.T) {
	_, apply, err := decide("", true, 1<<30, nil)
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if !apply {
		t.Errorf("пустой GOMEMLIMIT принят за явное решение оператора")
	}
}

func TestSelfCgroupPath(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
		ok   bool
	}{
		{"systemd unit", "0::/system.slice/gotcha.service\n", "/system.slice/gotcha.service", true},
		{"root", "0::/\n", "/", true},
		{"v1 only", "11:memory:/docker/abc\n", "", false},
		{"empty", "", "", false},
	}
	for _, c := range cases {
		got, ok := selfCgroupPath(c.raw)
		if got != c.want || ok != c.ok {
			t.Errorf("%s: selfCgroupPath = %q,%v, want %q,%v", c.name, got, ok, c.want, c.ok)
		}
	}
}

// systemd кладёт юнит в собственный слайс — лимит корня дерева принадлежит другому cgroup и не должен
// побеждать лимит юнита.
func TestContainerLimitPrefersOwnCgroup(t *testing.T) {
	root := t.TempDir()
	unit := filepath.Join(root, "system.slice", "gotcha.service")
	if err := os.MkdirAll(unit, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFile(t, filepath.Join(unit, "memory.max"), "1073741824\n")
	writeFile(t, filepath.Join(root, "memory.max"), "4294967296\n")
	proc := filepath.Join(root, "self-cgroup")
	writeFile(t, proc, "0::/system.slice/gotcha.service\n")

	restore := swapPaths(root, proc)
	defer restore()

	got, err := containerLimit()
	if err != nil {
		t.Fatalf("containerLimit: %v", err)
	}
	if got != 1073741824 {
		t.Fatalf("containerLimit = %d, want 1073741824 (лимит юнита, не корня)", got)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func swapPaths(root, proc string) func() {
	prevRoot, prevProc := cgroupRoot, procSelfCgroupPath
	cgroupRoot, procSelfCgroupPath = root, proc
	return func() {
		cgroupRoot, procSelfCgroupPath = prevRoot, prevProc
	}
}

// Потолок кучи должен быть строго меньше лимита контейнера — равный лимиту не защищает ни от чего,
// превысить его значит быть убитым OOM-killer'ом.
func TestApplyRatioLeavesHeadroom(t *testing.T) {
	const limit = int64(1 << 30)
	target := heapTarget(limit)
	if target >= limit {
		t.Fatalf("потолок кучи %d не оставляет запаса под лимитом %d", target, limit)
	}
	if target < limit/2 {
		t.Errorf("потолок кучи %d — меньше половины лимита %d: рантайм будет собирать мусор впустую",
			target, limit)
	}
}
