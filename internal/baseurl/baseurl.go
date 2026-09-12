package baseurl

import (
	"fmt"
	"net/url"
	"strings"
)

// Пустая raw возвращается как есть без ошибки — обязательность адреса решает вызывающий код.
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
