//go:build !linux

package export

// platformFreeBytes — честный фолбэк для не-Linux: платформа не
// поддержана, ok=false говорит вызывающему считать бюджет ТОЛЬКО по
// Config.DiskBudget, не выдумывая число. Прод — исключительно Linux
// (Dockerfile), эта ветка существует только затем, чтобы `go build`/тесты
// на macOS/Windows у разработчика не ломались.
func platformFreeBytes(dir string) (int64, bool, error) {
	return 0, false, nil
}
