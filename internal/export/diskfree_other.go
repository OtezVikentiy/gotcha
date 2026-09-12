//go:build !linux

package export

// Прод — только Linux; эта ветка существует, чтобы go build/тесты на
// macOS/Windows у разработчика не ломались.
func platformFreeBytes(dir string) (int64, bool, error) {
	return 0, false, nil
}
