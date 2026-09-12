package envcontract

import (
	"fmt"
	"sort"
	"strings"
)

// Явные имена вместо сентинела nil: nil (весь реестр) и пустой НЕ-nil срез
// визуально неотличимы, а такая мутация молча выключала бы fail-fast.
func CheckRenamedAll(getenv func(string) string) error {
	keys := make([]string, 0, len(Renamed))
	for k := range Renamed {
		keys = append(keys, k)
	}
	return checkRenamed(getenv, keys)
}

// Пустой old — легитимное «не проверять ничего в этой области», не сентинел
// «весь реестр»: для этого есть CheckRenamedAll.
func CheckRenamedScoped(getenv func(string) string, old []string) error {
	return checkRenamed(getenv, old)
}

// Пустое значение переменной легитимно (docker-compose прокидывает
// объявленные, но не заданные переменные пустой строкой) и старт не роняет.
func checkRenamed(getenv func(string) string, keys []string) error {
	var found []string
	for _, k := range keys {
		if getenv(k) != "" {
			found = append(found, k)
		}
	}
	return RenamedError(found)
}

// nil/пустой found — легитимное «ничего не найдено», а не ошибка.
func RenamedError(found []string) error {
	if len(found) == 0 {
		return nil
	}
	sorted := append([]string(nil), found...)
	sort.Strings(sorted)
	parts := make([]string, len(sorted))
	for i, k := range sorted {
		parts[i] = fmt.Sprintf("%s (renamed to %s)", k, Renamed[k])
	}
	return fmt.Errorf("environment variable(s) renamed, update your .env before upgrading: %s",
		strings.Join(parts, ", "))
}
