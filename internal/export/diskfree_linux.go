//go:build linux

package export

import "golang.org/x/sys/unix"

// Bavail (доступно непривилегированному процессу), не Bfree — тот включает
// резерв, зарезервированный ядром для root.
func platformFreeBytes(dir string) (int64, bool, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, false, err
	}
	// Bavail/Bsize — разные целочисленные типы по архитектурам Linux;
	// приведение через int64 держит переносимость.
	return int64(st.Bavail) * int64(st.Bsize), true, nil
}
