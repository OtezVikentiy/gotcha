package export

// ok=false — платформа не поддержана: вызывающий обязан считать бюджет только по Config.DiskBudget.
func freeBytes(dir string) (free int64, ok bool, err error) {
	return platformFreeBytes(dir)
}
