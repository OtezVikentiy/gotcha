package envcontract

import (
	"fmt"
	"sort"
	"strings"
)

// CheckRenamed — общая реализация fail-fast отказа на устаревшем имени env,
// раньше жившая двумя побайтово идентичными копиями в cmd/gotcha/config.go
// и internal/agent/config.go (ops-review E3 T8 круг 1). `old` называет,
// какое подмножество ключей Renamed проверять:
//   - nil — весь реестр. Так вызывает cmd/gotcha: сервер и агент штатно
//     делят один `.env` на одном хосте (install.sh/hosts.go кладут
//     агентские переменные рядом с серверными), и оператор с устаревшим
//     именем — хоть серверным, хоть агентским — обязан узнать об этом при
//     рестарте ЛЮБОГО из двух процессов.
//   - непустой срез — только эти ключи. Так вызывает internal/agent
//     (envcontract.AgentOwned, три свои пары): агент не должен отказывать
//     на чужих переменных общего `.env`, которые он никогда не читает
//     (PATH, настройки соседних сервисов, серверные переменные) — это не
//     защита, а самоуправство, и расходится с задачей известных имён
//     (internal/envcontract, будущий реестр), где у каждого бинаря свой
//     набор.
//
// Пустое значение переменной — легитимно (docker-compose штатно
// прокидывает объявленные, но не заданные переменные пустой строкой) и
// старт не роняет. Сообщение перечисляет ВСЕ найденные старые имена за один
// проход (отсортированно, для устойчивого текста ошибки между запусками —
// map не даёт порядка сам по себе), а не только первое найденное.
func CheckRenamed(getenv func(string) string, old []string) error {
	keys := old
	if keys == nil {
		keys = make([]string, 0, len(Renamed))
		for k := range Renamed {
			keys = append(keys, k)
		}
	}
	var found []string
	for _, k := range keys {
		if getenv(k) != "" {
			found = append(found, k)
		}
	}
	if len(found) == 0 {
		return nil
	}
	sort.Strings(found)
	parts := make([]string, len(found))
	for i, k := range found {
		parts[i] = fmt.Sprintf("%s (renamed to %s)", k, Renamed[k])
	}
	return fmt.Errorf("environment variable(s) renamed, update your .env before upgrading: %s",
		strings.Join(parts, ", "))
}
