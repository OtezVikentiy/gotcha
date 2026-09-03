package envcontract

import (
	"fmt"
	"sort"
	"strings"
)

// CheckRenamedAll и CheckRenamedScoped — общая реализация fail-fast отказа
// на устаревшем имени env, раньше жившая двумя побайтово идентичными
// копиями в cmd/gotcha/config.go и internal/agent/config.go. Две функции с
// явными именами вместо одной с сентинелом `nil` — так надёжнее: `nil`
// (весь реестр) и пустой, но НЕ-nil срез (в прежней сигнатуре — «нет
// ключей») визуально неотличимы на месте вызова, и мутация `nil` →
// `[]string{}` (обычный результат make/фильтрации/decode пустого JSON)
// молча выключала бы fail-fast целиком, не роняя ни одного теста, ни
// компилятора, ни go vet. Раздельные функции убирают вопрос: передать
// «ничего не проверять» вместо «весь реестр» больше не через один
// параметр, который делает диаметрально разные вещи в зависимости от
// длины.
//
// Обе используют один внутренний checkRenamed — логика не дублируется,
// дублируется только точка входа.

// CheckRenamedAll проверяет ВЕСЬ реестр Renamed. Так вызывает cmd/gotcha:
// сервер и агент штатно делят один `.env` на одном хосте (install.sh/
// hosts.go кладут агентские переменные рядом с серверными), и оператор с
// устаревшим именем — хоть серверным, хоть агентским — обязан узнать об
// этом при рестарте ЛЮБОГО из двух процессов.
func CheckRenamedAll(getenv func(string) string) error {
	keys := make([]string, 0, len(Renamed))
	for k := range Renamed {
		keys = append(keys, k)
	}
	return checkRenamed(getenv, keys)
}

// CheckRenamedScoped проверяет ТОЛЬКО перечисленные в `old` ключи Renamed.
// Так вызывает internal/agent (envcontract.AgentOwned, три свои пары):
// агент не должен отказывать на чужих переменных общего `.env`, которые он
// никогда не читает (PATH, настройки соседних сервисов, серверные
// переменные) — это не защита, а самоуправство, и расходится с задачей
// известных имён (internal/envcontract, будущий реестр), где у каждого
// бинаря свой набор. Пустой `old` — легитимный вызов «не проверять ничего
// в этой области» (не сентинел «весь реестр», как раньше): используй
// CheckRenamedAll, если нужен весь реестр.
func CheckRenamedScoped(getenv func(string) string, old []string) error {
	return checkRenamed(getenv, old)
}

// checkRenamed — общее тело: `keys` называет РОВНО те ключи Renamed, что
// проверяются (без специального смысла у nil/пустого среза — вызывающие
// CheckRenamedAll/CheckRenamedScoped сами решают, что в него положить).
//
// Пустое значение переменной — легитимно (docker-compose штатно
// прокидывает объявленные, но не заданные переменные пустой строкой) и
// старт не роняет — здесь, где keys уже сузили область до имён, которые
// эта проверка ЗНАЕТ по имени заранее. Форматирование сообщения не
// дублируется — RenamedError.
func checkRenamed(getenv func(string) string, keys []string) error {
	var found []string
	for _, k := range keys {
		if getenv(k) != "" {
			found = append(found, k)
		}
	}
	return RenamedError(found)
}

// RenamedError форматирует «имя (renamed to новое-имя)» для произвольного
// набора найденных старых имён — тот же текст, что CheckRenamedAll/
// CheckRenamedScoped, для вызывающих, которые находят старые имена ДРУГИМ
// способом (internal/agent.checkUnknownAgentEnvVars — сканом окружения по
// префиксу GOTCHA_AGENT_, а не перебором известного списка ключей через
// getenv) и не должны заново набирать тот же текст руками. found — уже
// отфильтрованные вызывающим имена (какие включать и с каким условием —
// решает он; здесь дублируется только форматирование, не отбор). nil/пустой
// srez — легитимный «ничего не найдено», а не ошибка.
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
