//go:build linux

package export

import (
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

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
