//go:build linux

package export

import "golang.org/x/sys/unix"

// platformFreeBytes — Linux-реализация freeBytes через unix.Statfs.
// Bavail (доступно НЕпривилегированному процессу), а не Bfree — Bfree
// включает запас, зарезервированный ядром для root, который процессу
// приложения (не root, см. USER gotcha в Dockerfile) всё равно недоступен.
//
// ok=false здесь никогда не возвращается: Linux — единственная платформа
// продукта в проде (см. Dockerfile), и unix.Statfs на ней есть всегда;
// ok=false — только у diskfree_other.go, для платформ разработки.
func platformFreeBytes(dir string) (int64, bool, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, false, err
	}
	// Bavail/Bsize у unix.Statfs_t на разных архитектурах Linux — разные
	// целочисленные типы (int64 на amd64/arm64, uint32/int32 на некоторых
	// 32-битных); явное приведение через int64 держит freeBytes переносимым
	// по всем целям Linux, которые собирает проект.
	return int64(st.Bavail) * int64(st.Bsize), true, nil
}
