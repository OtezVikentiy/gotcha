package memlimit

import (
	"errors"
	"testing"
)

// TestParseLimit закрепляет разбор файлов cgroup, включая обе формы записи
// «ограничения нет». Ошибиться здесь значит выставить потолок кучи в 7 эксабайт
// (v1) или уронить старт на слове «max» (v2).
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

// errParse — маркер «ожидается ошибка разбора»; сравнивается по значению.
var errParse = errors.New("ожидается ошибка разбора")

// TestDecideKeepsExplicitEnv: значение, написанное оператором руками, важнее
// вычисленного — иначе GOMEMLIMIT из small-оверлея молча перестал бы работать.
func TestDecideKeepsExplicitEnv(t *testing.T) {
	_, apply, err := decide("200MiB", true, 1<<30, nil)
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if apply {
		t.Errorf("явный GOMEMLIMIT переопределён вычисленным значением")
	}
}

// TestDecideAppliesContainerLimit: при лимите контейнера и пустом GOMEMLIMIT
// потолок вычисляется и ставится.
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

// TestDecideWithoutLimitDoesNothing: вне контейнера и в контейнере без лимита
// продукт не выдумывает потолок за оператора.
func TestDecideWithoutLimitDoesNothing(t *testing.T) {
	_, apply, err := decide("", false, 0, ErrNoLimit)
	if apply {
		t.Errorf("потолок выставлен при отсутствующем лимите контейнера")
	}
	if !errors.Is(err, ErrNoLimit) {
		t.Errorf("err = %v, want ErrNoLimit", err)
	}
}

// TestDecideEmptyEnvIsNotExplicit: GOMEMLIMIT="" — это не «оператор так решил»,
// а пустая переменная из compose; она не должна отключать автоопределение.
func TestDecideEmptyEnvIsNotExplicit(t *testing.T) {
	_, apply, err := decide("", true, 1<<30, nil)
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if !apply {
		t.Errorf("пустой GOMEMLIMIT принят за явное решение оператора")
	}
}

// TestApplyRatioLeavesHeadroom: потолок кучи должен быть строго меньше лимита
// контейнера. Потолок, равный лимиту, не защищает ни от чего — превысить его
// значит быть убитым OOM-killer'ом.
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
