package export

// freeBytes сообщает объём реального свободного места на файловой системе,
// содержащей dir — платформенно-зависимая реализация (diskfree_linux.go /
// diskfree_other.go). ok=false — платформа не поддержана (см.
// diskfree_other.go): вызывающий (Worker.process, worker.go) обязан считать
// бюджет ТОЛЬКО по Config.DiskBudget, не выдумывая число, которого нет.
//
// Нужна затем, что GOTCHA_EXPORT_DISK_BUDGET_BYTES — это число из env,
// независимое от РЕАЛЬНОГО состояния диска: в поставляемом docker-compose
// pgdata/chdata/exportdata — именованные тома НА ОДНОЙ файловой системе
// хоста, и без этой проверки выгрузки законно отъедают место у Postgres/
// ClickHouse сверх собственного бюджета (P2-OPS-4 аудита).
//
// Свободная функция уровня пакета, а не метод Worker — подменяется в тестах
// через поле Worker.FreeBytes (см. его докблок в worker.go): тот же
// паттерн, что у Worker.Notify — nil-поле означает «используй боевую
// реализацию (эту функцию)».
func freeBytes(dir string) (free int64, ok bool, err error) {
	return platformFreeBytes(dir)
}
