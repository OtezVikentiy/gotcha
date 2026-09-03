// Package baseurl — единственная нормализация «базового адреса» для всех
// переменных окружения, что задают адрес удалённой стороны строкой, а не DSN:
// GOTCHA_BASE_URL, GOTCHA_TELEGRAM_API_BASE, GOTCHA_PROBE_SERVER_URL
// (cmd/gotcha/config.go) и GOTCHA_AGENT_ENDPOINT (internal/agent/config.go).
//
// До задачи 6 (E3, заморозка контракта) у этой четвёрки было четыре разных
// политики: проба не срезала хвостовой слэш («https://host/» давало
// «//probe/lease» в каждом запросе) и не отвергала query/fragment, агент
// срезал слэш, но не проверял query. Normalize — общий код для всех
// четырёх, а не четыре копии с расходящимся поведением.
//
// cmd/gotcha (package main) и internal/agent оба могут импортировать этот
// пакет — в отличие от parseBool (internal/agent/config.go), который
// продублирован вручную именно потому, что package main агенту недоступен.
package baseurl

import (
	"fmt"
	"net/url"
	"strings"
)

// Normalize проверяет raw как абсолютный http(s)-адрес без query/fragment и
// возвращает его с обрезанными хвостовыми слэшами. name — имя переменной
// окружения, для текста ошибки.
//
// Пустой raw возвращается как есть, без ошибки: обязателен ли адрес —
// решает вызывающий код (GOTCHA_BASE_URL и GOTCHA_TELEGRAM_API_BASE
// опциональны, GOTCHA_AGENT_ENDPOINT и GOTCHA_PROBE_SERVER_URL в
// --mode=probe обязательны — это уже проверка вне Normalize).
func Normalize(name, raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("%s: %w", name, err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("%s must be an absolute http(s) url, got %q", name, raw)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("%s must not carry a query or fragment, got %q", name, raw)
	}
	return strings.TrimRight(raw, "/"), nil
}
