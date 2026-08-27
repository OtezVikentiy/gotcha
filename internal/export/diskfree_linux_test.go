//go:build linux

package export

import (
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// freeBytes на реальном каталоге: раньше вызов unix.Statfs и арифметика
// Bavail*Bsize не исполнялись под тестом ни разу (везде подмена через
// Worker.FreeBytes, см. worker_test.go) — самое рискованное место фичи
// (P2-OPS-4 аудита, проверка реального свободного места на ФС).
//
// Сверка идёт с ЖИВЫМ независимым вызовом unix.Statfs на том же каталоге,
// а не с диапазоном «похоже на правду»: диапазон пропускает мутацию вида
// «потерять множитель Bsize» — на файловой системе с крупным блоком
// Bavail сам по себе остаётся положительным и правдоподобным числом.
func TestFreeBytesRealDir(t *testing.T) {
	dir := t.TempDir()

	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		t.Fatalf("контрольный unix.Statfs(%q) вернул ошибку: %v", dir, err)
	}
	want := int64(st.Bavail) * int64(st.Bsize)

	free, ok, err := freeBytes(dir)
	if err != nil {
		t.Fatalf("freeBytes на существующем каталоге вернул ошибку: %v", err)
	}
	if !ok {
		t.Fatalf("freeBytes на Linux вернул ok=false — Statfs всегда поддержан")
	}
	if free != want {
		t.Fatalf("freeBytes вернул %d байт, контрольный Statfs даёт Bavail(%d)*Bsize(%d)=%d",
			free, st.Bavail, st.Bsize, want)
	}
	if free <= 0 {
		t.Fatalf("freeBytes вернул неправдоподобное число: %d байт", free)
	}
}

// freeBytes на несуществующем каталоге: Statfs обязан вернуть ошибку, а
// вызывающий (Worker.process) — НЕ получить молчаливое «места вагон».
// ok=true при ошибке — дыра: бюджет диска перестал бы проверяться там,
// где сам факт проверки не осилил дойти до диска.
func TestFreeBytesMissingDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "does-not-exist")
	free, ok, err := freeBytes(dir)
	if err == nil {
		t.Fatalf("freeBytes на несуществующем каталоге не вернул ошибку")
	}
	if ok {
		t.Fatalf("freeBytes на несуществующем каталоге вернул ok=true при ошибке %v — вызывающий примет это за «место есть»", err)
	}
	if free != 0 {
		t.Errorf("freeBytes при ошибке вернул free=%d, ожидали 0", free)
	}
}
